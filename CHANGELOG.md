# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this
package is pre-1.0, so a minor version may change behavior. Every behavior
change below is stated with what it was before, since a reader upgrading needs
the difference and not only the destination.

## v0.2.0

The release that closes the production plan
([docs/plans/2026-08-13-simdcsv-production.md](docs/plans/2026-08-13-simdcsv-production.md)).
Four behavior changes, one new option, three fuzz gates, and two evaluated
designs that did not ship.

### Fixed

- **A quoted `\r\n` now normalizes to `\n`, as `encoding/csv` does.** It was
  preserved before, which meant a field's bytes differed between the two
  packages on well-formed input — silent data corruption for anyone swapping
  this in, and a permanent hole in the differential gate, which had to exclude
  the whole class. The cost is gated: one scan of the record decides whether any
  field can need it, so a record with no CR keeps the zero-copy path.
- **`ReadAll` returns independent records under `ReuseRecord`.** It returned N
  aliases of the last record. `ReadAll` now ignores `ReuseRecord` for its own
  duration, which is what `encoding/csv` does.
- **A malformed record no longer drops bytes.** Junk after a closing quote
  (`"a"b,c`) was discarded; it now joins its field. Every malformed row shape
  has a pinned decision in `docs/architecture.md` §7 rather than being left
  undefined.
- **`recordEnd` no longer conflates "ran out" with "terminator was the last
  byte".** Both returned `len(b)`. Found while prototyping bounded reading,
  where the two cases must be told apart.

### Added

- **`TrimLeadingSpace`**, matching `encoding/csv`: leading unicode whitespace is
  dropped from each field. Off by default, the scan runs per field only when
  set, and a trimmed field is still a subslice — no copy. Whitespace is
  unicode's, as stdlib's is; an ASCII-only rule would trim an NBSP in one
  package and not the other.
- **Three fuzz targets as gates**: `FuzzOverlap` (byte-equality with
  `encoding/csv` on inputs both accept), `FuzzNoPanic`, and
  `FuzzContractMalformed` (a malformed record still yields aliasing fields and a
  reader that continues).
- **A decision for every remaining `encoding/csv` option**, in
  `docs/architecture.md` §12: `LazyQuotes`, `Comment`, rune delimiters and
  `ParseError`-shaped errors are each refused with the reason.

### Evaluated and not shipped

Both are in [docs/wrong.md](docs/wrong.md) with their numbers.

- **Bounded reading** meets every bar — a 64 KiB window parses an 85 MB input at
  0.86x–1.05x of whole-buffer, flat in memory across a 2000x input-size range —
  and still does not ship, because it cannot keep the property the API is built
  on: a record cannot outlive the next `Read`.
- **Streaming delivery with copied records**, which would buy that property
  back, costs 1.45x–1.51x against whole-buffer where the bar was 1.2x, and
  triples the allocation count. A caller who needs a record to outlive the
  buffer copies it, which is the same work applied only where it is wanted.

### Dependency

- `github.com/sebishogun/simd` v1.2.0 → v1.20.0.
- Go 1.25.0 or later.

### Unchanged

The public API of v0.1.0 keeps its names, signatures and meanings.
`TrimLeadingSpace` is the only addition to the `Reader` struct.

## v0.1.0

First tagged release. Whole-buffer reader, `[]byte` fields, one vector scan for
delimiters in quote-free records, separate parser for quoted records, no cgo.
Built on `github.com/sebishogun/simd` v1.2.0.
