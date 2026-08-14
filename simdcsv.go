// Package simdcsv reads CSV faster than encoding/csv by finding every
// delimiter in a buffer at once instead of one byte at a time.
//
// The shape is the standard library's:
//
//	r := simdcsv.NewReader(f)
//	for {
//		rec, err := r.Read()
//		if err == io.EOF {
//			break
//		}
//		...
//	}
//
// # How it is faster, and when it is not
//
// encoding/csv walks the input a byte at a time, deciding at each one whether
// it is a delimiter, a quote, a newline or data. That is a dependent branch per
// byte and it is what the time goes on.
//
// This finds all of them at once. [simd.IndexAll] scans a whole buffer with a
// vector compare and writes the position of every match, so splitting a record
// into fields is one pass with no branch per byte, and the fields come out as
// subslices of the original buffer rather than as copies.
//
// That only works while the input is unambiguous. A quote changes what the
// bytes after it mean — a comma inside a quoted field is data, and so is a
// newline — so a record containing one is parsed by the careful path instead.
// The split is per record, so a file where a few fields are quoted still runs
// mostly on the fast path, and a file where every field is quoted runs entirely
// on the slow one and is no faster than the standard library.
//
// # When it is faster, measured
//
// Zen 5, 20,000 rows, against encoding/csv with ReuseRecord on both sides:
//
//	16 columns, unquoted   1.92x
//	 4 columns, unquoted   1.49x
//	16 columns, quoted     1.07x
//	 4 columns, quoted     0.81x
//
// The last line is the honest one. A file where every field is quoted gets no
// vector scan at all — the delimiters are inside quotes, so they have to be
// walked — and this package is then doing the standard library's work with a
// layer on top. Narrow records make it worse, because the per-field overhead is
// amortised over less data.
//
// Two things were tried to remove that and both made it worse. Handing a
// quote-heavy file to encoding/csv wholesale measured 0.38x, because its
// []string records have to be copied into [][]byte. Skipping the vector call
// for short spans measured 1.30x on the unquoted case it was meant not to
// affect. Neither is in the code; the 0.81x is.
//
// # Numbers
//
// A record is returned as [][]byte pointing into the reader's buffer rather
// than as []string. Converting is a copy per field and the standard library has
// to do it because its buffer is reused; here the caller can decide. [Record]
// has Strings if a copy is what you want.
//
// For numeric data, [Record.Strings] with strconv is the route; this package
// ships no column converters.
package simdcsv

import (
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"github.com/sebishogun/simd"
)

// Reader reads records from a CSV-encoded stream.
//
// The zero value is not usable; call [NewReader].
type Reader struct {
	// Comma is the field delimiter. It defaults to ','.
	//
	// Unlike encoding/csv this is a byte rather than a rune: the vector scan
	// compares bytes, and a multi-byte delimiter would need a substring search
	// per record, which costs more than it saves. A multi-byte delimiter is
	// not supported. Unlike encoding/csv no value is rejected either -- any
	// byte, 0 included, is used as written.
	Comma byte

	// TrimLeadingSpace drops the leading whitespace of every field, as
	// encoding/csv's option of the same name does. Trailing whitespace is
	// kept, and whitespace inside a quoted field is data; a space before an
	// opening quote is trimmed and the field still parses as quoted.
	//
	// Off by default, and it costs nothing when off: the scan runs per field
	// only when it is set, over the leading whitespace alone. Trimming keeps
	// the field a subslice, so the zero-copy property survives it.
	TrimLeadingSpace bool

	// ReuseRecord makes Read return a Record backed by memory that the next
	// call overwrites, which is what makes it allocation-free.
	//
	// Default false, matching encoding/csv: every Read returns a Record that
	// stays valid. Set it only in a loop that consumes each record before
	// asking for the next — the first version of this package reused
	// unconditionally, and every record returned by ReadAll was the last one.
	ReuseRecord bool

	// FieldsPerRecord behaves as it does in encoding/csv: zero means the first
	// record sets the count, a positive value requires exactly that many, and a
	// negative value allows any number.
	FieldsPerRecord int

	src    io.Reader
	buf    []byte // the whole input, read up front
	pos    int    // where the next record starts
	idx    []int32
	qidx   []int32  // quote positions, reused across records
	nidx   []int32  // newline positions
	fields [][]byte // reused; see Read
	unq    []byte   // scratch for unescaping doubled quotes
	num    int      // fields in the first record, once seen
	line   int

	// Records the fast path cannot take -- anything containing a quote -- are
	// parsed in-house by quotedRecord, which handles doubled quotes, CRLF
	// normalization, and the malformed cases in architecture.md section 7.
}

