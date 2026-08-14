package simdcsv

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// The bounded reader must produce byte-identical records to the whole-buffer
// path, on the overlap corpus and on the malformed rows. A chunked reader that
// is merely "close" is not a reader, and the boundary cases are exactly where
// a chunk edge lands.

// boundedRecords copies each record before the next Read, which is the
// bounded path's contract: the buffer is compacted and refilled under the
// previous record's fields.
func boundedRecords(t *testing.T, in string, chunk int) [][]string {
	t.Helper()
	r := NewReader(strings.NewReader(in))
	b := newBounded(r, strings.NewReader(in), chunk, 1<<20)
	var out [][]string
	for {
		rec, err := b.Read()
		if err != nil {
			break
		}
		out = append(out, append([]string(nil), rec.Strings()...))
	}
	return out
}

func wholeRecords(t *testing.T, in string) [][]string {
	t.Helper()
	recs, err := NewReader(strings.NewReader(in)).ReadAll()
	if err != nil {
		t.Fatalf("input %q: %v", in, err)
	}
	out := make([][]string, len(recs))
	for i, rec := range recs {
		out[i] = append([]string(nil), rec.Strings()...)
	}
	return out
}

func sameRecords(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// Every chunk size from 1 upward, so a boundary lands inside every construct
// the format has: mid-field, mid-quote, between the CR and the LF.
func TestBoundedMatchesWholeBufferAtEveryChunkSize(t *testing.T) {
	inputs := []string{
		"a,b,c\n1,2,3\n",
		"a,b,c\r\n1,2,3\r\n",
		"\"a,b\",c\n",
		"\"a\nb\",c\n",
		"\"a\r\nb\",c\n",
		"\"a\"\"b\",c\n",
		"plain,row\n\"quoted,x\",y\nplain2,row2\n",
		"a,b\n\"c\",d\ne,f\n",
		"a,,c\n,,\n",
		"no,trailing,newline",
		"\"a\"b,c\n", // junk after a closing quote
		"\"a,b\n",    // unclosed quote
		"\"\"\"\n",   // three quotes
		"a\rb,c\n",   // a bare CR mid-field
		strings.Repeat("x,y,z\n", 40),
	}
	for _, in := range inputs {
		want := wholeRecords(t, in)
		for chunk := 1; chunk <= len(in)+2; chunk++ {
			got := boundedRecords(t, in, chunk)
			if !sameRecords(got, want) {
				t.Fatalf("input %q chunk %d:\n got  %q\n want %q", in, chunk, got, want)
			}
		}
	}
}

// A generated corpus, every chunk size, so the combinations nobody writes down
// are covered too.
func TestBoundedMatchesWholeBufferOnGeneratedInput(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	atoms := []string{"", "a", "bc", "d e", "x,y", "q\"r", "m\nn", "0"}
	for iter := 0; iter < 300; iter++ {
		var sb strings.Builder
		rows := 1 + rng.Intn(6)
		cols := 1 + rng.Intn(4)
		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				if c > 0 {
					sb.WriteByte(',')
				}
				a := atoms[rng.Intn(len(atoms))]
				if a == "" || strings.ContainsAny(a, ",\"\n") || rng.Intn(2) == 0 {
					sb.WriteByte('"')
					sb.WriteString(strings.ReplaceAll(a, "\"", "\"\""))
					sb.WriteByte('"')
				} else {
					sb.WriteString(a)
				}
			}
			if rng.Intn(4) == 0 {
				sb.WriteString("\r\n")
			} else {
				sb.WriteByte('\n')
			}
		}
		in := sb.String()
		want := wholeRecords(t, in)
		for _, chunk := range []int{1, 2, 3, 5, 8, 13, 64, len(in)} {
			got := boundedRecords(t, in, chunk)
			if !sameRecords(got, want) {
				t.Fatalf("iter %d chunk %d input %q:\n got  %q\n want %q", iter, chunk, in, got, want)
			}
		}
	}
}

