# simdcsv roadmap

Where the package is, where it is going, and the rule that governs the
going: nothing below exists in the code until a plan task lands it and a
gate passes. **Roadmap items are not shipped features.**

## Where it is

- Pre-1.0 API (`NewReader`, `Reader.Comma` byte / `FieldsPerRecord` /
  `ReuseRecord`, `Read`, `ReadAll`, `Record`). Latest tagged and published
  release: v0.1.0 (`simd v1.2.0`, go 1.25.0). `main` runs `simd v1.20.0`,
  untagged.
- Fast path (unquoted records): one vector scan per record, zero-copy
  fields, measured 1.92x / 1.49x over `encoding/csv` on 16/4 unquoted
  columns (Zen 5, 20,000 rows; committed record — `docs/wrong.md` entry 1
  cites the sources).
- Quoted records parse in-house; fully-quoted input runs at 0.81x. The
  alternatives tried (delegation, skipping SIMD on short spans) were
  measured and rejected (`docs/wrong.md` entries 2-4).
- The whole input is buffered on the first `Read`; fields alias it unless
  unescaped. The full contract, including the empirically established
  malformed-input behavior and the exact `encoding/csv` divergences, is in
  `docs/architecture.md`; the source-doc defects it lists (§13) are unpaid
  debt.

## Direction

A focused production CSV reader. Production means: contracts that hold for
malformed input, a documented and tested compatibility boundary, verified by
fuzz and differential tests, with bounded/streaming and compatibility
options **decided by measurement, not promise**. Three workstreams:

### 1. Harden malformed-input safety and contracts

The parser never errors on input content today: every malformed quote form
is accepted with its own splits, some silently destructive (`"a"b,c` drops
a byte and inserts an empty field). Before anything else is built, this
behavior must be a *decision*, not an accident:

- a decision matrix over every malformed class in `docs/architecture.md` §7
  (accept-as-is / accept-with-copy / reject like stdlib), with a rationale
  per row;
- contract tests pinning each decision (TDD; the behaviors already exist, so
  these are characterization tests first, then decisions);
- the four stale source-doc defects (architecture.md §13) fixed as part of
  the same contract work;
- a fuzz harness with the documented divergence classes carved out.

Bar: every malformed input class has a tested, documented contract; no
"undefined" rows remain.

### 2. Evaluate bounded/streaming options

The whole-buffer model is the constraint everything is built on. The
question is not "should it stream" but "what is the cheapest way to get a
memory bound", evaluated on evidence:

- **Bounded read:** feed the vector scan from bounded chunks while keeping
  record boundaries correct (a record spanning a chunk boundary must still
  parse — quote parity makes this a real design problem, exactly the one
  `recordEnd` already solves per line).
- **Streaming record delivery:** deliver records as the input arrives
  instead of after the whole file; changes `Read`'s ownership contract, so
  it is an API-level evaluation, not a patch.
- Each option gets a design in `docs/plans/`, a prototype, and a measured
  bar (e.g. within a stated factor of the whole-buffer path on unquoted
  input; a memory bound independent of input size; unchanged fast-path
  allocation profile).

**Delete-and-record:** an option that misses its bar is deleted and recorded
in `docs/wrong.md` with its measurement. Shipping a slower-for-everyone
streaming mode because streaming is fashionable is the failure mode this
rule exists for.

### 3. Evaluate compatibility options

- `LazyQuotes`, `TrimLeadingSpace`, `Comment`, `ParseError`-shaped errors,
  rune delimiters, and the two `ReadAll` divergences (`nil` on error; copy
  under `ReuseRecord`).
- Each is an evaluation with a design and a measurement bar — including the
  cost of *not* doing it (documented divergence). Some are expected to be
  refused: `Comment` and `LazyQuotes` change the scan (a comment line
  breaks the "every delimiter in one pass" assumption; `LazyQuotes` rewrites
  the quoted-field machine). Refusal with a documented reason is a valid
  outcome of an evaluation.
- Bar: a written decision per option in the plan, with the measurement or
  design argument that produced it; decisions land in `docs/architecture.md`
  §12.

### Release hygiene

- The next tag needs: dependency facts pinned (`simd v1.20.0`), the source-
  doc defects fixed, the fuzz harness in the gate list, and the decision
  matrix complete. Pre-1.0 means the API may still change; the *contracts*
  change only through the same decision process.

## Non-goals (held)

- CSV *validation* (rejecting malformed input for its own sake) — the
  package reads; the decision matrix is about what reading malformed input
  means, not about becoming a linter.
- Full `encoding/csv` parity — the overlap is declared, the divergences are
  documented; parity is only evaluated where it has a measurable cost or
  benefit (workstream 3).
- Any API addition before workstream 1 is done.
- Performance promises outside amd64 and wall-clock regression gates.

## What is not in this document

No dates, no version promises, no feature promises. The plan documents in
`docs/plans/2026-08-13-simdcsv-production-*.md` are the executable form of
this roadmap; this file is the policy that governs them. A roadmap entry
that has not gone through a plan task, a gate, and a commit does not exist
in the code — and a reader of `simdcsv.go` should never have to know this
file exists.