// NewReader returns a Reader reading from r.
//
// The whole input is read into memory on the first call to [Reader.Read]. That
// is the trade this package makes: finding every delimiter at once needs a
// buffer to find them in. For a stream too large to hold, use encoding/csv.
func NewReader(r io.Reader) *Reader {
	return &Reader{Comma: ',', src: r}
}

// Read returns the next record.
//
// The returned [Record] points into the reader's buffer and stays valid until
// the reader is garbage collected — unlike encoding/csv, which reuses its slice
// and requires a copy if a record is kept.
func (r *Reader) Read() (Record, error) {
	if r.buf == nil {
		b, err := io.ReadAll(r.src)
		if err != nil {
			return Record{}, err
		}
		r.buf = b
	}
	// A blank line is not a record — encoding/csv skips them and so must this,
	// or a file with a trailing newline gains a phantom empty record at the end
	// and any interior blank line shifts every count after it.
	var line []byte
	var next int
	var quoted bool
	for {
		if r.pos >= len(r.buf) {
			return Record{}, io.EOF
		}
		line, next, quoted = r.nextLine()
		r.line++
		if len(line) > 0 {
			break
		}
		r.pos = next
	}
	if quoted {
		rec, err := r.slowRecord()
		if err != nil {
			return Record{}, err
		}
		return rec, r.checkCount(len(rec.fields))
	}
	r.pos = next

	fields := r.split(line)
	return Record{fields: r.own(fields)}, r.checkCount(len(fields))
}

// nextLine returns the bytes of the next record, the offset to continue from,
// and whether it contains a quote.
//
// A quote is looked for first because it decides everything else: without one,
// a newline always ends the record, and with one it may not.
func (r *Reader) nextLine() (line []byte, next int, quoted bool) {
	rest := r.buf[r.pos:]
	end := simd.IndexByte(rest, '\n')
	if end < 0 {
		end = len(rest)
		next = len(r.buf)
	} else {
		next = r.pos + end + 1
	}
	line = rest[:end]
	// A trailing carriage return belongs to the line ending, not the last field.
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if simd.IndexByte(line, '"') >= 0 {
		return line, next, true
	}
	return line, next, false
}

// split cuts a quote-free record into fields.
//
// One vector pass finds every delimiter; the fields are then subslices between
// them. The index buffer is reused across records, so a file of a million rows
// allocates it once.
func (r *Reader) split(line []byte) [][]byte {
	if cap(r.idx) < len(line)+1 {
		r.idx = make([]int32, len(line)+1)
	}
	idx := r.idx[:len(line)+1]
	n := simd.IndexAll(idx, line, r.Comma)

	fields := r.fields[:0]
	start := 0
	for i := 0; i < n; i++ {
		p := int(idx[i])
		fields = append(fields, r.trimLeading(line[start:p]))
		start = p + 1
	}
	fields = append(fields, r.trimLeading(line[start:]))
	r.fields = fields
	return fields
}

// trimLeading drops leading whitespace when TrimLeadingSpace is set. The
// result is still a subslice of the input, so a trimmed field costs no copy.
//
// Whitespace is unicode's, matching encoding/csv, which uses unicode.IsSpace:
// an ASCII-only rule would trim a NBSP in one package and not the other, which
// is a new divergence to buy a scan this only runs when asked for.
func (r *Reader) trimLeading(f []byte) []byte {
	if !r.TrimLeadingSpace {
		return f
	}
	for len(f) > 0 {
		c := rune(f[0])
		size := 1
		if c >= utf8.RuneSelf {
			c, size = utf8.DecodeRune(f)
		}
		if !unicode.IsSpace(c) {
			break
		}
		f = f[size:]
	}
	return f
}

// recordEnd returns the offset just past the record starting at b[0],
// respecting quotes.
//
// A newline only ends a record when it is outside quotes, so the extent has to
// be established rather than found. This walks one line at a time and counts
// quotes: a doubled quote contributes two, so parity alone decides whether the
// line ending is inside a quoted field, and no special case for "" is needed.
//
// Scanning a line at a time is the whole point. The first version ran IndexAll
// over the entire remaining buffer to find every quote and newline in the file,
// once per record, which is quadratic — a 20,000-row file took 17 seconds
// against the standard library's 3.6 milliseconds.
func (r *Reader) recordEnd(b []byte) int {
	end, _ := r.recordEndTerminated(b)
	return end
}

