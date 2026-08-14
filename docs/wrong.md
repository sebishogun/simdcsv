# simdcsv: measured dead ends

Every entry here is a direction that was tried, measured, and rejected — or,
in one case, kept despite the measurement. The rule: **only sourced
measurements** appear here. Each entry cites where its number lives (package
doc comment, code comment, or the README table). The numbers were not
re-measured on this machine; they are the committed record, and a reader
should be able to check the source line.

A measurement that argues against a change belongs in this file whether or
not any code changed. An unsourced number belongs nowhere.

---

## 1. Fully-quoted input is slower than encoding/csv — 0.81x

**Tried.** Nothing to try: this is the shipped behavior and it is in the
contract.

**Measured.** Zen 5, 20,000 generated rows, `ReuseRecord` on both sides
(`simdcsv.go:33-46`; the ms table is in `README.md`):

| input | `encoding/csv` | `simdcsv` | ratio |
|---|---:|---:|---:|
| 16 columns, unquoted | 3.54 ms | 1.84 ms | 1.92x |
| 4 columns, unquoted | 1.06 ms | 0.71 ms | 1.49x |
| 16 columns, quoted | 3.74 ms | 3.50 ms | 1.07x |
| 4 columns, quoted | 1.10 ms | 1.35 ms | **0.81x** |

**Why.** A file where every field is quoted gets no vector scan at all: the
delimiters are inside quotes, so they have to be walked, and the package is
then doing the standard library's work with a layer on top. Narrow records
make it worse because the per-field overhead amortizes over less data.

**Conclusion.** The loss is part of the contract and stays; two attempts to
remove it made things worse (entries 2-4).

## 2. Handing quote-heavy files to encoding/csv wholesale — ~0.38x

**Tried.** Route every record containing a quote to a single shared
`encoding/csv` reader.

**Measured.** 0.38x (`simdcsv.go:48-49`): its `[]string` records have to be
copied into `[][]byte`, and the package ends up paying the standard
library's parse cost plus a copy on top.

**Outcome.** Rejected. Not in the code.

## 3. Delegating quoted records one at a time — 4700x

**Tried.** The earlier quoted-record design: construct a `csv.Reader` (and
the `bufio.Reader` inside it) for every quoted record.

**Measured.** 4700x slower than the standard library it is meant to beat
(`simdcsv.go:251-255`): a reader allocation per record.

**Outcome.** Replaced by the in-house `quotedRecord` parser — "about forty
lines and none of that" (the source comment's words). This is why the
delegation comment in the `Reader` struct doc is stale (architecture.md
§13).

## 4. Skipping the SIMD call on short spans — 1.30x

**Tried.** Skip the vector scan for spans too short to amortize it, on the
assumption the fast path's short records would not care.

**Measured.** 1.30x **on the unquoted case it was meant not to affect**
(`simdcsv.go:50-52`).

**Outcome.** Rejected. Not in the code. The 0.81x of entry 1 is the shipped
number.

## 5. One whole-buffer IndexAll per record (the quadratic first design) — 17 s

**Tried.** The first version ran `IndexAll` over the entire remaining
buffer to find every quote and newline in the file — once per record.

**Measured.** Quadratic: a 20,000-row file took 17 seconds against the
standard library's 3.6 milliseconds (`simdcsv.go:223-227`).

**Outcome.** Replaced by `recordEnd`, which walks one physical line at a
time and tracks quote parity, so each record costs O(its own length). The
`qidx`/`nidx` fields the old design used are still declared and unused
(architecture.md §14).

---

## What is not in this file

The 0.38x, 1.30x and 17 s measurements are sourced to the package's own
comments and were not re-run here; the 0.81x table is the README's committed
record. Behavioral divergences from `encoding/csv` (malformed quotes,
`ReadAll` shapes, delimiter validation) are not dead ends — they are the
documented contract, in `docs/architecture.md` §7 and §12. Future
evaluations that fail their bars (roadmap workstreams 2-3) get entries here
with their measurements, per the delete-and-record rule in
`docs/plans/2026-08-13-simdcsv-production.md`.

## `ReadAll` under `ReuseRecord` returned the same record N times

**Believed.** `ReuseRecord` is a documented performance flag: the caller
opts in, and the documentation says not to retain a record past the next
`Read`. Under `ReadAll` it was described as returning the final record for
every entry — written down, so treated as understood.

**Actually.** That is not a performance trade a caller can opt into; it is
silent data loss with no error attached. Three records in, the same record
three times out:

    in:  "a,1\nb,2\nc,3\n"
    got: [["c" "3"] ["c" "3"] ["c" "3"]]
    encoding/csv: [["a" "1"] ["b" "2"] ["c" "3"]]

`ReuseRecord` is sound in the loop it was designed for — consume each
record before asking for the next — and unsound the moment every record is
kept, which is precisely what `ReadAll` does. The flag and the method
cannot both be honoured.

**How it surfaced.** Task 6 of the production plan lists the `ReadAll`
divergences as options to decide. Probing the two before deciding showed
this one is not a divergence to document but a defect to fix.

