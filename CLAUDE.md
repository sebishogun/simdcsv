# CLAUDE.md — working in simdcsv

Instructions for working in this repository. Self-contained; this file is the
boundary, the facts, and the gates.

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