// recordEndTerminated is recordEnd with the fact the bounded reader needs:
// whether the record ended on a newline outside quotes, or simply ran out of
// buffer. The two are indistinguishable from the offset alone -- both return
// len(b) when the terminator is the final byte -- and a chunked reader that
// cannot tell them apart splits records at chunk edges.
//
// One walk rather than two: a second implementation of the quote-parity rule
// is how the two would come to disagree about where a record ends.
func (r *Reader) recordEndTerminated(b []byte) (int, bool) {
	off := 0
	inQuotes := false
	for {
		nl := simd.IndexByte(b[off:], '\n')
		seg := b[off:]
		if nl >= 0 {
			seg = b[off : off+nl]
		}
		if simd.CountByte(seg, '"')%2 == 1 {
			inQuotes = !inQuotes
		}
		if nl < 0 {
			return len(b), false
		}
		if !inQuotes {
			return off + nl + 1, true
		}
		off += nl + 1
	}
}

// quotedRecord splits a record that contains at least one quote.
//
// An earlier version handed the bytes to encoding/csv, which is correct and was
// 4700x slower than the standard library it is supposed to beat: a csv.Reader,
// and the bufio.Reader inside it, were allocated for every record. Parsing it
// here costs about forty lines and none of that.
//
// A field is a subslice of the input wherever it can be. Only a field
// containing a doubled quote needs a copy, because "" collapses to " and the
// result is shorter than the bytes it came from.
func (r *Reader) quotedRecord(rec []byte) [][]byte {
	fields := r.fields[:0]
	r.unq = r.unq[:0]

	// A CRLF inside a quoted field becomes an LF, which is what encoding/csv
	// does and therefore what a caller swapping this in already expects. One
	// scan of the record decides whether any field can need it, so a record
	// with no CR keeps the zero-copy path exactly as it was; only a field that
	// actually carries a CRLF pays a copy, and it is the same buffer the
	// doubled-quote unescape already builds.
	hasCR := simd.IndexByte(rec, '\r') >= 0

	i := 0
	for {
		if r.TrimLeadingSpace {
			// Before deciding quoted or not: a space ahead of the opening
			// quote is trimmed and the field is still quoted, which is what
			// encoding/csv does.
			trimmed := r.trimLeading(rec[i:])
			i = len(rec) - len(trimmed)
		}
		if i < len(rec) && rec[i] == '"' {
			i++
			start := i
			simple := true
			var buf []byte
			for i < len(rec) {
				q := simd.IndexByte(rec[i:], '"')
				if q < 0 {
					i = len(rec)
					break
				}
				i += q
				if i+1 < len(rec) && rec[i+1] == '"' {
					if simple {
						buf = appendUnCRLF(nil, rec[start:i], hasCR)
						simple = false
					} else {
						buf = appendUnCRLF(buf, rec[start:i], hasCR)
					}
					buf = append(buf, '"')
					i += 2
					start = i
					continue
				}
				break
			}
			seg := rec[start:min(i, len(rec))]
			if i < len(rec) {
				i++ // step over the closing quote
			}
			// Bytes between the closing quote and the delimiter are junk in a
			// well-formed file -- `"a"b,c`. They are still input, so they join
			// the field they follow rather than being dropped. A reader that
			// never errors on content must not silently lose content either,
			// and the previous behavior lost the `b` and invented an empty
			// field in its place.
			junkStart := i
			if i < len(rec) {
				if c := simd.IndexByte(rec[i:], r.Comma); c < 0 {
					i = len(rec)
				} else {
					i += c
				}
			}
			junk := rec[junkStart:i]
			switch {
			case !simple:
				buf = appendUnCRLF(buf, seg, hasCR)
				buf = appendUnCRLF(buf, junk, hasCR)
				fields = append(fields, buf)
			case len(junk) > 0:
				// seg and junk are not contiguous -- the closing quote sits
				// between them -- so this one needs a buffer.
				f := appendUnCRLF(nil, seg, hasCR)
				fields = append(fields, appendUnCRLF(f, junk, hasCR))
			case hasCR && hasCRLF(seg):
				fields = append(fields, appendUnCRLF(nil, seg, true))
			default:
				fields = append(fields, seg)
			}
		} else {
			start := i
			c := simd.IndexByte(rec[i:], r.Comma)
			if c < 0 {
				i = len(rec)
			} else {
				i += c
			}
			fields = append(fields, rec[start:i])
		}
		if i >= len(rec) {
			break
		}
		if rec[i] == r.Comma {
			i++
			if i == len(rec) {
				fields = append(fields, rec[len(rec):])
				break
			}
			continue
		}
		i++
	}
	r.fields = fields
	return fields
}

