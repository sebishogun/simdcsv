# simdcsv architecture

The design of the reader as it is implemented in `simdcsv.go`. Every
behavioral claim is backed by the source, the tests, or an executed probe
against `encoding/csv` (Go 1.26.5, amd64, 2026-08-13) that ran both parsers on
the same bytes. Where the source comments disagree with the code, §13 lists
the defect; the code wins.

## 1. Model: whole buffer, two paths

The package's premise: `encoding/csv` walks input one byte at a time with a
dependent branch per byte. This package finds every delimiter at once —
`simd.IndexAll` scans a buffer with a vector compare and writes match
positions into an `int32` index — so a quote-free record splits into fields in
one vector pass, and the fields come out as subslices of the input rather
than copies.

That works only while the input is unambiguous. A `"` changes what the bytes
after it mean (a comma inside quotes is data; so is a newline), so a record
**containing any quote anywhere** is parsed by the careful path instead. The
split is per record, so a file with a few quoted fields still runs mostly on
the fast path, and a file where every field is quoted runs entirely on the
slow one — which is the measured 0.81x against `encoding/csv` (see
`docs/wrong.md`).

Two consequences shape everything else:

- The whole input is read up front (`io.ReadAll` on the first `Read`) and
  retained. There is no streaming and no bounded memory; `NewReader`'s
  comment says so explicitly.
- The input is immutable once read. Fields are subslices, so correctness is a
  matter of slice headers and lifetimes, never of bytes changing underneath a
  caller.

## 2. Reader lifecycle and state

`NewReader(r)` stores `src` and `Comma = ','`; it reads nothing. State:

| field | meaning | set when |
|---|---|---|
| `src` | the input | `NewReader` |
| `buf` | the whole input; nil until first `Read` | first `Read` (`io.ReadAll`) |
| `pos` | offset where the next record starts | advanced by `Read`/`slowRecord` |
| `idx` | `int32` delimiter-position scratch | `split`, grown to longest line |
| `qidx`, `nidx` | quote/newline position scratch | **never** — vestigial, §14 |
| `fields` | reused outer `[][]byte` for the current record | every record parse |
| `unq` | unescape scratch | reset per quoted record, **never read** — vestigial, §14 |
| `num` | field count learned from the first record | first record under `FieldsPerRecord == 0` |
| `line` | physical line counter, for error messages | every `nextLine` call, including skipped blanks |

A reader whose `io.ReadAll` failed on the first `Read` returns that error and
keeps `buf == nil`; the next `Read` retries the read from `src`, which may
have been partially consumed (verified by probe).

## 3. Record read state machine (fast path)

`Read` (simdcsv.go:129):

```
buf == nil ? io.ReadAll → error or buf
loop:
  pos >= len(buf)        → io.EOF
  line, next, quoted = nextLine()
  line++                 (physical line, blanks included)
  len(line) == 0         → blank: pos = next; repeat    (blank lines are skipped)
  quoted                → slowRecord() → checkCount → Record
  else                  → pos = next; fields = split(line) → checkCount → Record
```

`nextLine` (simdcsv.go:172): one `simd.IndexByte(line, '\n')`; no newline
means the rest of the buffer is the record (`next = len(buf)`, so a truncated
final record is legal). A trailing `\r` is stripped — it belongs to the line
ending, not the last field. Then one `simd.IndexByte(line, '"')` decides the
path: any quote in the whole record routes it to the slow path. There is no
special case for `""` at this stage; parity handles it later (§4.1).

`split` (simdcsv.go:197): `simd.IndexAll` finds every `Comma` in one pass;
fields are the subslices between positions. `idx` is reused across records
(one allocation for the whole file, sized to the longest line), and so is the
outer `fields` slice.

`checkCount` (simdcsv.go:375): `FieldsPerRecord > 0` requires exactly that
count; `== 0` learns `num` from the first record and requires it after;
`< 0` never checks. The error is `simdcsv: record on line %d: wrong number of
fields: got %d, want %d` — a plain `error`, not a `*csv.ParseError`.

## 4. Quoted-record state machines

### 4.1 `recordEnd` — record extent by quote parity (simdcsv.go:228)

A newline ends a record only when it is outside quotes, so the extent has to
be established before fields are parsed. The machine walks one physical line
at a time:

