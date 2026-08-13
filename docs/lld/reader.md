# simdcsv reader — low-level design

Function-by-function design of the reader in `simdcsv.go`. All line numbers
refer to the `main` branch as of the 2026-08-13 docs session — the docs
branch carries no Go changes, so `simdcsv.go` here is identical to `main` at
that point; verify line numbers against the tree before relying on them.
Every behavioral claim is either derived from the code or probe-verified
against `encoding/csv` (Go 1.26.5, amd64, 2026-08-13); probe-backed claims
say so.

The reader is a stateful byte-offset walker over a fully-buffered input.
State (`Reader` struct, simdcsv.go:77-113) and its lifecycle are in
`docs/architecture.md` §2; this document is the per-function detail.

---

## `NewReader(r io.Reader) *Reader` — simdcsv.go:120

- Sets `Comma = ','` and stores `src`. Reads nothing; the zero value of
  `Reader` is not usable.
- Contract: the whole input is read into memory on the first `Read` — the
  trade that makes one-vector-scan-per-record possible. Streams too large to
  hold are `encoding/csv`'s job.

## `Read() (Record, error)` — simdcsv.go:129

Record state machine (also in `docs/architecture.md` §3):

```
buf == nil:
    b, err := io.ReadAll(src); err != nil → (Record{}, err), buf stays nil
    buf = b
loop:
    pos >= len(buf)        → (Record{}, io.EOF)
    line, next, quoted = nextLine(); line++
    len(line) == 0         → pos = next; repeat        (blank line, skip)
    quoted                → slowRecord(); if err → (Record{}, err);
                            return rec, checkCount(len(rec.fields))
    pos = next
    return Record{own(split(line))}, checkCount(len(fields))
```

- **Line numbering.** `line++` runs per `nextLine` call, including skipped
  blank lines, so count-error messages number physical lines. Probe-verified
  to agree with `encoding/csv` (`a,b\n\nc\n` → both report line 3).
- **Errors carry the record.** The count error is returned *together with*
  the record on both paths (the `Record{}, err` branch for `slowRecord` is
  unreachable today: `slowRecord` never errors). Probe-verified.
- **Position on error.** `pos` has advanced past the failing record before
  `checkCount` runs; the next `Read` continues at the following record.
  Probe-verified; same shape as `encoding/csv`.
- **EOF is terminal.** After the final record, `pos >= len(buf)` → `io.EOF`
  on every subsequent call.

## `nextLine() (line []byte, next int, quoted bool)` — simdcsv.go:172

One physical line, quote check, newline handling:

1. `end := simd.IndexByte(rest, '\n')`; no newline → `end = len(rest)`,
   `next = len(buf)` (truncated final record is legal); else `next = pos +
   end + 1`.
2. `line = rest[:end]`; strip one trailing `'\r'` — it belongs to the line
   ending, not the last field (CRLF). A `\r` anywhere else is data.
3. `quoted = simd.IndexByte(line, '"') >= 0`. Any quote in the record routes
   the whole record to the slow path — including a quote inside an
   otherwise-unquoted field (`a"b,c` goes slow, and parses as
   `["a\"b", "c"]`).

Invariant: `next` is always the offset just past the line's `\n`, or
`len(buf)` at EOF. The trailing-`\r` strip happens *before* the quote check,
so a record ending `"\r` loses its `\r` and then still parses as an unclosed
quote — consistent with the §7 malformed table of `docs/architecture.md`.

## `split(line []byte) [][]byte` — simdcsv.go:197

Fast-path field cut, one vector scan per record:

1. Grow `r.idx` to `len(line)+1` int32s if needed (once per file, sized to
   the longest line).
2. `n := simd.IndexAll(idx, line, r.Comma)` — positions of every delimiter
   written to `idx`, returns the count.
3. Fields are `line[start:p]` between consecutive positions, then
   `line[start:]` for the tail. `r.fields` (the reused outer slice) is
   rebuilt with `[:0]` + append.

- **Zero copies.** Every field is a subslice of `buf`.
- **Allocation (fast path):** 0 per record under `ReuseRecord=true`; 1
  (outer `[][]byte` copy) per record under `ReuseRecord=false` — the `own`
  copy, architecture.md §9.
- **Limit:** `int32` positions bound a single record to < 2^31 bytes
  (architecture.md §8).
- **Correctness coupling:** the `[len(line)+1]` sizing guarantees room for
  every delimiter position `IndexAll` can write (at most `len(line)-1`, for
  a line of all delimiters) with headroom to spare. The final empty field of
  a trailing-delimiter record is `line[len(line):]`, produced by the tail
  append — never written into `idx`.

## `recordEnd(b []byte) int` — simdcsv.go:228

Quoted-record extent by per-line quote parity (state machine in
architecture.md §4.1):

```
off = 0; inQuotes = false
loop:
    nl = IndexByte(b[off:], '\n')
    seg = b[off:] or b[off:off+nl]
    CountByte(seg, '"') % 2 == 1 → inQuotes = !inQuotes
    nl < 0        → return len(b)          (truncated: rest of buffer is the record)
    !inQuotes     → return off + nl + 1    (line ending is a record boundary)
    inQuotes      → off += nl + 1          (line ending is data)
```

- **Why parity suffices.** A doubled quote contributes two; whether it is
  inside or outside a field is irrelevant to whether the *line ending* after
  it is inside quotes. No `""` special case.
- **Complexity:** O(record) with one `IndexByte` + one `CountByte` per
  physical line — the fix for the quadratic whole-buffer `IndexAll` design
  (17 s vs 3.6 ms on 20,000 rows; wrong.md entry 5).