// hasCRLF reports whether s contains a CR immediately followed by an LF. A
// lone CR is data and stays -- encoding/csv keeps it too.
func hasCRLF(s []byte) bool {
	for i := 0; ; {
		j := simd.IndexByte(s[i:], '\r')
		if j < 0 {
			return false
		}
		i += j
		if i+1 < len(s) && s[i+1] == '\n' {
			return true
		}
		i++
	}
}

// appendUnCRLF appends s to dst with every CRLF reduced to LF. When the record
// held no CR at all the caller passes false and this is a plain append.
func appendUnCRLF(dst, s []byte, hasCR bool) []byte {
	if !hasCR {
		return append(dst, s...)
	}
	for i := 0; i < len(s); {
		j := simd.IndexByte(s[i:], '\r')
		if j < 0 {
			return append(dst, s[i:]...)
		}
		j += i
		if j+1 < len(s) && s[j+1] == '\n' {
			dst = append(dst, s[i:j]...) // drop the CR, keep the LF
			i = j + 1
			continue
		}
		dst = append(dst, s[i:j+1]...) // a lone CR is data
		i = j + 1
	}
	return dst
}

// slowRecord handles a record containing a quote.
func (r *Reader) slowRecord() (Record, error) {
	rest := r.buf[r.pos:]
	end := r.recordEnd(rest)
	rec := rest[:end]
	// Strip the line ending; recordEnd includes it.
	for len(rec) > 0 && (rec[len(rec)-1] == '\n' || rec[len(rec)-1] == '\r') {
		rec = rec[:len(rec)-1]
	}
	r.pos += end
	return Record{fields: r.own(r.quotedRecord(rec))}, nil
}

// own returns fields the caller may keep.
//
// Under ReuseRecord that is the slice itself. Otherwise the slice is copied,
// and so is any field that points into the unescape scratch — a field that came
// out of a doubled quote does not alias the input buffer, so it dies with the
// next record unless it is copied here.
func (r *Reader) own(fields [][]byte) [][]byte {
	if r.ReuseRecord {
		return fields
	}
	out := make([][]byte, len(fields))
	for i, f := range fields {
		if len(f) != 0 && !aliasesBuf(r.buf, f) {
			c := make([]byte, len(f))
			copy(c, f)
			out[i] = c
			continue
		}
		out[i] = f // a subslice of the input buffer outlives the reader
	}
	return out
}

// aliasesBuf reports whether f points inside buf, which is what distinguishes a
// zero-copy field from one built in the unescape scratch.
func aliasesBuf(buf, f []byte) bool {
	if len(buf) == 0 || len(f) == 0 {
		return false
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	p := uintptr(unsafe.Pointer(&f[0]))
	return p >= base && p < base+uintptr(len(buf))
}

func (r *Reader) checkCount(n int) error {
	switch {
	case r.FieldsPerRecord > 0:
		if n != r.FieldsPerRecord {
			return fmt.Errorf("simdcsv: record on line %d: wrong number of fields: got %d, want %d",
				r.line, n, r.FieldsPerRecord)
		}
	case r.FieldsPerRecord == 0:
		if r.num == 0 {
			r.num = n
		} else if n != r.num {
			return fmt.Errorf("simdcsv: record on line %d: wrong number of fields: got %d, want %d",
				r.line, n, r.num)
		}
	}
	return nil
}

// ReadAll reads every remaining record.
//
// ReuseRecord is ignored here, and has to be. It makes [Reader.Read] hand back
// memory the next call overwrites, which is sound in a loop that consumes each
// record before asking for the next one -- and unsound the moment every record
// is kept, which is what ReadAll does. Honouring it returned a slice whose
// entries all aliased the last record: three records in, the same record three
// times out, with no error to say so. encoding/csv copies here for the same
// reason.
//
// Unlike encoding/csv, the records parsed before an error are returned with
// it rather than discarded: this package's posture is to hand back what the
// input contained, and the error says where it stopped.
func (r *Reader) ReadAll() ([]Record, error) {
	reuse := r.ReuseRecord
	r.ReuseRecord = false
	defer func() { r.ReuseRecord = reuse }()

	var out []Record
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
}

// Record is one CSV record. Its fields point into the reader's buffer.
type Record struct{ fields [][]byte }

// Len returns the number of fields.
func (r Record) Len() int { return len(r.fields) }

// Field returns field i without copying. It is only valid while the Reader it
// came from is alive.
func (r Record) Field(i int) []byte { return r.fields[i] }

// Fields returns all fields without copying.
func (r Record) Fields() [][]byte { return r.fields }

// Strings copies the record into a []string, which is what encoding/csv would
// have returned.
func (r Record) Strings() []string {
	out := make([]string, len(r.fields))
	for i, f := range r.fields {
		out[i] = string(f)
	}
	return out
}
