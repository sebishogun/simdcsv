# simdcsv production plan — design

The design behind `docs/plans/2026-08-13-simdcsv-production.md`. This file
answers *why* the plan is shaped the way it is; the plan file is the
executable task list.

## Goal

Make simdcsv a production CSV reader: contracts that hold for malformed
input, a documented and tested compatibility boundary, verified by fuzz and
differential tests, with bounded/streaming and compatibility options
**decided by measurement, not promise**. No feature is added because it
sounds right; every option gets a design, a prototype, and a measured bar,
and an option that misses its bar is deleted and recorded in
`docs/wrong.md`.

## Current state (the snapshot the plan works from)

- Public surface: `NewReader`, `Reader.Comma` (byte) / `FieldsPerRecord` /
  `ReuseRecord`, `Read`, `ReadAll`, `Record.{Len,Field,Fields,String}`.
  Pre-1.0. v0.1.0 tagged (`simd v1.2.0`); `main` on `simd v1.20.0`, untagged.
- Whole input buffered on first `Read`; fields alias it; doubled-quote
  fields are fresh copies (double-copied under `ReuseRecord=false`, an
  observed quirk — architecture.md §9).
- Fast path: one vector scan per record; committed Zen 5 record 1.92x /
  1.49x / 1.07x / 0.81x (wrong.md entry 1).
- Malformed input: never errors; each class parses with its own splits
  (architecture.md §7, probe-verified). `encoding/csv` divergences pinned in
  architecture.md §12.
- Known source-doc defects: `ParseInts`/`ParseFloats` ghosts, rune-fallback
  cross-reference, two "delegates to encoding/csv" comments (architecture.md
  §13).
- Vestigial state: `qidx`, `nidx`, `unq` (architecture.md §14).

## The constraint that shapes everything

The vector scan needs the whole record in memory; the package reads the
whole input up front and returns zero-copy subslices of it. Every production
option is evaluated against this model, not around it:

- A **bounded** reader must keep the "find every delimiter in one pass"
  property or pay for losing it — measured, not assumed.
- A **streaming** reader changes the ownership contract (`Read` returning
  subslices of a rotating buffer is the one thing this package promises not
  to do), so it is an API-level evaluation, not a patch.
- A **compatibility** option either composes with the scan (`TrimLeadingSpace`
  is a per-field subslice trim — cheap) or fights it (`Comment` and
  `LazyQuotes` change what a delimiter/quote means, which is the exact thing
  the fast path assumes is unambiguous).

## Workstreams

### A. Malformed-input contracts (first, and gating everything else)

The parser's behavior on malformed input is currently an accident of the
state machines, documented only after the fact in architecture.md §7. The
plan turns each row into a *decision* with a contract test:

- **Accept-as-is** (quote inside an unquoted field, truncated record at
  EOF, unclosed quote running to end of record) — harmless to fast path,
  matches the "reader, not validator" stance.
- **Accept-with-copy or reject** (`"a"b,c` dropping a byte and inserting an
  empty field is destructive; `"""` → `"` is stdlib-identical but only by
  accident of parity) — each row argued on safety and cost.
- The four stale source-doc defects are fixed in the same workstream
  (comment-only Go changes, behavior untouched).

Bar: every §7 row has a decision and a pinning test; no "undefined" rows.

### B. Bounded/streaming evaluation

Two designs, prototyped and measured, each with a written bar:

1. **Bounded read:** chunk the input, keep records intact across chunk
   boundaries (quote parity per line is already solved by `recordEnd`).
   Bar: memory bound independent of input size; unquoted throughput within
   a stated factor of whole-buffer (candidate: 1.2x headroom, to be fixed
   at prototype time from measurement); fast-path allocation profile
   unchanged.
2. **Streaming delivery:** records available before EOF. Bar: same
   throughput bound; ownership contract documented as a breaking change
   (pre-1.0 makes it legal); `ReadAll` semantics unchanged.

**Delete-and-record:** a prototype that misses its bar is deleted; the
measurement goes into `docs/wrong.md` with its source. Shipping a slower
streaming mode because streaming is expected is the failure mode this rule
exists for.

### C. Compatibility evaluation

Per option (`LazyQuotes`, `TrimLeadingSpace`, `Comment`, rune delimiters,
`ParseError`-shaped errors, `ReadAll` divergence decisions): a design, a
prototype, a measurement bar, and a written decision. Refusal with a
documented reason is a valid outcome — `Comment` and `LazyQuotes` break the
fast-path assumption and are expected to be refused unless the measurement
surprises. Decisions land in architecture.md §12.

## Test strategy

- Differential tests stay within the declared overlap (well-formed input
  only); malformed classes are pinned by their own contract tests.
- Fuzz: overlap differential fuzz; arbitrary-bytes panic/hang fuzz; contract
  fuzz over the §7 generators. Details in `docs/verification.md`.
- `TestFastPathRuns`-style aliasing assertions accompany any change that
  touches ownership.
- Every change runs the full gate list (`docs/verification.md`): test,
  race, vet, `git diff --check`, links, full-range read.

## Documentation strategy

- README stays the surface: navigation, API, committed performance table,
  status. It gains nothing that belongs in the deep docs.
- architecture.md carries the contract (including the decision matrix once
  workstream A lands it); lld/reader.md tracks the parser as it changes;
  wrong.md gains the delete-and-record outcomes; verification.md gains the
  fuzz harness as a gate.
- The known source-doc defects have a tracking line in the plan until fixed,
  and a gate note so they cannot reappear in new text.

## Non-goals (this plan does not do)

- CSV validation as a feature (rejecting malformed input for its own sake).
- Full `encoding/csv` parity (the overlap is declared; divergences are
  documented).
- Any API addition before workstream A is complete.
- New performance claims without the A/B protocol (verification.md).

## Risks

- **The int32 record bound** (architecture.md §8): a >2 GiB single record
  cannot be indexed. Out of practical scope; documented, not fixed.
- **Double copy of unescaped fields** (§9): a real allocation waste that
  workstream A or B may touch; whichever does must measure it.
- **Vestigial state** (§14): `qidx`/`nidx`/`unq` invite "use the scratch"
  refactors; the plan only removes them as part of a change that measures.
- **Stale comments** (§13): the `Reader` struct comment still says quoted
  records are delegated to `encoding/csv`, which was removed because it
  measured 4700x slower (wrong.md entry 3). The README labels the delegation
  attempts correctly as rejected; the struct comment does not.