- **Never errors**, including unclosed quotes at EOF (returns `len(b)`).

## `quotedRecord(rec []byte) [][]byte` — simdcsv.go:260

Field-by-field parse of a record known to contain a quote (state machine in
architecture.md §4.2). Two field forms:

- **Quoted field** (`rec[i] == '"'`): skip the opener; scan with
  `IndexByte(rec[i:], '"')`. Doubled quote → unescape: on the *first* one,
  `buf = append([]byte(nil), rec[start:i]...)` (fresh allocation); append
  `'"'`; `i += 2`; continue. Any other quote closes the field: simple case →
  `rec[start:i]` subslice; unescaped case → append `rec[start:i]` into `buf`.
  Then `i++` consumes the closer.
- **Unquoted field:** `IndexByte(rec[i:], Comma)`; field is `rec[start:i]`; a
  `"` inside is data.
- **Delimiter handling after a field:** `rec[i] == Comma` → consume; if that
  was the last byte, append the final empty field. Otherwise **the stray
  skip**: any other byte is consumed by `i++` and produces nothing — this is
  the `"a"b,c` → `["a", "", "c"]` behavior (probe-verified, instrumented
  trace).

- **Never errors.** Unclosed quote → field runs to `len(rec)`.
- **Allocation:** none for simple fields; one fresh buffer per field with a
  doubled quote. The `r.unq` scratch is reset and never used (vestigial,
  architecture.md §14), so these buffers are never reused across records.

## `slowRecord() (Record, error)` — simdcsv.go:328

```
rest = buf[pos:]; end = recordEnd(rest); rec = rest[:end]
strip trailing '\n' and '\r' from rec        (recordEnd includes the ending)
pos += end
return Record{own(quotedRecord(rec))}, nil
```

- **Never errors**; the `error` in the signature is vestigial (architecture.md
  §11 — the `Read` branch that handles it is unreachable).
- **Line ending strip is on *both* characters** so `\r\n` is removed; a `\r`
  inside a quoted field survives as data. That survival is the mechanism of
  the well-formed divergence in architecture.md §12 divergence 6: simdcsv
  preserves `\r\n` inside quoted fields, `encoding/csv` normalizes it to
  `\n`. Policy decision is plan Task 0/Stage 0.

## `own(fields [][]byte) [][]byte` — simdcsv.go:347

The ownership switch:

- `ReuseRecord == true` → return `fields` as-is; the caller's record *is*
  `r.fields`, repointed by the next record.
- `ReuseRecord == false` → `make([][]byte, len(fields))`; for each field,
  `aliasesBuf(buf, f)` → keep the subslice (input is immutable and outlives
  the reader); otherwise → fresh copy. So an unescaped (doubled-quote) field
  is copied a second time here — the allocation quirk in architecture.md §9.

## `aliasesBuf(buf, f []byte) bool` — simdcsv.go:366

Pointer-range test: `&f[0]` inside `[&buf[0], &buf[0]+len(buf))`. Empty
slices never alias. `unsafe` is the only way to ask, and asking is the point:
it is what lets `own` leave input-backed fields shared while copying
scratch-backed ones. Same pattern as the test helper `aliases`
(simdcsv_test.go:194).

## `checkCount(n int) error` — simdcsv.go:375

```
FieldsPerRecord > 0  → n != FieldsPerRecord → error
FieldsPerRecord == 0 → first record sets num; later n != num → error
FieldsPerRecord < 0  → never
```

Error: `simdcsv: record on line %d: wrong number of fields: got %d, want %d`.
Plain `error`; no `*csv.ParseError`/`ErrFieldCount` (stdlib's message is
"record on line %d: wrong number of fields", wrapped in `ParseError`).
Called after `pos` advanced (§`Read`).

## `ReadAll() ([]Record, error)` — simdcsv.go:394

Loops `Read`; `io.EOF` → `(out, nil)`; any other error → `(out, err)` — the
offending record is **discarded** (stdlib `ReadAll` returns `(nil, err)`;
Go 1.26.5 `csv/reader.go`). Under `ReuseRecord`, all entries alias the last
record (architecture.md §10).

## `Record` — simdcsv.go:408

`struct{ fields [][]byte }` — unexported backing, exported accessors:

- `Len() int` — `len(r.fields)`; zero for the zero `Record`.
- `Field(i int) []byte` — `r.fields[i]`, index-panics out of range; borrows
  (valid while the input buffer lives; under `ReuseRecord` the outer slice is
  repointed by the next `Read`).
- `Fields() [][]byte` — the internal slice, no copy; mutating an element
  mutates what the reader would return next under `ReuseRecord`.
- `Strings() []string` — `make([]string, len)` + one `string(f)` per field:
  one allocation per record, one copy per field. This is what `encoding/csv`
  returns natively.

---

## Cross-cutting invariants

1. `buf` is immutable after `io.ReadAll`; all aliasing decisions rely on it.
2. `pos` only moves forward; `next` returned by `nextLine` is consistent with
   the `pos += end` of `slowRecord`; the two never disagree on a boundary.
3. `r.fields` is the only owner of the current record's outer slice, and is
   rebuilt with `[:0]` — never reallocated while in use by a caller's record
   under `ReuseRecord`.
4. `idx` is sized `len(line)+1` before every `IndexAll`, so position writes
   are always in bounds.
5. No path reads past `len(buf)`; truncated records end exactly at
   `len(buf)` by construction (`nextLine`, `recordEnd`).
6. No path errors on input content; the error surface is exactly
   `io.ReadAll` failure, `io.EOF`, and `checkCount` (architecture.md §11).
