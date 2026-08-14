# simdcsv

`simdcsv` is a whole-buffer CSV reader for Go. It returns fields as `[]byte` and
uses [simd.go](https://github.com/sebishogun/simd) to find delimiters in
quote-free records with one vector scan. Quoted records use a separate parser.
No cgo is required.

Requires Go 1.25 or later and uses `github.com/sebishogun/simd v1.20.0`.

```sh
go get github.com/sebishogun/simdcsv
```

```go
r := simdcsv.NewReader(f)

for {
	rec, err := r.Read()
	if err == io.EOF {
		break
	}
	if err != nil {
		return err
	}
	name := rec.Field(0) // borrowed []byte; copy if it must outlive the input
	_ = name
}
```

## API

`NewReader(io.Reader)` creates a reader with comma as its delimiter. The first
call to `Read` consumes the complete input with `io.ReadAll`; construction
itself does not read.

The reader exposes four controls:

- `Comma` is a single-byte delimiter and defaults to `,`.
- `FieldsPerRecord` follows `encoding/csv`: a positive value requires that
  count, zero learns it from the first record, and a negative value disables
  the check.
- `ReuseRecord` reuses the record's outer field slice. When enabled, consume a
  record before the next `Read`; do not retain its `Record` or `Fields()` value.
- `TrimLeadingSpace` drops leading unicode whitespace from each field, as
  `encoding/csv` does. Off by default; the scan runs per field only when set,
  and a trimmed field is still a subslice, so it costs no copy.

`Read` returns `io.EOF` after the final record. A field-count error is returned
with the record that caused it. `ReadAll` returns the records read before the
first non-EOF error, and its records are always independent -- it ignores
`ReuseRecord` rather than handing back N aliases of one record.

`Record` provides:

- `Len()` for the field count;
- `Field(i)` for one field, with the usual out-of-range panic;
- `Fields()` for the internal `[][]byte` without copying;
- `Strings()` for an allocated `[]string` copy.

## Ownership and memory

The entire input is retained in memory. Most fields are subslices of that input
and are not copied. A quoted field containing doubled quotes must be unescaped
into separate storage. With the default `ReuseRecord=false`, the record's outer
slice and unescaped fields remain valid after later reads; ordinary fields still
keep the full input buffer alive.

`Fields()` exposes the record's internal slice, and each field is mutable
`[]byte`. Copy data before mutating it if other records or the reader may still
observe the same input buffer. For an input too large to buffer, use
`encoding/csv`.

Bounded reading was prototyped and measured rather than assumed impossible: a
64 KiB window parses an 85 MB input at 0.86x-1.05x of the whole-buffer path,
because the window stays in cache. It does not ship because it cannot keep the
property this API is built on -- under a bounded buffer a record cannot outlive
the next `Read`, and buying that back by copying each record out costs 1.6x and
triples the allocation count. Both measurements are in
[docs/wrong.md](docs/wrong.md).

## CSV compatibility

The tested well-formed surface agrees with `encoding/csv` for comma-separated
records, blank lines, CRLF line endings, empty fields, quoted delimiters,
embedded `\n` newlines, doubled quotes, and `\r\n` inside a quoted field.
`Comma`, `FieldsPerRecord`, `ReuseRecord` and `TrimLeadingSpace` have
analogous roles, but this is not a drop-in `encoding/csv.Reader`.

There is no longer a well-formed exception. A quoted `\r\n` used to be
preserved here where `encoding/csv` reduces it to `\n`; it now normalizes the
same way, so no well-formed input class is excluded from the differential
corpus. The cost is gated -- one scan decides whether any field in the record
can need it, so a record with no CR keeps the zero-copy path.

The remaining `encoding/csv` options are refused, each with a reason recorded
in [docs/architecture.md](docs/architecture.md) §12: `LazyQuotes` (an
unterminated quote there consumes the rest of the file, so one stray byte
destroys every following record), `Comment` (needs a per-line scan of its own,
breaking the one-pass property), rune delimiters (a substring search per record
instead of a byte compare), and `ParseError`-shaped errors (this package has no
parse error to shape). `FieldPos` and `InputOffset` are absent.

One error shape still differs: `ReadAll` returns the records parsed before an
error, where stdlib returns `nil`. It is not a CSV validator either --
malformed quote syntax may be accepted or reported differently from
`encoding/csv`. Validate untrusted CSV with the standard library when matching
its rejection behavior matters.

## Performance

The v0.1.0 release recorded this Zen 5 comparison on 20,000 generated rows, with
`ReuseRecord` enabled on both readers. The benchmark source runs both
implementations on the same bytes; the raw release output was not retained.

It has not been re-measured for v0.2.0. The unquoted rows are unaffected by
anything v0.2.0 changed; the quoted rows are a lower bound rather than a current
reading, because `quotedRecord` gained one `IndexByte` over the record to decide
whether any field can need CRLF normalizing. The table is the shape of the
trade, not a gate.

| input | `encoding/csv` | `simdcsv` | ratio |
|---|---:|---:|---:|
| 16 columns, unquoted | 3.54 ms | 1.84 ms | **1.92x** |
| 4 columns, unquoted | 1.06 ms | 0.71 ms | **1.49x** |
| 16 columns, quoted | 3.74 ms | 3.50 ms | 1.07x |
| 4 columns, quoted | 1.10 ms | 1.35 ms | **0.81x** |

The loss remains part of the contract: four-column, fully quoted input ran at
0.81x, about 23% slower than `encoding/csv`. Quotes make delimiter meaning
stateful, so the vector split cannot run. Handing quoted records to
`encoding/csv` and skipping SIMD on short spans were both measured and rejected
because they made another part of the workload slower.

The benchmark is committed in `simdcsv_test.go`:

```sh
go test -run '^$' -bench '^BenchmarkRead$' -benchmem -count=6
```

The exact release timings are a historical measurement, not a regression gate;
measure your input mix on the target machine. No wall-clock result is claimed
outside amd64.

## Verification

```sh
go test -count=1 ./...
go test -count=1 -race ./...
go vet ./...
```

The suite compares well-formed hand-written and deterministic randomized inputs
against `encoding/csv`, checks field-count behavior and custom delimiters, and
asserts that the unquoted path returns fields which alias the input buffer. The
malformed surface is pinned case by case rather than left undefined, since
`encoding/csv` errors where this package splits.

Three fuzz targets back the differential and the contract:

```sh
go test -run '^$' -fuzz FuzzOverlap -fuzztime 90s
go test -run '^$' -fuzz FuzzNoPanic -fuzztime 90s
go test -run '^$' -fuzz FuzzContractMalformed -fuzztime 90s
```

`FuzzOverlap` asserts byte-equality with `encoding/csv` on inputs both accept,
`FuzzNoPanic` that no input panics, and `FuzzContractMalformed` that a malformed
record still yields fields that alias the input and a reader that keeps going.

## Status

The latest tagged and published release is **v0.2.0**, which closes the
production plan: quoted-CRLF parity with `encoding/csv`, `ReadAll` returning
independent records, a pinned decision for every malformed row shape and every
compatibility option, `TrimLeadingSpace`, and three fuzz gates. Two designs were
measured and refused rather than shipped -- bounded reading and streaming
delivery -- and their numbers are the deliverable. [CHANGELOG.md](CHANGELOG.md)
has the differences from v0.1.0, which used `simd v1.2.0`.

The API is pre-1.0. v0.2.0 adds one field to `Reader` and changes no existing
name, signature or meaning.

The maintained inventory of libraries built on `simd` is in the
[`simd` README](https://github.com/sebishogun/simd#built-on-this).

## Documentation

- [CHANGELOG.md](CHANGELOG.md) — what each release changed, and why.
- [docs/architecture.md](docs/architecture.md) — model, state machines, ownership, allocation, malformed-input behavior, and the exact `encoding/csv` boundary.
- [docs/lld/reader.md](docs/lld/reader.md) — function-level design of the reader.
- [docs/verification.md](docs/verification.md) — gates, differential-test rules, fuzz plan, and the measurement methodology.
- [docs/wrong.md](docs/wrong.md) — the measured dead ends and what shipped instead.
- [docs/roadmap.md](docs/roadmap.md) — production goals; nothing there is shipped.
- [docs/plans/2026-08-13-simdcsv-production-design.md](docs/plans/2026-08-13-simdcsv-production-design.md) and [docs/plans/2026-08-13-simdcsv-production.md](docs/plans/2026-08-13-simdcsv-production.md) — the production plan.
- [AGENTS.md](AGENTS.md) / [CLAUDE.md](CLAUDE.md) — agent guidance, self-contained.

## License

MIT. See [LICENSE](LICENSE). `simd` is MIT; the indirect `golang.org/x/sys`
dependency is BSD-3-Clause.
