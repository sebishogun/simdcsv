# simdcsv — agent guidance

Self-contained orientation for agents working in this repository. Read this
before anything else: it is the boundary, the facts, and the gates. It applies
to the whole repository and does not depend on any other file.

## What this is

`simdcsv` is a whole-buffer CSV reader for Go built on
[github.com/sebishogun/simd](https://github.com/sebishogun/simd). It finds
every delimiter in a quote-free record with one vector scan
([`simd.IndexAll`]) and returns fields as `[]byte` subslices instead of copies.
Records containing a quote are parsed by a careful in-house path. No cgo.

The repository is one package plus its module and docs: `simdcsv.go`
(~430 lines), `simdcsv_test.go`, `go.mod`/`go.sum`, `LICENSE`, `README.md`,
and `docs/`. There is no CI, no Makefile, no assets, and no build machinery
beyond the Go toolchain.

## Boundary (source-backed, `simdcsv.go`)

The entire public surface is:

- `NewReader(io.Reader) *Reader` — construction reads nothing; the whole input
  is read with `io.ReadAll` on the first call to `Read`. An `io.ReadAll`
  error is returned from that `Read` and the read is retried on the next call.
- `Reader.Comma byte` — defaults to `,`. A byte, not a rune: the vector scan
  compares bytes. No delimiter validation of any kind.
- `Reader.FieldsPerRecord int` — `0` learns the count from the first record,
  positive requires exactly that many, negative allows any.
- `Reader.ReuseRecord bool` — default `false`. `false` makes every record
  independent of later reads; `true` reuses the record's outer field slice.
- `Read() (Record, error)` — `io.EOF` after the final record; a field-count
  error is returned together with the record that caused it (both paths).
- `ReadAll() ([]Record, error)` — records read before the first non-EOF error;
  the offending record is discarded.
- `Record` — `Len()`, `Field(i)` (index panic out of range), `Fields()`
  (internal `[][]byte`, no copy), `Strings()` (allocated `[]string`).

Nothing else is public. `internal`-style helpers (`nextLine`, `split`,
`recordEnd`, `quotedRecord`, `own`, `aliasesBuf`, `checkCount`) are unexported
and may change freely.

## Non-goals

- Not a drop-in `encoding/csv` replacement: missing options (`Comment`,
  `LazyQuotes`, `TrimLeadingSpace`, `FieldPos`, `InputOffset`, `ParseError`
  values), byte-only delimiter, different error text. Exact list in
  `docs/architecture.md`.
- Not a CSV validator: malformed quote syntax is accepted with this package's
  own field splits, silently, where `encoding/csv` errors. Never claim parity
  on malformed input.
- Not streaming: the whole input is buffered before the first record is
  returned. Streams too large to hold belong to `encoding/csv`.
- No performance claim outside amd64, and no wall-clock regression gate: the
  committed numbers are a historical record, not a bar.

## Status

- Latest tagged and published release: **v0.1.0** (commit `5da3d42`), built
  against `simd v1.2.0`, `go 1.25.0`.
- `main` (`be2b26c`, at the time of writing): `simd v1.20.0`, not tagged. The
  API is pre-1.0 and may change.
- `docs/v120-documentation`: documentation-only branch; it began with `d44455f`
  ("docs: document simdcsv contracts") and carries docs and nothing else.

## Read order

1. `README.md` — the surface.
2. `docs/architecture.md` — model, state machines, ownership, compatibility.
3. `docs/lld/reader.md` — function-level design.
4. `docs/wrong.md` — the measured dead ends (do not re-try them).
5. `docs/roadmap.md`, `docs/plans/` — where it is going; none of it is shipped.
6. `simdcsv.go`, `simdcsv_test.go` — the source of truth.

## Comments can be stale; code wins

The package doc comment references `[ParseInts]` and `[ParseFloats]` that do
not exist; `Reader.Comma`'s comment claims a rune-delimiter fallback that does
not exist; the `Reader` struct comment says quoted records go to
`encoding/csv`, which they no longer do; the test comment says the quoted path
"literally delegates" to it. All four are recorded as known source-doc defects
in `docs/architecture.md` and must not be copied into new text as fact. When
source and comment disagree, believe the source — or better, probe the
behavior and record what actually happens.

## Ownership and lifetime

- Most fields are subslices of the reader's input buffer: zero-copy, and they
  keep the **entire** input buffer alive, so retaining one small field retains
  the whole file.
- A field containing doubled quotes is unescaped into a fresh per-field
  buffer (never into the input). Under the default `ReuseRecord=false` such a
  field is copied a second time by `own` (observed; see `docs/architecture.md`
  §9) — do not present it as allocation-free.
- `ReuseRecord=true`: consume a record before the next `Read`; do not retain
  its `Record` or `Fields()` value. `ReadAll` under `ReuseRecord=true` returns
  the final record for every entry (this is documented behavior and diverges
  from Go 1.25+ `encoding/csv.ReadAll`, which allocates each record freshly,
  independent of `ReuseRecord`).
- Records outlive the reader: under `ReuseRecord=false` every record is
  independent; even under `ReuseRecord=true` the input buffer and unescape
  buffers are not overwritten (only the outer slice headers are), but that is
  observed behavior, not a promise.

## encoding/csv compatibility

Identical on the declared overlap: quoted delimiters, embedded `\n` newlines,
doubled quotes, blank-line skipping, CRLF line endings and trailing-CR-at-EOF
stripping, empty fields, field-count checks, and the record-with-error shape
of `Read`. Verified by the differential tests and by probes against Go 1.26.5
(`encoding/csv`).

Divergences (all empirical, see `docs/architecture.md` §12 for the table):

- malformed quotes are accepted with this package's own splits (`a"b`,
  `"a"b,c`, unclosed quotes, bare `"`, `"""` all parse; stdlib errors on all);
