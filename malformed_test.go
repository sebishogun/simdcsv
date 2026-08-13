package simdcsv

import (
	"encoding/csv"
	"strings"
	"testing"
)

// The decision matrix for malformed input, one case per row of
// architecture.md section 7.
//
// This package never errors on input content: the only errors are a read
// failure, io.EOF, and the field-count check. That is a deliberate posture --
// a reader, not a validator -- and it comes with one rule that is not
// negotiable: never lose bytes. A parser that accepts everything and silently
// drops part of it is worse than one that rejects, because nothing downstream
// can tell.
//
// encoding/csv errors on most of these. Its LazyQuotes mode does not agree
// either, and deliberately so: there an unterminated quote consumes the rest
// of the file, newlines included, so one stray byte destroys every following
// record. Here an unterminated quote ends with its record, and the damage
// stops there.

func recordStrings(t *testing.T, in string, reuse bool) [][]string {
	t.Helper()
	r := NewReader(strings.NewReader(in))
	r.ReuseRecord = reuse
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("input %q: %v", in, err)
	}
	out := make([][]string, len(recs))
	for i, rec := range recs {
		out[i] = append([]string(nil), rec.Strings()...)
	}
	return out
}

func TestMalformedDecisions(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want [][]string
		why  string
	}{
		{
			name: "quote inside an unquoted field is data",
			in:   "a\"b,c\n",
			want: [][]string{{`a"b`, "c"}},
			why:  "accept as-is: nothing is lost and the split is unambiguous",
		},
		{
			name: "junk after a closing quote joins its field",
			in:   "\"a\"b,c\n",
			want: [][]string{{"ab", "c"}},
			why:  "was [a][][c]: the b vanished and an empty field appeared in its place",
		},
		{
			name: "junk containing quotes joins its field verbatim",
			in:   "\"a\"x\"b\",c\n",
			want: [][]string{{`ax"b"`, "c"}},
			why:  "same rule; the quotes in the junk are data because the field already closed",
		},
		{
			name: "space after a closing quote joins its field",
			in:   "\"a\" ,c\n",
			want: [][]string{{"a ", "c"}},
			why:  "no special case for whitespace: TrimLeadingSpace is not implemented here",
		},
		{
			name: "unclosed quote runs to the end of the record",
			in:   "\"a,b\n",
			want: [][]string{{"a,b"}},
			why:  "accept as-is: the damage stops at the record, unlike LazyQuotes",
		},
		{
			name: "unclosed quote after a doubled quote",
			in:   "\"a\"\"b\n",
			want: [][]string{{`a"b`}},
			why:  "accept as-is: the doubling still unescapes",
		},
		{
			name: "lone quote",
			in:   "\"\n",
			want: [][]string{{""}},
			why:  "accept as-is: an empty unterminated field",
		},
		{
			name: "three quotes",
			in:   "\"\"\"\n",
			want: [][]string{{`"`}},
			why:  "accept as-is: an unterminated field holding one unescaped quote",
		},
		{
			name: "EOF in the middle of a quoted field",
			in:   "\"a,b",
			want: [][]string{{"a,b"}},
			why:  "accept as-is: truncation is not corruption",
		},
		{
			name: "no trailing newline",
			in:   "a,b",
			want: [][]string{{"a", "b"}},
			why:  "parity with encoding/csv",
		},
		{
			name: "trailing CR at EOF",
			in:   "a,b\r",
			want: [][]string{{"a", "b"}},
			why:  "parity with encoding/csv",
		},
		{
			name: "junk after a closing quote at end of record",
			in:   "\"a\"b\n",
			want: [][]string{{"ab"}},
			why:  "the rule holds with no delimiter after the junk",
		},
		{
			name: "junk after a closing quote at EOF",
			in:   "\"a\"b",
			want: [][]string{{"ab"}},
			why:  "and with no record terminator either",
		},
		{
			name: "well-formed quoted field is untouched",
			in:   "\"a\",c\n",
			want: [][]string{{"a", "c"}},
			why:  "the junk rule must not fire on a correct file",
		},
		{
			name: "doubled quote in a well-formed field is untouched",
			in:   "\"a\"\"b\",c\n",
			want: [][]string{{`a"b`, "c"}},
			why:  "likewise",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, reuse := range []bool{false, true} {
				got := recordStrings(t, c.in, reuse)
				if len(got) != len(c.want) {
					t.Fatalf("reuse=%v: %d records, want %d (%s)", reuse, len(got), len(c.want), c.why)
				}
				for i := range got {
					if len(got[i]) != len(c.want[i]) {
						t.Fatalf("reuse=%v record %d: %q, want %q (%s)", reuse, i, got[i], c.want[i], c.why)
					}
					for j := range got[i] {
						if got[i][j] != c.want[i][j] {
							t.Fatalf("reuse=%v record %d: %q, want %q (%s)", reuse, i, got[i], c.want[i], c.why)
						}
					}
				}
			}
		})
	}
}