```
off = 0; inQuotes = false
loop:
  seg = b[off:] up to the next '\n' (or the end)
  CountByte(seg, '"') odd   → inQuotes = !inQuotes
  no '\n'                   → return len(b)              (truncated record)
  !inQuotes                 → return off + nl + 1        (record boundary)
  else                      → off += nl + 1; repeat       (newline is data)
```

A doubled quote contributes two to the count, so **parity alone** decides
whether a line ending is inside a quoted field — no `""` special case is
needed. Note this is `% 2` on the raw byte count, so `"a""b` (an odd number
of quotes) toggles state exactly like any other odd count.

Scanning line at a time is deliberate: the first version ran `IndexAll` over
the entire remaining buffer once per record, which is quadratic — 20,000 rows
took 17 s against `encoding/csv`'s 3.6 ms (`docs/wrong.md` entry 5).

### 4.2 `quotedRecord` — field-by-field with unescape (simdcsv.go:260)

The machine walks the record once, alternating between two field forms:

```
i = 0
loop:
  rec[i] == '"'  → quoted field:
       skip the opening quote; start = i
       scan with IndexByte(rec[i:], '"'):
         no closing quote       → i = len(rec)                    (unclosed → rest of record)
         doubled (rec[i+1]=='"')→ unescape: append rec[start:i] + '"' into a fresh buf;
                                   i += 2; start = i; continue
         else                   → closing quote; break
       field = rec[start:i] if simple (a subslice), else the unescape buf
       i++                      (consume the closing quote)
  else         → unquoted field:
       scan with IndexByte(rec[i:], Comma)
       field = rec[start:i]     (a subslice; a '"' inside is data, ignored)
  rec[i] == Comma → i++; if i == len(rec): append empty field; break
  else            → i++          (the "stray skip" — see below)
```

Two behaviors matter for the malformed-input contract (§7):

- **The stray skip.** A character between a closing quote and the next comma
  is consumed by the final `i++` without producing a field. So `"a"b,c`
  parses as `["a", "", "c"]` — the `b` is dropped and an empty field appears
  in its place (verified by probe and by an instrumented trace of this exact
  function).
- **Quotes inside unquoted fields are data.** `a"b,c` parses as
  `["a\"b", "c"]`; the record is on the slow path (it contains a quote) but
  the field is scanned as unquoted.

An unescaped (doubled-quote) field is always a **fresh per-field allocation**,
never a subslice: `""` collapses to `"`, so the result is shorter than the
bytes it came from and cannot alias the input. The `r.unq` scratch is reset
per record but never actually used (§14), so the fresh buffers are never
reused.

### 4.3 `slowRecord` (simdcsv.go:328)

The glue: `recordEnd` fixes the extent of the record at `pos`, the line
ending (`\n`, `\r\n`) is stripped, `pos` advances, and `quotedRecord` parses.
`recordEnd` and `quotedRecord` never return errors; the only error a quoted
record can produce is the field-count check in `Read`.

## 5. Delimiter, quote and unescape paths

- **Delimiter.** `Comma` is a byte and is used only in `split`
  (`IndexAll(line, Comma)`) and in `quotedRecord`'s unquoted-field scan. A
  record is never split on `\n` — `nextLine`/`recordEnd` handle line endings
  first. There is no validation of `Comma`; `'\n'`, `'\r'`, `'"'` are
  accepted and produce odd but defined results. (`encoding/csv` rejects, with
  "csv: invalid field or comment delimiter", any `Comma` equal to `0`, `'"'`,
  `'\r'`, `'\n'`, `utf8.RuneError` or a non-UTF-8 rune, and a `Comma` equal
  to its `Comment` field when `Comment` is configured — csv/reader.go
  `validDelim`. simdcsv has no `Comment` and validates nothing.)
- **Quote detection.** One `IndexByte` per record decides fast vs slow. The
  cost of a quoted record is therefore the fast path's detection plus the
  full careful parse.
- **Unescape.** Only the doubled-quote case allocates: `append([]byte(nil),
  rec[start:i]...)` on first doubled quote, then appends. The result is a
  fresh buffer whose length is exactly the unescaped field.

## 6. Ownership and lifetime

The rules, as implemented:

- `buf` holds the whole input for the life of the reader. It is never
  modified after `io.ReadAll`.
- A fast-path field and a simple quoted field are subslices of `buf`. They
  stay valid as long as `buf` lives — and `buf` lives as long as **any**
  retained record references it, because a subslice keeps its whole backing
  array alive. Retaining one 4-byte field retains a 10 GB file.
