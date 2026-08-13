package simdcsv

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

// Three fuzz targets, each with a different job.
//
// FuzzOverlap is the differential: inside the declared overlap this package
// and encoding/csv must agree, so any finding there is a bug in one of them.
// The generator builds well-formed CSV from atoms rather than mutating raw
// bytes, because raw bytes leave the overlap immediately and the differential
// then proves nothing.
//
// FuzzNoPanic takes arbitrary bytes and asserts only that reading terminates
// without panicking. Malformed input may produce any record content -- that is
// the documented contract, not a bug.
//
// FuzzContractMalformed pins the decisions from the malformed matrix, so a
// change to the parser cannot quietly move them.

// buildCSV turns fuzz-chosen bytes into a well-formed document, and returns
// both the text and the records it must parse to. Everything here stays inside
// the overlap: fields carry no CR at all, so nothing depends on the
// normalization rule, and the record terminator is the only place \r appears.
func buildCSV(seed []byte, comma byte, crlf bool) (string, [][]string) {
	if len(seed) == 0 {
		return "", nil
	}
	atoms := []string{"", "a", "bc", "d e", "x" + string(comma) + "y", "q\"r", "m\nn", "  ", "0", "\"\""}
	rows := int(seed[0]%16) + 1
	cols := int(seed[len(seed)-1]%8) + 1
	var b strings.Builder
	want := make([][]string, 0, rows)
	k := 0
	for r := 0; r < rows; r++ {
		rec := make([]string, 0, cols)
		for c := 0; c < cols; c++ {
			a := atoms[int(seed[k%len(seed)])%len(atoms)]
			k++
			rec = append(rec, a)
			if c > 0 {
				b.WriteByte(comma)
			}
			// Quote whenever the atom carries something structural, always
			// when it is empty, and sometimes when neither: both spellings
			// must parse the same.
			//
			// The empty case is not decoration. A one-column record holding an
			// unquoted empty field renders as a blank line, and a blank line is
			// skipped by both parsers -- so the document would parse to fewer
			// records than the generator built, and the differential would
			// report a disagreement that is the generator's fault. Found by
			// this fuzz target on its first run.
			if a == "" || strings.ContainsAny(a, string(comma)+"\"\n") || seed[k%len(seed)]&1 == 0 {
				b.WriteByte('"')
				b.WriteString(strings.ReplaceAll(a, "\"", "\"\""))
				b.WriteByte('"')
			} else {
				b.WriteString(a)
			}
		}
		if crlf {
			b.WriteString("\r\n")
		} else {
			b.WriteByte('\n')
		}
		want = append(want, rec)
	}
	return b.String(), want
}

func FuzzOverlap(f *testing.F) {
	for _, s := range []string{"a", "ab", "abc", "zzz", "\x00\x01\x02", "\xff\xfe"} {
		f.Add([]byte(s), true, false)
		f.Add([]byte(s), false, true)
	}
	f.Fuzz(func(t *testing.T, seed []byte, semicolon, crlf bool) {
		comma := byte(',')
		if semicolon {
			comma = ';'
		}
		in, want := buildCSV(seed, comma, crlf)
		if in == "" {
			return
		}
		sr := csv.NewReader(strings.NewReader(in))
		sr.Comma = rune(comma)
		sr.FieldsPerRecord = -1
		std, stdErr := sr.ReadAll()

		r := NewReader(strings.NewReader(in))
		r.Comma = comma
		r.FieldsPerRecord = -1
		got, gotErr := r.ReadAll()

		if (stdErr != nil) != (gotErr != nil) {
			t.Fatalf("input %q: stdlib err=%v, simdcsv err=%v", in, stdErr, gotErr)
		}
		if stdErr != nil {
			return
		}
		// The generator says what it built; stdlib is the second opinion.
		if len(std) != len(want) {
			t.Fatalf("input %q: the generator and stdlib disagree (%d vs %d records)", in, len(std), len(want))
		}
		if len(got) != len(std) {
			t.Fatalf("input %q: %d records, stdlib %d", in, len(got), len(std))
		}
		for i := range got {
			gs := got[i].Strings()
			if len(gs) != len(std[i]) {
				t.Fatalf("input %q record %d: %q, stdlib %q", in, i, gs, std[i])
			}
			for j := range gs {
				if gs[j] != std[i][j] {
					t.Fatalf("input %q record %d field %d: %q, stdlib %q", in, i, j, gs[j], std[i][j])
				}
			}
		}
	})
}

func FuzzNoPanic(f *testing.F) {
	for _, s := range []string{
		"", "a\n", "\"", "\"\"\"", "a,b\r\n", "\"a\r\nb\",c\n",
		"\"a\"b,c\n", "\"a,b", ",,,\n\n\n", strings.Repeat("\"", 64),
	} {
		f.Add([]byte(s), byte(','), false)
	}
	f.Fuzz(func(t *testing.T, in []byte, comma byte, reuse bool) {
		if comma == 0 {
			comma = ','
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			r := NewReader(strings.NewReader(string(in)))
			r.Comma = comma
			r.ReuseRecord = reuse
			r.FieldsPerRecord = -1
			for n := 0; n < 4096; n++ {
				if _, err := r.Read(); err != nil {
					return
				}
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("read did not terminate on %q comma=%q", in, comma)
		}
	})
}

// FuzzContractMalformed keeps the malformed decisions pinned under mutation:
// whatever junk follows a closing quote, every data byte of the input has to
// come back out in some field.
func FuzzContractMalformed(f *testing.F) {
	for _, s := range []string{"a", "b", "xy", "a\"b", "a,b", "a\nb"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, field, junk string) {
		// Keep the junk free of the record terminator: a newline there ends
		// the record and the bytes after it belong to the next one, which is
		// a different property than the one under test.
		if strings.ContainsAny(junk, "\n\r") || strings.ContainsAny(field, "\n\r") {
			return
		}
		in := "\"" + strings.ReplaceAll(field, "\"", "\"\"") + "\"" + junk + "\n"
		recs, err := NewReader(strings.NewReader(in)).ReadAll()
		if err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		var out strings.Builder
		for _, rec := range recs {
			for _, f := range rec.Strings() {
				out.WriteString(f)
			}
		}
		want := countData(field) + countData(junk)
		if have := countData(out.String()); have != want {
			t.Fatalf("input %q: %d data bytes survived of %d (fields %q)", in, have, want, out.String())
		}
	})
}

// countData counts the bytes that are never structural, so they must survive
// any parse.
func countData(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			n++
		}
	}
	return n
}