// Nothing the parser accepts may drop a byte. Every input byte that is not a
// delimiter, a record terminator, or a structural quote has to appear in some
// field, and this counts them rather than trusting the table above.
func TestMalformedLosesNoBytes(t *testing.T) {
	for _, in := range []string{
		"a\"b,c\n", "\"a\"b,c\n", "\"a\"x\"b\",c\n", "\"a\" ,c\n",
		"\"a,b\n", "\"a\"\"b\n", "\"\n", "\"\"\"\n", "\"a,b",
		"\"a\"b\n", "\"a\"b", "x,\"y\"z,w\n", "\"\"a\"\",b\n",
	} {
		recs := recordStrings(t, in, false)
		var got strings.Builder
		for _, rec := range recs {
			for _, f := range rec {
				got.WriteString(f)
			}
		}
		// Letters and digits are never structural, so every one of them in the
		// input must survive into some field.
		want := 0
		for i := 0; i < len(in); i++ {
			if c := in[i]; c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
				want++
			}
		}
		have := 0
		s := got.String()
		for i := 0; i < len(s); i++ {
			if c := s[i]; c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
				have++
			}
		}
		if have != want {
			t.Errorf("input %q: %d data bytes survived of %d (fields %q)", in, have, want, s)
		}
	}
}

// The malformed cases must not drag well-formed records off the fast path.
// A record with no quote takes the aliasing path whatever else is in it.
func TestFastPathSurvivesMalformedNeighbours(t *testing.T) {
	for _, in := range []string{
		"a,b\n",
		"a\rb,c\n", // a bare CR mid-field is not structural
		"a,b\r\n",
		"plain,row\n",
	} {
		r := NewReader(strings.NewReader(in))
		r.ReuseRecord = true
		rec, err := r.Read()
		if err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		for i, f := range rec.fields {
			if len(f) != 0 && !aliasesBuf(r.buf, f) {
				t.Errorf("input %q field %d does not alias the input buffer: the fast path was left", in, i)
			}
		}
	}
	// A quote anywhere in the record means the slow path, by construction.
	r := NewReader(strings.NewReader("a\"b,c\n"))
	r.ReuseRecord = true
	if _, err := r.Read(); err != nil {
		t.Fatal(err)
	}
}

// ReadAll keeps every record it returns, so ReuseRecord cannot apply to it:
// honouring the flag handed back a slice whose entries all aliased the last
// record parsed. Three records in, the same record three times out, and no
// error to say so.
func TestReadAllIgnoresReuseRecord(t *testing.T) {
	const in = "a,1\nb,2\nc,3\n"
	want := [][]string{{"a", "1"}, {"b", "2"}, {"c", "3"}}
	for _, reuse := range []bool{false, true} {
		r := NewReader(strings.NewReader(in))
		r.ReuseRecord = reuse
		got, err := r.ReadAll()
		if err != nil {
			t.Fatalf("reuse=%v: %v", reuse, err)
		}
		if len(got) != len(want) {
			t.Fatalf("reuse=%v: %d records, want %d", reuse, len(got), len(want))
		}
		for i := range got {
			gs := got[i].Strings()
			for j := range want[i] {
				if gs[j] != want[i][j] {
					t.Fatalf("reuse=%v record %d: %q, want %q", reuse, i, gs, want[i])
				}
			}
		}
		// The caller's setting survives the call.
		if r.ReuseRecord != reuse {
			t.Fatalf("ReadAll left ReuseRecord as %v, caller set %v", r.ReuseRecord, reuse)
		}
	}
}

