package simdcsv

import (
	"errors"
	"io"
)

// Bounded reading: a chunk at a time, instead of the whole file.
//
// The whole-buffer path reads the input with io.ReadAll, so peak memory is the
// file. That is the trade the package makes -- finding every delimiter at once
// needs a buffer to find them in -- and it rules out inputs larger than
// memory, which is the one thing encoding/csv can do that this cannot.
//
// The problem a chunked reader has to solve is exactly the one recordEnd
// already solves per line: a chunk boundary may fall inside a quoted field, so
// where a record ends cannot be decided by looking for a newline. The fill
// loop therefore keeps reading until the buffer holds a complete record, and
// the memory bound is a chunk plus the longest record rather than the file.
//
// One property does not survive the change, and it is the package's main
// advantage: records cannot outlive the next Read. The whole-buffer path
// returns fields that alias a buffer which never moves; a bounded buffer is
// compacted and refilled under them. So bounded records carry encoding/csv's
// contract -- valid until the next call -- and a caller who needs to keep one
// copies it. That is not a defect in the prototype, it is what bounded memory
// costs, and it is the finding this evaluation exists to produce.
//
// This is a prototype under evaluation (plan task 4). It is unexported and
// reachable only from tests until the measurements say whether it ships.

// ErrRecordTooLarge means a single record did not fit the bounded reader's
// budget. A record larger than the budget cannot be parsed without buffering
// it, so the budget is the honest limit rather than something to grow past
// silently.
var ErrRecordTooLarge = errors.New("simdcsv: record exceeds the read budget")

const defaultChunk = 64 << 10

// bounded wraps a Reader so it fills from src a chunk at a time.
type bounded struct {
	r     *Reader
	src   io.Reader
	chunk int
	max   int  // the largest the buffer may grow, so one huge record errors
	eof   bool // src returned io.EOF
}

func newBounded(r *Reader, src io.Reader, chunk, max int) *bounded {
	if chunk <= 0 {
		chunk = defaultChunk
	}
	if max <= 0 {
		max = 64 << 20
	}
	b := &bounded{r: r, src: src, chunk: chunk, max: max}
	r.buf = make([]byte, 0, chunk)
	r.src = nil // the bounded path owns the reader
	return b
}

// Read returns the next record, filling from src as needed.
func (b *bounded) Read() (Record, error) {
	for {
		if b.complete() {
			return b.r.Read()
		}
		if b.eof {
			if b.r.pos >= len(b.r.buf) {
				return Record{}, io.EOF
			}
			// The last record, unterminated.
			return b.r.Read()
		}
		if err := b.fill(); err != nil && err != io.EOF {
			return Record{}, err
		}
	}
}

// complete reports whether the buffer holds a whole record.
//
// recordEnd walks lines counting quotes, so it answers the question a newline
// search cannot: whether the newline it found is inside a quoted field. A
// record that runs to the end of the buffer is only complete if the input is.
func (b *bounded) complete() bool {
	if b.r.pos >= len(b.r.buf) {
		return false
	}
	// The buffer ending in a newline is not enough: that newline may be inside
	// a quoted field, which is the whole difficulty. recordEndTerminated says
	// whether the walk stopped on a terminator or ran out of bytes.
	_, terminated := b.r.recordEndTerminated(b.r.buf[b.r.pos:])
	return terminated
}

// fill compacts what has been delivered and appends another chunk.
//
// Compaction happens here rather than before every record, which was the
// prototype's first shape and cost 3.4x the whole-buffer path: a 64 KiB move
// per 20-byte record. The bound does not need it any sooner -- the buffer only
// has to shrink before it grows.
func (b *bounded) fill() error {
	if b.r.pos > 0 {
		n := copy(b.r.buf, b.r.buf[b.r.pos:])
		b.r.buf = b.r.buf[:n]
		b.r.pos = 0
	}
	if len(b.r.buf)+b.chunk > b.max {
		return ErrRecordTooLarge
	}
	want := len(b.r.buf) + b.chunk
	if cap(b.r.buf) < want {
		nb := make([]byte, len(b.r.buf), want)
		copy(nb, b.r.buf)
		b.r.buf = nb
	}
	b.r.buf = b.r.buf[:want]
	n, err := b.src.Read(b.r.buf[want-b.chunk:])
	b.r.buf = b.r.buf[:want-b.chunk+n]
	if err == io.EOF {
		b.eof = true
	}
	return err
}

// ReadAll returns every remaining record.
func (b *bounded) ReadAll() ([]Record, error) {
	var out []Record
	for {
		rec, err := b.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
}
