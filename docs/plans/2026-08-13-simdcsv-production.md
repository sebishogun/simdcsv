# simdcsv Production Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Turn simdcsv from a fast reader with accidental malformed-input
behavior into a production CSV reader: every malformed-input class becomes a
tested, documented decision; the source-doc defects are fixed; fuzz is a
gate; bounded/streaming and compatibility options are evaluated on
measurement with delete-and-record outcomes; and the results are recorded in
`docs/` per the documentation strategy.

**Architecture:** Whole-buffer model stays. Fast path (one `IndexAll` scan
per unquoted record, zero-copy subslices) and in-house quoted path
(`recordEnd` parity walk + `quotedRecord` field machine) stay. Workstream A
first: decision matrix from `docs/architecture.md` §7, pinned by contract
tests, source-doc defects fixed (comment-only). Workstreams B and C are
prototype-and-measure evaluations with written bars; a prototype that misses
its bar is deleted and the measurement recorded in `docs/wrong.md` — never
shipped with a caveat.

**Tech Stack:** Go 1.25+, `github.com/sebishogun/simd` (IndexAll/IndexByte/
CountByte), `encoding/csv` as differential oracle (declared overlap only —
the overlap excludes CRLF-normalization-sensitive quoted data until Task 0
decides), `go test`/`-race`/`vet`, `go test -c` + `go tool objdump` for
disassembly, `perf stat -e instructions:u,cycles:u` for sub-floor changes,
fuzz (Go native), Git.

**Standing rules for every task:**

- TDD: write the failing test first, watch it fail, then implement.
- Gates per task, run bare or with `set -o pipefail` (never through
  `tail`): `go test -count=1 ./...`, `go test -count=1 -race ./...`,
  `go vet ./...`, `git diff --check`.
- Performance tasks: disassemble first; A/B builds interleaved in one
  session, minima, machine idle (load < 1); wall-clock floor 8.3%, below it
  `perf stat` and the disassembly; see `docs/verification.md`.
- **Delete-and-record:** an evaluation that misses its bar is reverted
  completely; the measurement and the reason land in `docs/wrong.md` as a
  new entry with its source. A finding that cost a measurement belongs there
  whether or not code changed.
- Documentation changes are `.md` only; Go changes happen only inside their
  owning tasks (Tasks 0-6; Task 7 is release gates only).
- `docs/wrong.md` holds only sourced measurements.

---

### Task 0 (Stage 0): Quoted-field CRLF normalization policy and corpus case

Closes the current verification gap recorded in `docs/verification.md`: the
differential corpus contains no quoted-field CRLF case, and the declared
overlap excludes CRLF-normalization-sensitive quoted data until this task
decides.

**Files:**
- Modify: `simdcsv_test.go` (one differential case + one contract case)
- Modify: `docs/architecture.md` (§12 divergence 6 gains the decision)
- Modify: `docs/verification.md` (gap closed; overlap definition updated)
- Modify: `docs/roadmap.md`

**Step 1 (TDD).** Add the differential case `"a\r\nb",c\r\n` to
`TestMatchesStdlib` (or its own test): with the current parser this fails —
simdcsv preserves `\r\n` (`["a\r\nb" "c"]`), `encoding/csv` normalizes to
`\n` (`["a\nb" "c"]`), both probe-verified against Go 1.26.5. Watch it fail:
that failure is the recorded gap, made concrete.

**Step 2 — Decide the policy.** Two options, decided with the design's
evidence bar:

- **Preserve** (current behavior): `\r\n` inside a quoted field stays as
  data. Keep the divergence; document it in architecture.md §12 divergence
  6 as the decision; the overlap stays excluding this class forever; the
  differential case from Step 1 becomes a contract case pinned to the
  preserve behavior, and `FuzzOverlap` (Task 2) must keep `\r\n` out of
  quoted fields.
- **Normalize** (stdlib parity): strip the `\r` before a `\n` inside quoted
  fields. This is a behavior change in `quotedRecord`/`recordEnd`; the Step
  1 case becomes a differential parity case, the overlap widens to include
  the class, and a fuzz generator (Task 2) may then emit it.

