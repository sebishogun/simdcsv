# CLAUDE.md — working in simdcsv

Instructions for working in this repository. Self-contained; this file is the
boundary, the facts, and the gates.

## The repository

One Go package (`simdcsv.go`, `simdcsv_test.go`) with module files
(`go.mod`/`go.sum`), `LICENSE`, a README, and `docs/`. A whole-buffer CSV
reader: one vector scan per unquoted record via `github.com/sebishogun/simd`,
fields returned as `[]byte` subslices, quoted records parsed in-house. No
cgo, no CI, no Makefile, no assets.

## Source of truth

`simdcsv.go` and `simdcsv_test.go` are the truth. The doc comments are not all
truth: the package comment references `[ParseInts]`/`[ParseFloats]` which do
not exist, `Reader.Comma`'s comment claims a rune fallback that does not
exist, and two comments say quoted records are delegated to `encoding/csv`,
which they are not. These are recorded as known source-doc defects in
`docs/architecture.md` §13 — do not propagate them, and never "fix" them by
editing Go in a documentation commit.

When source and comment disagree, and the difference is behavioral, probe it:
run both `simdcsv` and `encoding/csv` on the same bytes from a throwaway
program in `/tmp`, and record what actually happens. Behavioral claims in
these docs are probe-backed against Go 1.26.5 unless a source line is cited.

## The boundary (everything public)

`NewReader`, `Reader.Comma` (byte), `Reader.FieldsPerRecord`,
`Reader.ReuseRecord`, `Read`, `ReadAll`, and `Record`'s `Len`/`Field`/`Fields`/
`Strings`. That is all. `ReuseRecord=false` (default) makes each record
independent; `true` reuses the outer slice — consume before the next `Read`.
The whole input is read on the first `Read` and retained; fields alias it
unless unescaped (doubled quotes), in which case they are fresh copies.

`\r\n` inside a quoted field is reduced to `\n` here and by `encoding/csv`;
a lone `\r` is data in both. That used to be a divergence with the class
excluded from the differential overlap; Task 0/Stage 0 of the production plan
decided it for parity, and six cases are now in the corpus. A record with no
CR keeps the zero-copy path; only a field carrying a CRLF is copied.

## Non-goals — do not drift into these

- Drop-in `encoding/csv` parity (missing `Comment`, `LazyQuotes`,
  `TrimLeadingSpace`, `FieldPos`, `InputOffset`, `ParseError`; byte-only
  `Comma`; different error text).
- CSV validation (malformed quotes are accepted with this package's own
  splits; that is a documented contract, not a bug).
- Streaming or bounded memory (the design is whole-buffer; alternatives are
  roadmap evaluation items, not background changes).
- Performance claims outside amd64, and wall-clock regression gates (the
  committed numbers are a historical record).

## Status facts

v0.1.0 is the latest tagged/published release (`simd v1.2.0`, go 1.25.0);
`main` uses `simd v1.20.0` and is not tagged; API is pre-1.0.
`docs/v120-documentation` is a docs-only branch.

## Read order

README → `docs/architecture.md` → `docs/lld/reader.md` → `docs/verification.md` →
`docs/wrong.md` → `docs/roadmap.md` + `docs/plans/` → source.

## Concurrency and reuse

Not safe for concurrent use, no locks, one Reader per goroutine. No reset:
EOF is permanent; after a field-count error the position has already advanced
past the failing record. `ReadAll` ignores `ReuseRecord` and returns independent records, as
`encoding/csv` does. It used to honour the flag and hand back a slice whose
entries all aliased the last record parsed — three records in, the same record
three times out, with no error to say so.

## Performance work

- Disassemble first: `go test -c -o /tmp/x.test .` +
  `go tool objdump -s 'github.com/sebishogun/simdcsv\.' /tmp/x.test`.
- Wall-clock noise floor 8.3%; below it use
  `perf stat -e instructions:u,cycles:u` and the disassembly.
- A/B builds interleaved in one session, minima, never across sessions,
  machine idle (load < 1).
- **Bare gates or `set -o pipefail` — never pipe a gate through `tail`**; a
  piped gate reports the last command's status and a failure vanishes.

## Gates

Before claiming anything done:

```
go test -count=1 ./...
go test -count=1 -race ./...
go vet ./...
git diff --check
```

For docs: every `.md` in the change is read end-to-end; internal relative
links resolve; the two external links (simd repo, `#built-on-this` anchor)
resolve. A docs commit touches only `.md`. No push unless asked.

## wrong.md

Sourced measurements only: 0.81x all-quoted, ~0.38x wholesale delegation,
4700x per-record delegation, 1.30x skipping SIMD on short spans, 17 s vs
3.6 ms quadratic scan — each cited to its source line. A measurement that
argues against a change is recorded even if nothing shipped; an unsourced
number never is.

## Roadmap is not shipped

`docs/roadmap.md` and the `docs/plans/2026-08-13-*` plans are a production
future: harden malformed-input safety and contracts; evaluate bounded/
streaming and compatibility options on evidence, not promise. A failed
evaluation is deleted and recorded in `docs/wrong.md`. Nothing in those files
exists in the code.