- An unescaped (doubled-quote) field is a fresh allocation, independent of
  `buf`.
- `own` (simdcsv.go:347) is the ownership switch:
  - `ReuseRecord = true`: the record's outer slice **is** `r.fields`. The
    next record's `split`/`quotedRecord` reuse the same backing array, so the
    previous `Record`/`Fields()` value is repointed. Consume before the next
    `Read`. Individual field *bytes* are not clobbered (the input is
    immutable and unescape buffers are per-field), but that is observed
    behavior, not a promise.
  - `ReuseRecord = false` (default): the outer slice is copied per record
    (`make([][]byte, len(fields))`), and any field that does not alias `buf`
    (i.e. an unescaped field) is copied again. Every record is fully
    independent of later reads and of the reader.
- `aliasesBuf` (simdcsv.go:366) distinguishes the two cases with pointer
  arithmetic on the backing arrays.

## 7. Malformed and truncated inputs

Established by executing both parsers on the same bytes (Go 1.26.5). The
short version: `simdcsv` **never errors on input content** — the only errors
are `io.ReadAll` failure, `io.EOF`, and the field-count check. Everything
malformed parses, with its own splits. `encoding/csv` rejects most of it.

Every row below has a decision, pinned by a case in `malformed_test.go`. The
rule behind the decisions: this package accepts everything, and therefore
**must never lose a byte**. A parser that accepts an input and silently drops
part of it is worse than one that rejects it, because nothing downstream can
tell that anything went missing.

| input | `simdcsv` result | decision | `encoding/csv` |
|---|---|---|---|
| `a"b,c\n` | `["a\"b" "c"]` — quote in unquoted field is data | accept as-is | error: bare `"` in non-quoted-field |
| `"a"b,c\n` | `["ab" "c"]` — junk after the closing quote joins its field | **changed**: was `["a" "" "c"]`, which dropped the `b` and invented an empty field | error: extraneous or missing `"` |
| `"a"x"b",c\n` | `["ax\"b\"" "c"]` — junk verbatim, quotes included | accept (same rule) | error |
| `"a" ,c\n` | `["a " "c"]` | accept (same rule; no whitespace special case) | error |
| `"a,b\n` (unclosed) | `["a,b"]` — field runs to end of record | accept as-is | error: missing `"` |
| `"a""b\n` (unclosed after doubled) | `["a\"b"]` | accept as-is | error |
| `"\n` (lone quote) | `[""]` | accept as-is | error |
| `"""\n` | `["\""]` | accept as-is | error |
| `"a,b` (EOF mid-quote) | `["a,b"]` — truncated quoted field accepted | accept as-is | error |
| no trailing newline (`a,b`) | `["a" "b"]` | parity | `["a" "b"]` — same |
| trailing `\r` at EOF (`a,b\r`) | `["a" "b"]` | parity | `["a" "b"]` — same |

**Why not `LazyQuotes`.** `encoding/csv` has a mode that also accepts most of
this, and it was probed rather than assumed: it does not agree, and the
difference matters. Under `LazyQuotes` an unterminated quote consumes the
rest of the **file**, newlines included, so `"a,b\n` swallows every following
record. Here it ends with its record and the damage stops there. Four of
eleven probed rows agree; the rest differ this way, so `LazyQuotes` is not a
parity target and the differential does not use it.

Truncated records (EOF without newline) are legal on both paths; the final
`\r` before EOF is stripped by both. Blank lines (`""`, `"\r\n"`) are skipped
by both. A record of `","` is a record of two empty fields in both.

`\r\n` inside a quoted field used to be a well-formed divergence. Task 0
decided it for parity: both packages reduce it to `\n` and both keep a lone
`\r`, so the class is inside the declared overlap now (§12).

## 8. Limits and resource model

- **Memory, input:** the whole input is in memory from the first `Read` on,
  plus one copy under `ReuseRecord=false` per retained record's unescaped
  fields. There is no bound other than the input size; a stream too large to
  hold is out of scope (`NewReader` says so).
- **Record length:** delimiter positions are `int32`, so a single record is
  bounded by `2^31-1` bytes of indexable span. Derived from the type;
  untested at that size.
- **Scratch:** `idx` is `4 × (longest line + 1)` bytes; `fields` is
  `24 × (field count of the longest record)` bytes (slice headers). Both are
  allocated once per file (grown on demand) and reused.