**Step 3.** Whatever the decision, update the declared-overlap definition
in `docs/verification.md`, `docs/architecture.md` §12, and the agent files'
compatibility sections so all of them name the same rule.

**Definition of Done:** the case is in the suite; the decision is written
with its rationale; every "excludes CRLF-normalization-sensitive quoted
data" sentence in the docs names this task as the decider and reflects its
outcome; all gates green.

**Delete-and-record:** n/a (a policy decision; if the decision is later
reversed, the reversal and its reason are recorded in wrong.md).

---

### Task 1: Malformed-input decision matrix (design + contract tests)

**Files:**
- Create: `malformed_test.go` (package simdcsv)
- Modify: `docs/architecture.md` (§7 table gains a Decision column)
- Modify: `docs/roadmap.md` (mark workstream 1 status)

**Step 1 — Characterization first.** Write table-driven tests that pin the
current behavior for every row of architecture.md §7 (probe-verified rows:
`a"b,c`, `"a"b,c`, unclosed `"a,b`, `"a""b`, lone `"`, `"""`, EOF-mid-quote,
trailing CR at EOF, no trailing newline), both `ReuseRecord` values. Run
them: they pass against the current parser by construction. These are
characterization tests; they make the accident visible, they do not bless
it. (The quoted-field CRLF row is not here: it is a well-formed case in §12
divergence 6, owned by Task 0.)

**Step 2 — Decide each row.** For each row, decide: **accept-as-is**
(quote inside unquoted field, truncated record, unclosed quote — harmless,
matches "reader not validator"), or **accept-with-copy / reject** (the
destructive `"a"b,c` byte-drop and empty-field insertion; `"""` →
`"` which is stdlib-identical only by parity accident). Write the decision
and its one-paragraph rationale into architecture.md §7 as a Decision
column. Where a decision changes behavior, the test from Step 1 changes
with it (red first).

**Step 3 — Fast-path integrity guard.** Add a test asserting that any
record without a quote takes the fast path (alias check, as
`TestFastPathRuns` does) for a set of malformed-but-quote-free inputs —
e.g. `a"b,c` **does** contain a quote and must take the slow path; a record
with `\r` mid-field must stay fast.

**Definition of Done:** every §7 row has a decision and a pinning test; no
"undefined" rows; all gates green; architecture.md updated; the plan's
tracking line for the source-doc defects unchanged (that is Task 3).

**Delete-and-record:** not applicable (contract task; if a decision is
reconsidered later, the reversal is recorded in wrong.md with the reason).

---

### Task 2: Fuzz harness as a gate

**Files:**
- Create: `fuzz_test.go` (package simdcsv)
- Modify: `docs/verification.md` (fuzz section becomes gate text)
- Modify: `docs/roadmap.md`

**Step 1 (TDD):** `FuzzOverlap` — differential fuzz vs `encoding/csv`
within the declared overlap. Generator: well-formed atoms only (plain,
quoted, doubled-quote, embedded-`\n`-newline fields; delimiters from
`{',', ';'}`; CRLF line endings at record level — never `\r\n` inside
quoted fields, per the overlap exclusion unless Task 0 decided normalize;
both `ReuseRecord` values; sizes 0-64
rows, 1-8 cols). Assert: error-parity and `Strings()` equality exactly as
`checkAgainstStdlib`. Seed corpus: every `TestMatchesStdlib` input. Run
5 seconds locally, then `-race` 2 seconds. **Any finding here is a bug and
blocks the task.**

**Step 2 (TDD):** `FuzzNoPanic` — arbitrary bytes, any `Comma` byte value
(the validation-free set), both `ReuseRecord` values; read to EOF within a
small budget. Property: terminates, no panic. Malformed input may produce
any record content — that is contract, not bug.

**Step 3 (TDD):** `FuzzContractMalformed` — generators targeting the §7
rows assert the pinned decisions from Task 1 (e.g. the `"a"b,c` split), so
the decisions stay pinned if the parser changes.

**Step 4:** Document the harness in `docs/verification.md` as part of the
gate list: `go test -fuzz=FuzzOverlap -fuzztime=10s` etc. (times to be
confirmed by a full local run, then recorded).