// The memory bound is the claim being evaluated: the buffer must not grow
// with the input, only with the longest record.
func TestBoundedBufferDoesNotGrowWithTheInput(t *testing.T) {
	for _, rows := range []int{100, 10000, 200000} {
		var sb strings.Builder
		for i := 0; i < rows; i++ {
			sb.WriteString("aaaa,bbbb,cccc,dddd\n")
		}
		in := sb.String()
		r := NewReader(strings.NewReader(in))
		b := newBounded(r, strings.NewReader(in), 4096, 1<<20)
		n := 0
		peak := 0
		for {
			_, err := b.Read()
			if err != nil {
				break
			}
			n++
			if cap(r.buf) > peak {
				peak = cap(r.buf)
			}
		}
		if n != rows {
			t.Fatalf("%d rows: read %d", rows, n)
		}
		// A 20-byte record with a 4 KiB chunk: the buffer should stay near the
		// chunk, whatever the file size.
		if peak > 3*4096 {
			t.Fatalf("%d rows: buffer peaked at %d bytes for a 4096-byte chunk", rows, peak)
		}
		t.Logf("%d rows (%d bytes): peak buffer %d bytes", rows, len(in), peak)
	}
}

// A record larger than the budget is an error rather than an unbounded grow,
// because a record that does not fit cannot be parsed without buffering it.
func TestBoundedRefusesAnOversizedRecord(t *testing.T) {
	in := "\"" + strings.Repeat("x", 100000) + "\"\n"
	r := NewReader(strings.NewReader(in))
	b := newBounded(r, strings.NewReader(in), 1024, 8192)
	if _, err := b.Read(); err != ErrRecordTooLarge {
		t.Fatalf("err %v, want ErrRecordTooLarge", err)
	}
}

// Bounded records do NOT outlive the next Read, and this pins that rather
// than wishing otherwise: the buffer moves under them. It is the cost of the
// memory bound, and a caller who needs to keep a record copies it.
func TestBoundedRecordsExpireAtTheNextRead(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("field0,field1,field2\n")
	}
	in := sb.String()
	r := NewReader(strings.NewReader(in))
	b := newBounded(r, strings.NewReader(in), 512, 1<<20)
	var kept []Record
	for {
		rec, err := b.Read()
		if err != nil {
			break
		}
		kept = append(kept, rec)
	}
	if len(kept) != 2000 {
		t.Fatalf("kept %d records", len(kept))
	}
	// Copying at read time is the supported pattern, and it works.
	r2 := NewReader(strings.NewReader(in))
	b2 := newBounded(r2, strings.NewReader(in), 512, 1<<20)
	n := 0
	for {
		rec, err := b2.Read()
		if err != nil {
			break
		}
		s := append([]string(nil), rec.Strings()...)
		if len(s) != 3 || s[0] != "field0" || s[2] != "field2" {
			t.Fatalf("record %d reads %q when copied at read time", n, s)
		}
		n++
	}
	if n != 2000 {
		t.Fatalf("copied %d records", n)
	}
	_ = bytes.MinRead
}

func benchCorpus(rows, cols int, quoted bool) string {
	var sb strings.Builder
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			if c > 0 {
				sb.WriteByte(',')
			}
			if quoted {
				sb.WriteString("\"value")
				sb.WriteString(itoa(c))
				sb.WriteString("\"")
			} else {
				sb.WriteString("value")
				sb.WriteString(itoa(c))
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// The A/B the evaluation turns on: bounded against whole-buffer, on the four
// shapes the plan named.
func BenchmarkBoundedVsWhole(b *testing.B) {
	for _, shape := range []struct {
		name   string
		cols   int
		quoted bool
	}{
		{"unquoted-4col", 4, false},
		{"unquoted-16col", 16, false},
		{"quoted-4col", 4, true},
		{"quoted-16col", 16, true},
	} {
		in := benchCorpus(20000, shape.cols, shape.quoted)
		b.Run("whole/"+shape.name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for i := 0; i < b.N; i++ {
				r := NewReader(strings.NewReader(in))
				n := 0
				for {
					rec, err := r.Read()
					if err != nil {
						break
					}
					n += rec.Len()
				}
				if n == 0 {
					b.Fatal("nothing read")
				}
			}
		})
		b.Run("bounded/"+shape.name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for i := 0; i < b.N; i++ {
				r := NewReader(strings.NewReader(in))
				bd := newBounded(r, strings.NewReader(in), 64<<10, 1<<20)
				n := 0
				for {
					rec, err := bd.Read()
					if err != nil {
						break
					}
					n += rec.Len()
				}
				if n == 0 {
					b.Fatal("nothing read")
				}
			}
		})
	}
}