// Read still reuses when asked: ReadAll's exception must not disable the flag
// for the streaming path, which is the whole reason it exists.
func TestReadStillReusesRecords(t *testing.T) {
	r := NewReader(strings.NewReader("a,1\nb,2\n"))
	r.ReuseRecord = true
	first, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(first.Field(0)); got != "a" {
		t.Fatalf("first record reads %q", got)
	}
	if _, err := r.Read(); err != nil {
		t.Fatal(err)
	}
	// The point of the flag: the first Record's own slice was handed to the
	// second record, so reading it now gives the second record's field. The
	// field header has to be read after the second Read to see it -- taking a
	// copy of it beforehand would keep pointing at the old bytes.
	if got := string(first.Field(0)); got != "b" {
		t.Fatalf("first record reads %q after a second Read; ReuseRecord stopped reusing", got)
	}
}

// TrimLeadingSpace against encoding/csv's option of the same name. The rule
// has three halves that are easy to get wrong: trailing whitespace stays,
// whitespace inside a quoted field is data, and a space before an opening
// quote is trimmed with the field still parsing as quoted.
func TestTrimLeadingSpaceMatchesStdlib(t *testing.T) {
	for _, in := range []string{
		" a, b ,c\n",
		"\ta,b\n",
		" \"a\",b\n",
		"\" a\",b\n",
		"a,  \n",
		"  ,x\n",
		" \n",
		"a,b\n",
		"   spaced   ,   x\n",
		" a,b\n", // NBSP is whitespace to unicode.IsSpace
	} {
		sr := csv.NewReader(strings.NewReader(in))
		sr.TrimLeadingSpace = true
		sr.FieldsPerRecord = -1
		want, wantErr := sr.ReadAll()

		r := NewReader(strings.NewReader(in))
		r.TrimLeadingSpace = true
		r.FieldsPerRecord = -1
		got, gotErr := r.ReadAll()

		if (wantErr != nil) != (gotErr != nil) {
			t.Errorf("input %q: stdlib err=%v, simdcsv err=%v", in, wantErr, gotErr)
			continue
		}
		if wantErr != nil {
			continue
		}
		if len(got) != len(want) {
			t.Errorf("input %q: %d records, stdlib %d", in, len(got), len(want))
			continue
		}
		for i := range got {
			gs := got[i].Strings()
			if len(gs) != len(want[i]) {
				t.Errorf("input %q record %d: %q, stdlib %q", in, i, gs, want[i])
				continue
			}
			for j := range gs {
				if gs[j] != want[i][j] {
					t.Errorf("input %q record %d: %q, stdlib %q", in, i, gs, want[i])
					break
				}
			}
		}
	}
}

// Whitespace after a closing quote is junk, which is malformed input and
// therefore outside the declared overlap: encoding/csv errors, this package
// does not. The no-lost-bytes rule still applies, so the junk joins its field
// and the trailing whitespace survives -- trimming is leading-only.
func TestTrimLeadingSpaceWithTrailingJunk(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{" \"a b\" ,c\n", []string{"a b ", "c"}},
		{"\t\t\"q\"\t,z\n", []string{"q\t", "z"}},
	} {
		r := NewReader(strings.NewReader(c.in))
		r.TrimLeadingSpace = true
		got, err := r.ReadAll()
		if err != nil {
			t.Fatalf("input %q: %v", c.in, err)
		}
		gs := got[0].Strings()
		for j := range c.want {
			if gs[j] != c.want[j] {
				t.Fatalf("input %q: %q, want %q", c.in, gs, c.want)
			}
		}
	}
}

// Off by default, and off means untouched.
func TestTrimLeadingSpaceOffByDefault(t *testing.T) {
	r := NewReader(strings.NewReader(" a, b\n"))
	if r.TrimLeadingSpace {
		t.Fatal("TrimLeadingSpace defaults to true")
	}
	rec, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Strings(); got[0] != " a" || got[1] != " b" {
		t.Fatalf("fields %q, want the spaces kept", got)
	}
}

// Trimming must not cost the zero-copy property: a trimmed field is still a
// subslice of the input buffer.
func TestTrimLeadingSpaceStaysZeroCopy(t *testing.T) {
	r := NewReader(strings.NewReader("   a,   b\n"))
	r.TrimLeadingSpace = true
	r.ReuseRecord = true
	rec, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range rec.fields {
		if len(f) != 0 && !aliasesBuf(r.buf, f) {
			t.Errorf("field %d was copied; trimming should only move the start", i)
		}
	}
}
