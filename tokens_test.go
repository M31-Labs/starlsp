package starlsp

// Semantic tokens and folding ranges are positions in the buffer, and an editor
// reads them against its own text. LSP counts columns in UTF-16 code units,
// while tree-sitter counts bytes, so a token that follows a non-ASCII character
// on its line lands in the wrong place unless the server converts. Folding
// answers must also come from one version of the buffer.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

type tokenAt struct{ line, col, length, kind int }

// decodeTokens turns the wire encoding of semantic tokens into absolute
// positions.
func decodeTokens(t *testing.T, data []uint32) []tokenAt {
	t.Helper()
	if len(data)%5 != 0 {
		t.Fatalf("the token array has %d numbers, not a multiple of 5", len(data))
	}
	var out []tokenAt
	line, col := 0, 0
	for i := 0; i < len(data); i += 5 {
		line += int(data[i])
		if data[i] == 0 {
			col += int(data[i+1])
		} else {
			col = int(data[i+1])
		}
		out = append(out, tokenAt{line: line, col: col, length: int(data[i+2]), kind: int(data[i+3])})
	}
	return out
}

// utf16Column counts the UTF-16 code units before a byte offset on its line.
func utf16Column(text string, offset int) (line, col int) {
	start := strings.LastIndexByte(text[:offset], '\n') + 1
	return strings.Count(text[:offset], "\n"), len(utf16.Encode([]rune(text[start:offset])))
}

func TestSemanticTokenColumnsAreUTF16(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name string
		src  string
		// the text of every token the server should produce, in order
		tokens []string
	}{
		{"ascii", "x = \"ab\"; yy = 1\n", []string{"x", `"ab"`, "yy", "1"}},
		{"two bytes", "x = \"é\"; yy = 1\n", []string{"x", `"é"`, "yy", "1"}},
		{"three bytes", "x = \"✓\"; yy = 1\n", []string{"x", `"✓"`, "yy", "1"}},
		{"four bytes, two units", "x = \"🚀🚀\"; yy = 1\n", []string{"x", `"🚀🚀"`, "yy", "1"}},
		{"a comment after text", "n = len(\"é\")  # naïve ✓\n", []string{"n", "len", `"é"`, "# naïve ✓"}},
		{"later lines are unaffected", "a = \"🚀\"\nbb = 2\n", []string{"a", `"🚀"`, "bb", "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := srv.docs.snapshot("file:///tmp/starlsp-test/tokens.star", 1, []byte(tc.src))
			got := decodeTokens(t, srv.semanticTokensFor(d).Data)

			// The expected tokens, each located in the source and measured in
			// UTF-16 units by code written separately from the server's.
			var want []tokenAt
			from := 0
			for _, text := range tc.tokens {
				i := strings.Index(tc.src[from:], text)
				if i < 0 {
					t.Fatalf("%q is not in %q", text, tc.src)
				}
				at := from + i
				line, col := utf16Column(tc.src, at)
				want = append(want, tokenAt{line: line, col: col, length: len(utf16.Encode([]rune(text)))})
				from = at + len(text)
			}

			if len(got) != len(want) {
				t.Fatalf("got %d tokens, want %d: %v", len(got), len(want), got)
			}
			for i := range want {
				if got[i].line != want[i].line || got[i].col != want[i].col || got[i].length != want[i].length {
					t.Errorf("token %d (%q): got line %d col %d length %d, want line %d col %d length %d",
						i, tc.tokens[i], got[i].line, got[i].col, got[i].length, want[i].line, want[i].col, want[i].length)
				}
			}
		})
	}
}

// TestSemanticTokensOfALongLineAreFast: converting columns must not make a long
// line quadratic. A generated file can put thousands of tokens on one line. A
// conversion that walks the line from its start for every token takes 5.6 s for
// the 208 KB line here; the converter takes a few milliseconds.
func TestSemanticTokensOfALongLineAreFast(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var b strings.Builder
	b.WriteString("x = [")
	const items = 20000
	for i := 0; i < items; i++ {
		fmt.Fprintf(&b, "\"é%d\", ", i)
	}
	b.WriteString("]\n")
	d := srv.docs.snapshot("file:///tmp/starlsp-test/long-line.star", 1, []byte(b.String()))

	start := time.Now()
	data := srv.semanticTokensFor(d).Data
	elapsed := time.Since(start)
	t.Logf("%d bytes on one line, %d tokens: %v", b.Len(), len(data)/5, elapsed)
	if elapsed > 2*time.Second {
		t.Errorf("tokens for a %d byte line took %v; column conversion is not linear", b.Len(), elapsed)
	}

	got := decodeTokens(t, data)
	if len(got) < items {
		t.Fatalf("got %d tokens, want at least %d", len(got), items)
	}
	// Spot-check the last string, far along the line.
	last := got[len(got)-1]
	text := b.String()
	at := strings.LastIndex(text, fmt.Sprintf("\"é%d\"", items-1))
	_, col := utf16Column(text, at)
	if last.col != col {
		t.Errorf("the last token starts at column %d, want %d", last.col, col)
	}
}

// TestFoldingRangesComeFromOneVersion: when a buffer's own parse has no usable
// structure, block folds come from the last version that had some. The comment
// folds must come from that version too. A reply that takes blocks from one
// version and comments from another describes a buffer that never existed.
//
// The tree-sitter grammar hoists comments out of an error, so a half-typed
// buffer rarely has comments of its own and no structure. This test builds that
// case by hand, which is the only way to be sure of it.
func TestFoldingRangesComeFromOneVersion(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/fold-versions.star"
	v1 := srv.docs.snapshot(uri, 1, []byte("# one\n# two\ndef f():\n    x = 1\n    return x\n"))
	v2 := srv.docs.snapshot(uri, 2, []byte("\n\n\n# a\n# b\n# c\nx = 1\n"))
	if !v1.usable || len(srv.commentFolds(v2)) == 0 {
		t.Fatal("the two versions do not have the shape this test needs")
	}
	v2.usable, v2.lastGood = false, v1 // v2's own parse had no structure

	got := srv.foldingRanges(v2)
	want := srv.foldingRanges(v1)
	sortFolds(got)
	sortFolds(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the folds mix two versions of the buffer.\ngot  %v\nwant %v (all of version 1)", got, want)
	}
}

func sortFolds(f []FoldingRange) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].StartLine != f[j].StartLine {
			return f[i].StartLine < f[j].StartLine
		}
		return f[i].EndLine < f[j].EndLine
	})
}