- error text differs and there is no `*csv.ParseError`;
- **quoted-field CRLF normalization:** `\r\n` inside a quoted field is
  preserved here but normalized to `\n` by `encoding/csv`. This is a
  well-formed divergence; the declared overlap excludes CRLF-normalization-
  sensitive quoted data until a policy decision, and the differential corpus
  currently has no such case (verification gap, Task 0/Stage 0 in the plan);
- stdlib rejects `Comma` equal to `0`, `'\r'`, `'\n'`, `'"'`, `utf8.RuneError`
  (or any non-UTF-8 rune), and equal to its `Comment` field when configured;
  simdcsv accepts any byte;
- stdlib `ReadAll` returns `nil, err` on the first error; simdcsv returns the
  partial records;
- stdlib `ReadAll` with `ReuseRecord` allocates each record freshly; simdcsv's
  entries all alias the last one.

Differential tests against `encoding/csv` may only run within the declared
overlap (well-formed input excluding CRLF-normalization-sensitive quoted
data). Malformed-input behavior is a documented contract of its own, tested
by its own cases, not compared to stdlib.

## Concurrency

Not safe for concurrent use, by design and without locks: `pos`, `fields`,
`unq`, `idx`, `num`, `line`, and the lazily-initialized `buf` all mutate on
every `Read`, so even a single goroutine reading is stateful. The lazy
first-`Read` means two goroutines calling `Read` at once race on `buf` and on
the underlying `io.Reader`. Use one `Reader` per goroutine. A `Reader` cannot
be reset: after `io.EOF` it returns `io.EOF` forever; after a field-count
error the position has already advanced past the failing record (matching
`encoding/csv`).

## Performance work

- **Disassemble first, always.** Before theorizing about speed:
  `go test -c -o /tmp/x.test .` then
  `go tool objdump -s 'github.com/sebishogun/simdcsv\.' /tmp/x.test`. The
  disassembly shows register pressure, bounds-check elimination, inlining, and
  which branch is fallthrough — nothing else does.
- The wall-clock noise floor here is **8.3%**. Below that, compare
  `perf stat -e instructions:u,cycles:u` (layout-independent) and read the
  disassembly. More samples do not beat layout noise; it is per-build.
- A/B builds must be interleaved in one session, compared on the minimum,
  never across sessions. Machine idle (load average < 1) before measuring.
- **Never pipe a gate through `tail` (or anything) without `pipefail`** — the
  pipe reports the last command's status and a red gate vanishes. Run gates
  bare, or `set -o pipefail` first.
- The benchmark in `simdcsv_test.go` (`BenchmarkRead`) is the reproduction
  command for the committed numbers; it is not a regression gate.

## Gates (run before claiming anything done)

```
go test -count=1 ./...
go test -count=1 -race ./...
go vet ./...
git diff --check
```

Plus, for documentation work: a manual link check over every `.md` touched
(internal relative links resolve; the external links
`https://github.com/sebishogun/simd` and
`https://github.com/sebishogun/simd#built-on-this` resolve), and a full read
of every `.md` file in the commit range for consistency. A documentation
commit changes only `.md` files — never Go, tests, `go.mod`/`go.sum`, build
files, workflows, or assets. Do not push unless asked.

## wrong.md

`docs/wrong.md` records only **sourced measurements**: all-quoted narrow
records at 0.81x, delegating quoted records to `encoding/csv` wholesale at
~0.38x (and per-record at 4700x, the earlier design), skipping the SIMD call
on short spans at 1.30x, and the quadratic whole-buffer scan (17 s vs 3.6 ms
on 20,000 rows). Every entry cites the source line in the package comments or
the README table. A measurement that argues against a change belongs there
whether or not code changed; an unsourced number does not belong anywhere.

## Roadmap is not shipped

`docs/roadmap.md` and `docs/plans/2026-08-13-*` describe a production future:
hardening malformed-input contracts and evaluating bounded/streaming and
compatibility options **based on evidence**. Nothing there is implemented or
promised. An option whose evaluation misses its bar is deleted and recorded in
`docs/wrong.md`, not shipped with a caveat.