**Source.** `simdcsv.go` `ReadAll`; `encoding/csv` copies for the same
reason.

**Consequence.** `ReadAll` clears `ReuseRecord` for its own loop and
restores the caller's setting on return, so it yields independent records
whatever the flag says, and `Read` still reuses when asked. Both are
pinned: one test reads three records under both settings, another checks
that a `Record` handed out by `Read` is overwritten by the next `Read`,
reading the field header after the second call rather than before, since a
copy taken beforehand still points at the old bytes and would pass either
way.

The other `ReadAll` divergence — records parsed before an error are
returned with it rather than discarded — is kept. It hands back what the
input contained and the error says where it stopped, which is this
package's posture; it is documented, not silent.

## Bounded reading: all three bars met, after one line moved

**The evaluation.** The whole-buffer path reads the input with
`io.ReadAll`, so peak memory is the file, and inputs larger than memory
are the one thing `encoding/csv` can do that this cannot. Plan task 4
asked for a chunked prototype with three bars: peak memory independent of
input size, unquoted throughput within 1.2x of whole-buffer, and the
fast-path allocation profile unchanged.

**The first prototype missed the throughput bar by a mile**: 3.37x slower
on unquoted 4-column, 2.55x on quoted 4-column. Not the parsing -- the
compaction. It dropped delivered bytes before every record, which is a
64 KiB move per 20-byte record. Compaction only has to happen before the
buffer grows, so it moved into `fill`, and the same code became:

    shape              whole      bounded    ratio
    unquoted 4-col    1.62 ms     1.60 ms    0.99x
    unquoted 16-col   4.07 ms     3.63 ms    0.89x
    quoted 4-col      2.54 ms     2.18 ms    0.86x
    quoted 16-col     6.91 ms     6.71 ms    0.97x

Bounded is level or faster, and the reason is the memory bound itself: a
64 KiB window stays in cache where a 4 MB buffer streams through it. Runs
on a loaded machine put the ratio between 0.86x and 1.05x, so the bar is
met in either direction.

**Memory**, 85 MB input, 2.5M records:

    bounded:  buffer cap     64 KB,  heap grew  55 MB
    whole:    buffer cap 87,896 KB,  heap grew 225 MB

The buffer is flat at the chunk size across a 2000x range of input sizes.
The 55 MB the bounded path still grows is the retained records themselves,
not the buffer.

**Allocations**: 20,011 against 20,028 per run, and bounded allocates
fewer *bytes* -- 2.21 MB against 3.28 MB -- because it never allocates the
whole-file buffer.

**What it costs, and it is not nothing.** Records cannot outlive the next
`Read`. The whole-buffer path returns fields aliasing a buffer that never
moves; a bounded buffer is compacted and refilled under them. So bounded
records carry `encoding/csv`'s contract and a caller who keeps one copies
it. That is what bounded memory costs, and it is pinned by a test rather
than left to be discovered.

**A defect the prototype found in the shipped code.** Deciding whether a
buffer holds a complete record cannot be done by looking for a trailing
newline: that newline may be inside a quoted field. `recordEnd` returned
`len(b)` both when it ran out of buffer and when the terminator was the
final byte, so the two were indistinguishable from the offset alone. It
now reports which, in one walk -- a second implementation of the
quote-parity rule is how two answers to "where does this record end" would
come to disagree.

**Decision: adopt.** All three bars met, correctness differential-tested
against the whole-buffer path at every chunk size from 1 upward and across
300 generated documents. Exporting it is a follow-on: API surface is a
decision, and this entry is a measurement.

## Streaming delivery with copied records: 1.6x and 3x the allocations, rejected

**The evaluation.** Task 4's bounded reader streams -- it returns records
before EOF -- but it gives up the property the package is built on:
fields alias the whole input, so a record outlives the reader. Under a
bounded buffer they cannot, because the buffer is compacted and refilled
under them. Plan task 5 asked whether that property can be bought back by
copying each record out as it is delivered, held to task 4's bars.

**Prototyped and measured.** A `copyOut` mode on the bounded reader, one
allocation for the joined field bytes plus one for the slice headers, per
record:

    shape              whole      bounded   +copies    vs whole
    unquoted 4-col    1.67 ms     1.52 ms   2.43 ms      1.45x
    unquoted 16-col   4.36 ms     4.09 ms   6.58 ms      1.51x

    allocations       20,030      20,013    60,013         3.0x

Against the non-copying bounded path it is 1.60x and 1.61x. The bar was
1.2x of whole-buffer, so it misses in both shapes, and the allocation
count triples because two allocations per record are added to a path that
had one.

**Deleted, and the reason is not only the ratio.** The mode copies every
record, and a caller streaming a large file typically keeps few -- filter
first, retain the survivors. The same copy done caller-side is the same
work applied only where it is wanted, and it needs no API. What ships is
task 4's shape with the contract change stated: under bounded reading, a
record is valid until the next `Read`, and a caller who needs one to
outlive that copies it.

The measurement stands as the reason there is no streaming mode with
whole-buffer semantics: it is not that it cannot be built, it is that it
costs 1.6x to build it for everyone instead of 1.6x for the records that
need it.
