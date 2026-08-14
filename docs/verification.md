# simdcsv verification

How this package is verified, and how performance claims are made. The
distinction matters: correctness is gated, performance is measured and
recorded — the committed numbers are history, not a bar.

## Gates — run before claiming anything done

All bare — **never pipe a gate through `tail` (or anything) without
`pipefail`**: the pipe reports the last command's status and a red gate
vanishes. Run gates bare, or `set -o pipefail` first.

```
go test -count=1 ./...
go test -count=1 -race ./...
go vet ./...
git diff --check
```

Documentation work adds:

- **Manual link check:** every internal relative link in every `.md`
  touched resolves (the docs cross-reference `docs/architecture.md`,
  `docs/lld/reader.md`, `docs/roadmap.md`, `docs/verification.md`,
  `docs/wrong.md`, `docs/plans/`, `README.md`, `AGENTS.md`, `CLAUDE.md`,
  `LICENSE`); the external links resolve (`https://github.com/sebishogun/simd`
  and `https://github.com/sebishogun/simd#built-on-this` — anchor verified
  against the simd README's "## Built on this").
- **Full range read:** every `.md` in the commit range (`main...HEAD`) is
  read end-to-end for consistency with the source. A docs commit changes
  only `.md` files.

## Differential testing within the declared overlap

`simdcsv_test.go` compares this package against `encoding/csv` on the same
bytes, within the **declared overlap**: well-formed input. A quoted field
containing `\r\n` was excluded until plan Task 0/Stage 0 decided it for
parity; both packages now reduce it to `\n`, both preserve a lone `\r`, and
six such cases are in the corpus (architecture.md §12).
`checkAgainstStdlib` (simdcsv_test.go:16) asserts error-parity and
field-parity (`Strings()` equality). The corpus:

- `TestMatchesStdlib` (simdcsv_test.go:46) — hand-written cases: empty
  input, no trailing newline, blank lines, CRLF line endings, empty fields,
  single column, quoted delimiters, embedded `\n` newlines, doubled quotes,
  mixed plain/quoted rows, 100-row stress.
- `TestMatchesStdlibRandom` (simdcsv_test.go:72) — 400 seeded-random inputs
  (`rand.NewPCG(1, 2)`) over atoms including quoted and multi-line fields
  (`\n` only inside quotes, never `\r\n`). Fixed seed: the corpus is
  reproducible and does not rot.
- `TestFastPathRuns` (simdcsv_test.go:93) — the fast path must actually run:
  a fast-path field must alias the reader's buffer (pointer-range check via
  `unsafe`, `aliases` at simdcsv_test.go:194). A suite where every test
  passes by delegation is not testing this package.
- `TestFieldsPerRecord` — count learning, learned-count enforcement, `-1`
  ragged (no explicit positive-value case in the suite).
  `TestSemicolonDelimiter` — custom byte delimiter.

**That gap is closed.** The corpus carried no quoted-field CRLF case while
the two packages disagreed, and the overlap definition excluding the class
was what kept the suite sound. Plan Task 0/Stage 0 decided the class for
parity, `quotedRecord` normalizes, and six cases are in the corpus above:
CRLF inside a quoted field, a doubled CRLF, one in a later field, a lone CR
mid-field, a lone CR at field end, and a CRLF beside a doubled quote. The
overlap is now every well-formed input, with no class held back.

**Fuzz gates.** Three targets, each run 90s at the v0.2.0 release gate:
`FuzzOverlap` (50.1M execs) asserts byte-equality with `encoding/csv` on
inputs both accept, `FuzzNoPanic` (58.0M) that nothing panics, and
`FuzzContractMalformed` (8.7M) that a malformed record still yields fields
aliasing the input and a reader that continues. No failures, so nothing was
written to `testdata/fuzz` -- inputs the run finds interesting live in the
build cache, and only a failing input is committed as a regression seed.

**The rule:** a differential test may only feed the overlap. Malformed
inputs (`a"b`, `"a"b,c`, unclosed quotes) are *documented divergences*
(docs/architecture.md §7), not parity failures; they get contract tests of
their own in the production plan, never differential ones. The existing
random corpus must stay well-formed for the same reason.

## Doc-reference gate

Comments have no compiler, so a doc link to a function that does not exist
survives every other gate. This one is a grep, run with the rest:

```
! grep -n 'ParseInts\|ParseFloats' *.go     # functions that never existed
! grep -n 'literally delegates' *_test.go   # the quoted path is parsed in-house
go vet ./...                                # doc-link syntax
```

Four comments claimed things the code does not do: a package-doc paragraph
advertising `ParseInts`/`ParseFloats`, a `Comma` note promising a rune
delimiter falls back to `encoding/csv`, a `Reader` field note sending quoted
records to `encoding/csv`, and a test comment saying the quoted path
delegates there. Nothing delegates: `quotedRecord` parses in-house.

## Fuzz gates ([production plan task 2](plans/2026-08-13-simdcsv-production.md#task-2-fuzz-harness-as-a-gate))

Three targets in `fuzz_test.go`, shipped and seeded. Smoke times below were
run locally, not assumed:

```
go test -run '^$' -fuzz FuzzOverlap           -fuzztime 25s .   # 14.4M execs
go test -run '^$' -fuzz FuzzNoPanic           -fuzztime 12s .   #  4.1M execs
go test -run '^$' -fuzz FuzzContractMalformed -fuzztime 12s .   #  0.9M execs
go test -race -run '^$' -fuzz FuzzOverlap     -fuzztime 8s  .
```

1. **`FuzzOverlap` — differential within the overlap.** The generator builds
   well-formed documents out of atoms (quoted, unquoted, doubled-quote,
   embedded-LF, delimiters from `{',', ';'}`, CRLF at record level) rather
   than mutating raw bytes, because raw bytes leave the overlap immediately
   and the differential then proves nothing. It checks its own expectation
   against `encoding/csv` as a second opinion. A failure here is a bug.
2. **`FuzzNoPanic` — arbitrary bytes.** Any input, any `Comma`, both
   `ReuseRecord` values, read to EOF under a ten-second per-input deadline.
   The parser must terminate and must not panic; malformed input may produce
   any record content, which is contract rather than bug.
3. **`FuzzContractMalformed` — the malformed decisions stay pinned.**
   Whatever junk follows a closing quote, every data byte of the input must
   come back out in some field (architecture.md §7).

**A finding, on the first run.** `FuzzOverlap` failed its own seed corpus
immediately: a one-column record holding an unquoted empty field renders as
a blank line, and a blank line is skipped by both parsers, so the document
parsed to fewer records than the generator built. That is the generator's
fault, not the parser's — the plan's rule for exactly this case — so the
generator now quotes empty atoms, and the failing input stays committed as
`testdata/fuzz/FuzzOverlap/fdfe2dc8bbeeb576` so a fresh clone replays it.

Seed corpus: the `TestMatchesStdlib` inputs plus the §7 table rows.
Outcome policy: a differential failure is a bug and blocks; a divergence
outside the overlap is a plan decision ([roadmap workstream 1](roadmap.md#1-harden-malformed-input-safety-and-contracts)), not a bug —
recorded, never "fixed" silently. A generator that produces input outside the
overlap is fixed in the generator, and the input is kept as a seed.

## Benchmarks

The reproduction command (from `README.md`):

```
go test -run '^$' -bench '^BenchmarkRead$' -benchmem -count=6
```

`BenchmarkRead` (simdcsv_test.go:150) runs both `encoding/csv` and
`simdcsv` on the same generated bytes, 4/16 columns × quoted/unquoted,
20,000 rows, `ReuseRecord` on both sides, `b.SetBytes` set. The committed
numbers (1.92x / 1.49x / 1.07x / 0.81x; table in README) are the v0.1.0-era
Zen 5 record. They are **not a regression gate** — no wall-clock claim is
made outside amd64, and the noise floor makes a gate dishonest.

### How to measure anything here

- **Disassemble first, always.** Before theorizing about a slowdown, look at
  the instructions:
  `go test -c -o /tmp/x.test .` then
  `go tool objdump -s 'github.com/sebishogun/simdcsv\.' /tmp/x.test`.
  Register pressure (a spilled loop counter), bounds checks that survived,
  an index multiply that stayed a multiply, a `memmove` where inline stores
  were expected, which branch is fallthrough — the disassembly says what no
  performance counter says.
- **The noise floor is 8.3%** (code-layout noise, per-build). A wall-clock
  difference smaller than that is not tellable from nothing, and more
  samples do not help. For sub-floor changes compare
  `perf stat -e instructions:u,cycles:u` (layout-independent) and read the
  disassembly for the *why*.
- **A/B protocol:** both builds in one interleaved session; compare the
  minimum; never compare across sessions. Machine quiet: load average
  under 1, nothing else running (a stray `go vet` or `git commit` in
  another shell fabricates regressions — documented in the simd repo's
  wrong.md entry 21).
- **Gates stay bare** (see above).

## Cross-architecture

- `simdcsv` is pure Go on `simd`, which dispatches per architecture; the
  package builds and tests anywhere Go runs. The **performance claims are
  amd64-only** (Zen 5, committed record) — nothing else is claimed.
- Cross-arch gate for changes: `GOARCH=arm64 go build ./...` and
  `GOARCH=arm64 go test -c ./...` at minimum (build-level; runtime checks
  need hardware or emulation, which this repository does not promise).
  A performance claim for another architecture must be measured there and
  recorded, following the A/B protocol.

## Release and dependency gates

- Facts are pinned per commit, not per memory: the tagged release v0.1.0
  builds against `simd v1.2.0`; `main` against `simd v1.20.0` (go.mod).
  Any text that states a dependency or release version is checked against
  go.mod and the tag list before it is written.
- Release claims: nothing is a release until it is tagged and published;
  the README's Status section is authoritative for release status.
- The API is pre-1.0; doc text must not imply stability.

## Documentation gates

- `go vet ./...` covers doc-comment syntax; the known stale references
  (`ParseInts`/`ParseFloats`, the rune-fallback cross-reference, the
  delegation comments — architecture.md §13) are tracked there and in the
  roadmap, and must not reappear in new text as fact.
- Every behavioral claim in the docs is either cited to a source line or
  probe-backed; an unverifiable number does not get written.
- `docs/wrong.md` accepts only sourced measurements; a measurement that
  argues against a change is recorded there even when no code changed.
