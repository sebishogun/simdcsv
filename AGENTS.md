# simdcsv — agent guidance

Self-contained orientation for agents working in this repository. Read this
before anything else: it is the boundary, the facts, and the gates. It applies
to the whole repository and does not depend on any other file.

## Core tenets: performance-aware programming

**These are the core tenets of this codebase. Read them before writing a line.**

The stance is Casey Muratori's: *performance-aware programming*. Not
"optimization" as a phase that happens after the code works — knowing roughly
what the machine will do with what you write, while you write it. The
alternative is not "clean code that gets optimized later"; it is code whose
shape forecloses the fast version, and the rewrite costs more than thinking for
five minutes did.

Two ideas underneath everything below:

- **Know the order of magnitude before you type.** How many times does this run
  — once, per request, per row, per element? What does one iteration touch?
  Nobody needs a cycle count; everybody needs to know whether they just wrote
  something that runs 200,000 times and allocates.
- **The machine is not an abstract machine.** It has caches, a prefetcher, wide
  registers, and many cores. Code that pretends otherwise leaves 10-100x on the
  floor, and no amount of later profiling recovers a layout decision.

**How the tenets relate.** They are not a list of independent good ideas. The
data-layout ones exist to make the bulk operation POSSIBLE:

    struct-of-arrays + grouped lifetimes + zero per-element allocation
        -> contiguous, uniformly-typed arrays
            -> the kernel can run at all
                -> SIMD, and the parallel shard boundaries come free

You cannot vectorize an array-of-structs: the lanes are not adjacent. You
cannot vectorize a slice that is really a graph of separately-allocated
objects. You cannot keep a kernel fed if every element costs an allocation.
So struct-of-arrays, arenas and lifetime grouping are not housekeeping to do
after the fast path works — they are the precondition for the fast path
existing, and the reason a layout decision made carelessly cannot be recovered
by profiling later.

Read the sections in that order, and design in that order.

### 1. Zero allocations wherever it is possible at all

Not "few" — zero, on any path that runs per element, per record, per row or per
request.

The checklist, in the order it usually pays:

- **Nothing per-element or per-record that can be per-batch.** A `map` built
  per record, a `fmt.Sprintf` per line, a `[]byte`->`string` per field: at 200k
  records those are 200k allocations and 200k pieces of GC work. Reach for a
  byte scan over the fixed shape instead of a reflective decode into a map.
- **Size every slice and map you can size.** `make([]T, 0, n)` when n is known
  or estimable. Growing from nil reallocates and copies the whole thing at
  every doubling.
- **Reuse the caller's buffer.** Append into a supplied `[]byte`, compact in
  place when the write cursor provably trails the read cursor, take a `dst`
  parameter rather than returning a fresh allocation.
- **Do not scan twice.** If a later stage already parses the data, do not
  validate it fully first — do the O(1) structural check and let the one place
  that parses report the rest.
- **Escape analysis is part of the design.** A pointer stored in an interface,
  a closure capturing a local, a returned slice of a local array: each forces a
  heap allocation. `go build -gcflags=-m` says which.
- **Prefer a wider type to a pointer chase.** An index into a slab beats a
  pointer when the slab is contiguous — it is smaller, it does not escape, and
  it keeps the array vectorizable.

Verify with `-benchmem`. `0 allocs/op` is a target you can actually hit on a
hot path, and worth stating in the doc comment when you hit it.

### 2. Think about the data, then the code

Muratori's central point, and the one most often skipped. The layout of the
data decides the speed; the instructions are usually a detail.

**Struct-of-arrays over array-of-structs** for anything scanned columnwise. A
filter that reads one field should stream that field's array, not stride
through whole records pulling in fifteen fields it does not want. This is the
single highest-leverage decision in a columnar store, and it is made when the
type is declared, not later.

**Group lifetimes; allocate them together.** Objects born together and dying
together belong in one allocation. A per-request arena — one buffer that
everything for that request is carved out of, released in one move when the
request ends — replaces thousands of individual allocations and frees with a
single pointer reset. It also gives locality for free: everything the request
touches is contiguous. Where the lifetime is per-batch, per-group or
per-connection instead, the same applies at that scope. The rule is that the
allocation boundary should match the lifetime boundary; when it does not, you
get either leaks or a per-object free.

**Use the whole cache line.** Touch it once and consume all of it. Block a pass
to fit L1/L2 rather than striding across a large array repeatedly. Keep hot
fields adjacent and cold fields elsewhere so a line carries only what the loop
reads. Watch for false sharing when threads write adjacent words.

Locality is a hypothesis to check with perf counters, not a rule to apply
blindly: windowing won in simdcsv and did nothing in simdjson.

### 3. Do the work in bulk — use SIMD

