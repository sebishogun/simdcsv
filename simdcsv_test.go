package simdcsv

import (
	"encoding/csv"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"
)

// Every input is parsed both ways and the results compared. encoding/csv is the
// definition of correct inside the declared overlap: this package exists to be
// faster, not different. Nothing delegates to it -- the quoted path is parsed
// in-house -- so parity is asserted whichever path a record takes.
func checkAgainstStdlib(t *testing.T, in string) {
	t.Helper()

	want, wantErr := csv.NewReader(strings.NewReader(in)).ReadAll()
	got, gotErr := NewReader(strings.NewReader(in)).ReadAll()

	if (wantErr != nil) != (gotErr != nil) {
		t.Fatalf("input %q: stdlib err=%v, simdcsv err=%v", in, wantErr, gotErr)
	}
	if wantErr != nil {
		return // both failed; the messages differ by design
	}
	if len(got) != len(want) {
		t.Fatalf("input %q: %d records, want %d", in, len(got), len(want))
	}
	for i := range want {
		g := got[i].Strings()
		if len(g) != len(want[i]) {
			t.Fatalf("input %q: record %d has %d fields, want %d\n got %q\nwant %q",
				in, i, len(g), len(want[i]), g, want[i])
		}
		for j := range want[i] {
			if g[j] != want[i][j] {
				t.Fatalf("input %q: record %d field %d: got %q, want %q",
					in, i, j, g[j], want[i][j])
			}
		}
	}
}

func TestMatchesStdlib(t *testing.T) {
	for _, in := range []string{
		"",
		"a\n",
		"a,b,c\n",
		"a,b,c\n1,2,3\n",
		"a,b,c\r\n1,2,3\r\n",
		"a,b,c",     // no trailing newline
		"a,,c\n",    // empty field
		",,\n",      // all empty
		"a\nb\nc\n", // single column
		"1,2\n3,4\n5,6\n",
		`"a","b"` + "\n",
		`"a,b",c` + "\n", // delimiter inside quotes
		"\"a\nb\",c\n",   // newline inside quotes
		// CRLF inside a quoted field: encoding/csv reduces it to LF, and so
		// does this now. A lone CR is data and stays. Task 0 of the production
		// plan decided this; before it, the whole class was excluded from the
		// declared overlap because the two disagreed.
		"\"a\r\nb\",c\r\n",
		"\"a\r\n\r\nb\",c\n",
		"x,\"y\r\nz\"\r\n",
		"\"a\rb\",c\n",
		"\"a\r\",c\n",
		"\"a\r\nb\"\"c\",d\n",
		`"a""b",c` + "\n",                          // escaped quote
		"plain,row\n\"quoted,x\",y\nplain2,row2\n", // mixed
		"a,b\n\"c\",d\ne,f\n",
		strings.Repeat("x,y,z\n", 100),
	} {
		checkAgainstStdlib(t, in)
	}
}

// Randomised, because the interesting failures in a parser are the combinations
// nobody writes down.
func TestMatchesStdlibRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	atoms := []string{"a", "bb", "", "1", "-2.5", `"q"`, `"a,b"`, `"x""y"`, "  sp  ", "\"multi\nline\""}
	for iter := 0; iter < 400; iter++ {
		var sb strings.Builder
		rows := 1 + r.IntN(5)
		cols := 1 + r.IntN(4)
		for i := 0; i < rows; i++ {
			for j := 0; j < cols; j++ {
				if j > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(atoms[r.IntN(len(atoms))])
			}
			sb.WriteByte('\n')
		}
		checkAgainstStdlib(t, sb.String())
	}
}

// The fast path must actually run, or every test above passes by delegating.
func TestFastPathRuns(t *testing.T) {
	in := "1,2,3\n4,5,6\n"
	r := NewReader(strings.NewReader(in))
	rec, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	// A fast-path field is a subslice of the reader's buffer, so its backing
	// array is the buffer. A field from the fallback is a fresh copy.
	f := rec.Field(0)
	if len(f) == 0 || !aliases(r.buf, f) {
		t.Error("field does not point into the reader's buffer; the fast path did not run")
	}
}

func TestFieldsPerRecord(t *testing.T) {
	_, err := NewReader(strings.NewReader("a,b\nc\n")).ReadAll()
	if err == nil {
		t.Error("expected an error for a short record")
	}
	r := NewReader(strings.NewReader("a,b\nc\n"))
	r.FieldsPerRecord = -1
	if _, err := r.ReadAll(); err != nil {
		t.Errorf("FieldsPerRecord=-1 should allow ragged records: %v", err)
	}
}

func TestSemicolonDelimiter(t *testing.T) {
	r := NewReader(strings.NewReader("a;b;c\n"))
	r.Comma = ';'
	rec, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Strings(); len(got) != 3 || got[1] != "b" {
		t.Errorf("got %q", got)
	}
}

func genCSV(rows, cols int, quoted bool) string {
	var sb strings.Builder
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			if quoted {
				fmt.Fprintf(&sb, `"v%d_%d"`, i, j)
			} else {
				fmt.Fprintf(&sb, "v%d_%d", i, j)
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func BenchmarkRead(b *testing.B) {
	for _, cols := range []int{4, 16} {
		for _, quoted := range []bool{false, true} {
			name := "plain"
			if quoted {
				name = "quoted"
			}
			data := genCSV(20000, cols, quoted)
			b.Run(fmt.Sprintf("cols=%d/%s/stdlib", cols, name), func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				for b.Loop() {
					rd := csv.NewReader(strings.NewReader(data))
					rd.ReuseRecord = true
					for {
						if _, err := rd.Read(); err == io.EOF {
							break
						} else if err != nil {
							b.Fatal(err)
						}
					}
				}
			})
			b.Run(fmt.Sprintf("cols=%d/%s/simdcsv", cols, name), func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				for b.Loop() {
					rd := NewReader(strings.NewReader(data))
					rd.ReuseRecord = true
					for {
						if _, err := rd.Read(); err == io.EOF {
							break
						} else if err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

// aliases reports whether sub points inside buf. unsafe is the only way to ask,
// and asking is the point: a fast-path field is a subslice of the reader's
// buffer, while a fallback field is a fresh copy, so this distinguishes which
// path produced it without exporting anything to say so.
func aliases(buf, sub []byte) bool {
	if len(buf) == 0 || len(sub) == 0 {
		return false
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	p := uintptr(unsafe.Pointer(&sub[0]))
	return p >= base && p < base+uintptr(len(buf))
}