**Definition of Done:** three fuzz targets, seeded, documented as gates;
no finding outstanding; gates green.

**Delete-and-record:** a differential finding that turns out to be a
*divergence outside the overlap* (generator bug producing a malformed
input) is not a bug: the generator is fixed, and the input class, if new,
is added to the §7 matrix in Task 1's format. Nothing is ever "fixed" by
silently excluding a failing input.

---

### Task 3: Source-doc defect cleanup (comment-only Go change)

**Files:**
- Modify: `simdcsv.go` (comments only — package doc, `Comma`, `Reader`
  struct)
- Modify: `simdcsv_test.go` (the stale "literally delegates" comment)
- Modify: `docs/architecture.md` (§13 becomes history)
- Modify: `docs/roadmap.md` (mark defect-tracking line done)

**Step 1 (TDD — test first, but nothing behavioral can change):** add a
test asserting the *absence* of the ghosts is impossible from outside; the
real guard is `go vet` (doc-link syntax) plus a grep gate:
`grep -n 'ParseInts\|ParseFloats' *.go` must find nothing. Put the grep in
the docs gate list.

**Step 2:** Rewrite the four stale comments to describe the code:
- package doc: drop the `[ParseInts]`/`[ParseFloats]` paragraph (functions
  do not exist; no replacement paragraph is needed — `Record.Strings` +
  strconv is the actual numeric route);
- `Comma`: byte-only, no rune fallback exists, remove the `[NewReader]`
  cross-reference;
- `Reader` struct: quoted records are parsed in-house by `quotedRecord`;
- test file comment: "the quoted path literally delegates to it" →
  "the quoted path is parsed in-house; parity is asserted regardless of
  path".

**Step 3:** `gofmt`, then the full gate list. Behavior must be byte-identical
(the diff is comments only — verify with `git diff --stat` showing no
non-comment hunks).

**Definition of Done:** no stale reference remains; grep gate added to
`docs/verification.md`; all gates green; commit message notes the
comment-only scope.

**Delete-and-record:** n/a.

---

### Task 4: Bounded-memory evaluation (prototype, measure, decide)

**Files:**
- Create: `bounded_eval_test.go` (temporary, deleted by DoD or by
  delete-and-record)
- Create: `bounded_eval.go` behind a build tag or unexported (temporary)
- Modify: `docs/plans/2026-08-13-simdcsv-production.md` (outcome)
- Modify: `docs/wrong.md` (if the bar is missed)

**Step 1 — Design (no code):** chunked read that preserves record
boundaries across chunk edges. The problem is exactly the one `recordEnd`
solves per line: quote parity decides whether a chunk boundary falls inside
a record. Write the design's memory bound and the measurement plan into the
plan doc.

**Step 2 (TDD):** prototype. Correctness first: differential tests vs the
whole-buffer path on the overlap corpus (records must be byte-identical),
then the malformed §7 rows (behavior must match the pinned decisions).

**Step 3 — Measure.** Disassemble first; then A/B against the whole-buffer
path (same machine, interleaved, minima): unquoted 4/16 cols, quoted 4/16
cols, plus a 100 MB file to demonstrate the memory bound (peak RSS via
`/usr/bin/time -v`). Bars (candidates; fix the exact numbers from Step 1's
measurement plan at prototype time, then commit to them):
- peak memory independent of input size (bounded by chunk + record);
- unquoted throughput within 1.2x of whole-buffer;
- fast-path allocation profile unchanged.

**Definition of Done:** all bars met; the prototype is production-shaped
(gates green, race green); decision recorded in the plan doc; a follow-on
task can ship it.