This family exists for it. Whole-slice work goes through the kernels, not a
hand-written scalar loop. Where no kernel exists for the shape, say so
explicitly rather than quietly writing the scalar loop and leaving it.

Check the dispatch actually reaches the kernel at runtime: every complex kernel
in `simd` was dead code from v1.14.0 to v1.20.0 because nothing walked the
tables the runtime indexes.

A per-element function call defeats vectorization outright — measured at 11
extra instructions per element, a 2.56x ratio. If the API shape forces one, the
API shape is the bug.

### 4. Don't do the work at all

The fastest code is the code that does not run. Prune before you decode: a
bloom filter that rejects a group, a time window that skips a block, a column
never materialized because nothing asked for it. simdlogs' rare-needle path
beats a full scan by rejecting groups without decoding them, not by decoding
faster.

Hoist invariants out of loops. Compute once what does not change. Do not scan
twice — if a later stage already parses the data, do the O(1) structural check
and let the one place that parses report the rest.

### 5. Multi-threaded where it is beneficial

And only there. Parallelism pays when the work per shard clears the
coordination cost; below that it is slower and less predictable.

Shard on a boundary the data already has (groups, blocks, row ranges), give
each worker its own output buffer, merge once. Never share a mutable buffer
between goroutines without saying so in the doc comment. `-race` is a gate.

### 6. `sync.Pool` is the last resort — and it has to be correct

Reach for it last. Most allocation wins are a size hint, an arena, or a
caller-supplied buffer: free at runtime, no correctness hazard. A pool costs
Get/Put, a miss allocates anyway, and it introduces a class of bug the others
cannot have.

When a pool IS the right answer, these are not optional:

- **The buffer must be fully overwritten before anything reads it.** A pooled
  buffer arrives holding a PREVIOUS request's data. If any path reads an
  element it did not write, that request's data is silently served to this one
  — a correctness bug, cross-request data leakage, not a performance one. Know
  the property holds and say WHY in the doc comment; do not assume it.
- **Prove it with a poisoning test.** Fill pooled buffers with a value that
  cannot occur, then assert the pooled result equals the unpooled result
  exactly. Write that test FIRST. This is the only thing that catches the bug,
  because the unpooled path zeroes and therefore hides it.
- **Ownership must be unambiguous.** A pooled buffer must not escape into a
  returned value, be captured by a goroutine that outlives the Put, or be
  aliased by a slice the caller keeps. Returning a slice of a pooled array is a
  use-after-free in all but name.
- **Put back exactly what you took**, reset to a known state, once. A double
  Put hands the same array to two callers at the same time.
- **Pool a pointer, not a slice.** A `[]T` placed in an `any` allocates on
  every Put, which is the cost the pool exists to remove.
- **Sizing is part of the contract.** A pool of mixed sizes either wastes the
  large buffers or reallocates on the small ones; decide which and say so.
- **Testing note:** `sync.Pool.Put` drops the value at random one time in four
  under `-race`, so any test asserting reuse across a single round trip is red
  a quarter of the time. Assert reuse within a few attempts, not on a
  particular one.

### Then measure

These tenets are where to start, not a substitute for the benchmark.
Fast-looking code that was never measured is a guess. The noise floor, the
interleaved A/B discipline, and "disassemble before you theorise" apply to code
written this way exactly as they apply to a tuning change — and a claim with no
number behind it does not go in a doc.

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
4. `docs/verification.md` — gates, differential-test rules, measurement methodology.
5. `docs/wrong.md` — the measured dead ends (do not re-try them).
6. `docs/roadmap.md`, `docs/plans/` — where it is going; none of it is shipped.
7. `simdcsv.go`, `simdcsv_test.go` — the source of truth.

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
  its `Record` or `Fields()` value. `ReadAll` ignores the flag and returns
  independent records, matching `encoding/csv`: it keeps every record it
  returns, so reuse there produced a slice whose entries all aliased the last
  record parsed.
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
- **quoted-field CRLF normalization: parity, no longer a divergence.**
  `\r\n` inside a quoted field is reduced to `\n` by both packages, and a
  lone `\r` is preserved by both. Decided by Task 0/Stage 0 of the plan;
  six cases are in the differential corpus;
- stdlib rejects `Comma` equal to `0`, `'\r'`, `'\n'`, `'"'`, `utf8.RuneError`
  (or any non-UTF-8 rune), and equal to its `Comment` field when configured;
  simdcsv accepts any byte;
- stdlib `ReadAll` returns `nil, err` on the first error; simdcsv returns the
  partial records;
- stdlib `ReadAll` with `ReuseRecord` allocates each record freshly; simdcsv's
  entries all alias the last one.

Differential tests against `encoding/csv` may only run within the declared
overlap (well-formed input). Malformed-input behavior is a documented contract of its own, tested
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
