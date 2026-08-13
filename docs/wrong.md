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