- **Line count:** `line` is an `int` and counts physical lines including
  skipped blanks; error messages use it. Matches `encoding/csv`'s physical
  line counting on the probed inputs (both report line 3 for `a,b\n\nc\n`).

## 9. Allocation per record (observed from the code paths)

| path | `ReuseRecord` | allocations per record |
|---|---|---|
| fast (unquoted) | true | 0 (after `idx` grown to max line length) |
| fast | false | 1 (outer `[][]byte` copy) |
| quoted, no doubled quotes | true | 0 |
| quoted, no doubled quotes | false | 1 (outer copy) |
| quoted, doubled quote present | true | 1 per unescaped field (fresh unescape buffer) |
| quoted, doubled quote present | false | **2 per unescaped field** (unescape buffer, then `own` copies it again because it does not alias `buf`) |

The double copy is the result of two facts together: `quotedRecord` allocates
a fresh per-field buffer (it no longer uses the `r.unq` scratch), and `own`
copies every field that does not alias the input. Not measured; a known
allocation quirk worth a roadmap look.

## 10. Concurrency and Reader reuse

- **Not safe for concurrent use**, by design, with no locks. `pos`, `fields`,
  `unq`, `idx`, `num`, and `line` mutate on every `Read`; the lazy
  `io.ReadAll` on the first `Read` races `buf` and the underlying reader
  itself. One `Reader` per goroutine.
- **No reset.** After `io.EOF`, `Read` returns `io.EOF` forever. There is no
  method to rewind or rebind.
- **After an error.** On a field-count error, `pos` has already advanced past
  the offending record (fast path: `r.pos = next` before `split`; slow path:
  inside `slowRecord`). The next `Read` returns the following record —
  verified by probe, and the same shape as `encoding/csv`.
- **`ReadAll` under `ReuseRecord`.** All returned records share `r.fields`,
  so every entry is the final record (documented in `Reader.ReuseRecord`'s
  comment: "the first version of this package reused unconditionally, and
  every record returned by ReadAll was the last one"). This diverges from Go
  1.25+ `encoding/csv.ReadAll`, which allocates per record and returns
  independent records even under `ReuseRecord`.

## 11. Errors and EOF

The complete error surface:

- `io.ReadAll` failure on the first `Read` — returned as-is; the reader
  retries on the next `Read`.
- `io.EOF` — after the final record, and immediately for empty input.
  `ReadAll` converts `io.EOF` to a nil error.
- Field-count mismatch — plain `error` (no `*csv.ParseError`, no
  `ErrFieldCount`), message `simdcsv: record on line %d: wrong number of
  fields: got %d, want %d`, returned **together with the record** on both
  paths (probe-verified; `slowRecord` itself never errors, so the
  `Record{}, err` branch in `Read` is currently unreachable).
- `ReadAll` **discards the offending record**: it appends only successful
  records and returns `(partial, err)`. `encoding/csv.ReadAll` returns
  `(nil, err)` instead (Go 1.26.5 source, `csv/reader.go` `ReadAll`).
- `Record.Field(i)` out of range panics (index panic), like any slice.

## 12. Compatibility with `encoding/csv`

### Identical on the declared overlap (well-formed input, excluding
CRLF-normalization-sensitive quoted data)

Verified by the differential tests in `simdcsv_test.go` (hand-written corpus
plus 400 seeded-random inputs, `TestMatchesStdlib`, `TestMatchesStdlibRandom`)
and by probe: quoted delimiters, embedded `\n` newlines, doubled quotes, blank
lines, CRLF line endings, trailing `\r` at EOF, empty fields,
no-trailing-newline files, and the record-with-error shape of `Read`.
Field-count behavior: `TestFieldsPerRecord` pins `FieldsPerRecord == 0`
(first-record learning, then learned-count enforcement) and `-1` (ragged).
Explicit positive `FieldsPerRecord` behavior is not in the test suite — it
comes from `checkCount` (source) and probe confirmation of the error path,
not from the differential corpus. The tests assert `(stdlib err
!= nil) == (simdcsv err != nil)` and byte-equality of fields via `Strings()`.