**Delete-and-record:** any bar missed → revert the prototype completely;
record the measurements and the reason in `docs/wrong.md` (new entry,
sourced to the eval's own output). The entry is the deliverable — see
wrong.md's policy: "a finding that cost a measurement belongs there whether
or not any code changed".

---

### Task 5: Streaming-delivery evaluation (prototype, measure, decide) -- DONE (refused, measured)

**Files:** as Task 4 (`stream_eval.*` temporary files; plan doc; wrong.md
if missed).

**Step 1 — Design (no code):** streaming means `Read` returns records before
EOF, which breaks the "fields alias the immutable whole input" contract.
Design options, in increasing cost: (a) deliver records whose fields are
copies once a chunk boundary is passed; (b) keep whole-buffer semantics
behind a separate mode; (c) refuse streaming. Write the ownership contract
change and measurement plan first.

**Step 2 (TDD):** prototype option (a). Differential tests vs the
whole-buffer path on the overlap; §7 rows pinned; `ReuseRecord` semantics
documented for the streaming mode.

**Step 3 — Measure.** Same protocol as Task 4. Bars: throughput within the
Task 4 bound; memory bounded; allocation profile documented (copies at
chunk boundaries are expected — the bar is that they are *measured*, not
zero).

**Definition of Done:** bars met and production-shaped, decision recorded.

**Delete-and-record:** as Task 4. The likely outcome is a documented
refusal with the measurement as the evidence — that is a success for this
evaluation, not a failure.

---

### Task 6: Compatibility option evaluation

**Files:**
- Create: `compat_eval_test.go` (temporary)
- Modify: `docs/architecture.md` §12 (decisions land here)
- Modify: `docs/plans/2026-08-13-simdcsv-production.md`
- Modify: `docs/wrong.md` (if a prototype is deleted)

**Step 1 — Options and expectations (no code).** Per option, write into
the plan doc: what it is, where it touches the scan, expected verdict:
- `TrimLeadingSpace` — per-field subslice trim; composes with the fast
  path; expected: cheap, ships or refuses on API-surface grounds.
- `LazyQuotes` — rewrites the quoted-field machine; expected: refused
  unless the measurement surprises.
- `Comment` — breaks "every delimiter in one pass" (a comment line needs a
  scan of its own); expected: refused.
- Rune delimiters — a multi-byte delimiter needs a substring search per
  record ("costs more than it saves", per the package comment);
  expected: refused.
- `ParseError`-shaped errors — error surface parity; expected: evaluation
  of cost on the hot path (error construction is off the fast path).
- `ReadAll` divergences (`nil` on error; copy under `ReuseRecord`) —
  decision: adopt stdlib shapes or document refusal.
Each expectation is a hypothesis; the measurement decides.

**Step 2 (TDD):** prototype the ones not refused by design (start with
`TrimLeadingSpace` and the `ReadAll` shapes — both are small and
measurable). Differential tests vs stdlib with the option enabled.

**Step 3 — Measure.** Protocol as Task 4; bars per option written in Step
1.

**Definition of Done:** every option has a written decision in
architecture.md §12, each backed by a measurement or a design argument;
shipped options are production-shaped with gates green.

**Delete-and-record:** refused prototypes are deleted; the measurement and
reason recorded in wrong.md. A refused option with its evidence is the
deliverable.

---

### Task 7: Release gate -- DONE (v0.2.0)

**Files:**
- Modify: `README.md` (status, only after facts are pinned)
- Modify: `go.mod`/`go.sum` only if the dependency decision from Task 6 or
  the evaluation tasks requires it (otherwise untouched)
- Create: release notes

**Step 1 — Facts.** Pin: `go.mod` (`simd v1.20.0`, go 1.25.0), tag list
(v0.1.0), the committed benchmark record, the §12 decision list. No text
states a version or number that is not in a pinned file.

**Step 2 — Gates.** Full gate list from `docs/verification.md`: test/race/
vet/`git diff --check`, fuzz targets (Task 2) with recorded time budgets,
links, full-range `.md` read, cross-arch `GOARCH=arm64 go build ./...`
build check.

**Step 3 — Tag and publish** only if every gate is green and the
`main`-vs-tag dependency facts are pinned in the README status section.

**Definition of Done:** a tagged, published release whose README status,
dependency facts, and contract documents all describe the same tree.

**Delete-and-record:** if a gate fails at this point, the failing change is
reverted or fixed by its owning task — never released with a caveat; the
failure and its cause are recorded in wrong.md if they cost a measurement.