**Overlap: CRLF in a quoted field — decided, parity.** A quoted field
containing `\r\n` used to parse differently: simdcsv preserved it,
`encoding/csv` reduces it to `\n`. Plan Task 0/Stage 0 decided for parity,
because a field whose bytes differ is silent data corruption for anyone
swapping this in, and because the exclusion put a permanent hole in the
strongest gate this package has. `quotedRecord` now normalizes, and the
class is **inside** the declared overlap with six cases in the differential
corpus.

A lone `\r` is data and is preserved, by both packages. The cost is gated:
one scan of the record decides whether any field can need normalizing, so a
record with no CR keeps the zero-copy path unchanged, and a field that does
carry a CRLF is copied into the same scratch the doubled-quote unescape
already builds. That cost has not been measured on a quiet machine.

### Missing from `simdcsv` (present in `encoding/csv`)

`Comment` (comment lines), `LazyQuotes`, `TrimLeadingSpace`, `FieldPos`,
`InputOffset`, `TrailingComma` (deprecated in stdlib), rune `Comma`, and the
`*csv.ParseError` type with `Err`/`Line`/`Column`/`StartLine`.
`ReuseRecord` exists in both with different `ReadAll` consequences (§10).

### Divergent behavior (all probe-verified against Go 1.26.5)

1. Malformed quotes parse with this package's own splits (§7 table) where
   stdlib errors.
2. Error values and text differ; no `ParseError`; stdlib's count error is
   `*csv.ParseError` with `ErrFieldCount`, this one is a plain error.
3. `Comma` validation: stdlib rejects `Comma` equal to `0`, `'"'`, `'\r'`,
   `'\n'`, `utf8.RuneError` or any non-UTF-8 rune, and a `Comma` equal to its
   `Comment` when configured ("csv: invalid field or comment delimiter");
   simdcsv accepts any byte. (`Comma = ';'` works identically in both.)
4. `ReadAll` error shape: simdcsv returns partial records; stdlib returns
   `nil`.
5. `ReadAll` + `ReuseRecord`: simdcsv aliases everything to the last record;
   stdlib allocates each record freshly, independent of `ReuseRecord`.
6. Quoted-field CRLF normalization (well-formed, probe-verified with exact
   bytes): `"a\r\nb",c` → simdcsv `["a\r\nb" "c"]`, stdlib `["a\nb" "c"]`.
   Excluded from the declared overlap; policy decision pending
   ([plan Task 0/Stage 0](plans/2026-08-13-simdcsv-production.md#task-0-stage-0-quoted-field-crlf-normalization-policy-and-corpus-case)).

This is why the package is "not a drop-in": the declared overlap is
identical, the malformed and error surfaces are not, and one well-formed
input class (quoted CRLF) is documented as diverging with the policy still
open.

## 13. Known source-doc defects

The package doc comments are stale in four places. None is fixed in code (a
docs task does not edit Go); they are contract work in the roadmap
(`docs/plans/2026-08-13-simdcsv-production.md`):

1. `simdcsv.go:61-63` — package comment promises `[ParseInts]` and
   `[ParseFloats]` ("convert a whole column in one call, which is about five
   times what strconv manages per value"). Neither function exists in the
   package (grep: no definition anywhere). Broken doc links; `go vet` does
   not flag them.
2. `simdcsv.go:80-83` — `Reader.Comma`'s comment: "A rune delimiter falls
   back to encoding/csv; see [NewReader]." There is no fallback — `Comma` is
   a byte and `NewReader`'s comment says nothing about runes. A rune constant
   above `0xFF` does not compile into it; a typed rune truncates.
3. `simdcsv.go:111-112` — `Reader` struct comment: "Records the fast path
   cannot take — anything containing a quote — go to encoding/csv, so the
   answer is the standard library's answer." Quoted records are parsed
   in-house by `quotedRecord`; delegation was removed because it measured
   4700x slower per record (`docs/wrong.md` entry 3).
4. `simdcsv_test.go:15` — test comment: "the quoted path literally delegates
   to it." Same staleness as 3.

## 14. Vestigial state

Three fields exist but are never used for their declared purpose, all facts
from the source:

- `qidx` ("quote positions, reused across records") and `nidx` ("newline
  positions") — declared, never read or written. They belonged to the
  quadratic whole-buffer design that `recordEnd` replaced (§4.1).
- `unq` ("scratch for unescaping doubled quotes") — reset to empty in
  `quotedRecord` and never read; the unescape buffers are per-field fresh
  allocations instead (§4.2). Its disuse is why `own` double-copies
  unescaped fields under `ReuseRecord=false` (§9).
