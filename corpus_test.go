package starlsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real-world Starlark differs from the Starlark anyone writes in a test. Bazel
// BUILD files, .bzl rule libraries, and Tiltfiles are the largest corpora that
// exist, and they exercise shapes a hand-written fixture never reaches:
// thousand-line select() chains, deeply nested dicts, load() with a dozen
// symbols, and string literals holding whole shell scripts.
//
// These tests run against a checked-out corpus when GTS_STARLARK_CORPUS points
// at one, and skip otherwise. The point is not to assert a particular parse; it
// is to prove the server survives, produces no false diagnostics on files that
// are known-good, and never panics.

func corpusRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("GTS_STARLARK_CORPUS")
	if root == "" {
		t.Skip("set GTS_STARLARK_CORPUS to a directory of real Starlark to run corpus tests")
	}
	return root
}

// corpusFiles collects every Starlark file under root.
func corpusFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		base := info.Name()
		switch {
		case strings.HasSuffix(base, ".bzl"),
			strings.HasSuffix(base, ".star"),
			strings.HasSuffix(base, ".sky"),
			base == "BUILD", base == "BUILD.bazel",
			base == "WORKSPACE", base == "WORKSPACE.bazel",
			base == "Tiltfile":
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking corpus: %v", err)
	}
	if len(out) == 0 {
		t.Skipf("no Starlark files under %s", root)
	}
	return out
}

// TestCorpusSurvivesEveryFile opens every corpus file through the full server
// path and requires that nothing panics and every request answers.
func TestCorpusSurvivesEveryFile(t *testing.T) {
	root := corpusRoot(t)
	files := corpusFiles(t, root)
	s := newSession(t)

	var (
		opened   int
		withDiag int
		largest  int
	)
	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(source) > largest {
			largest = len(source)
		}
		uri := pathToURI(path)
		diags := s.open(uri, string(source))
		opened++
		if len(diags) > 0 {
			withDiag++
		}

		// Exercise every whole-document capability on real input.
		for _, method := range []string{
			"textDocument/documentSymbol",
			"textDocument/foldingRange",
			"textDocument/documentLink",
			"textDocument/semanticTokens/full",
		} {
			s.request(method, documentSymbolParams{TextDocument: TextDocumentIdentifier{URI: uri}})
		}
		s.notify("textDocument/didClose", didCloseParams{TextDocument: TextDocumentIdentifier{URI: uri}})
		s.waitDiagnostics(uri)
	}
	t.Logf("opened %d files, largest %d bytes, %d produced diagnostics", opened, largest, withDiag)
}

// TestCorpusProducesNoFalseSyntaxErrors is the sharp one. Every file in a
// released Bazel or Tilt repository parses for the tool that consumes it, so
// any syntax error the server reports on one is the server's bug.
//
// Semantic diagnostics are a different matter and are excluded: a BUILD file
// legitimately calls names that only Bazel predeclares, and the vanilla host
// does not know them. That is what a dialect host is for.
func TestCorpusProducesNoFalseSyntaxErrors(t *testing.T) {
	root := corpusRoot(t)
	files := corpusFiles(t, root)
	s := newSession(t)

	type failure struct {
		path string
		diag Diagnostic
	}
	var failures []failure

	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		uri := pathToURI(path)
		for _, d := range s.open(uri, string(source)) {
			// Undefined-name reports are expected: the vanilla host does not
			// predeclare Bazel's or Tilt's builtins.
			if strings.Contains(d.Message, "undefined") {
				continue
			}
			if d.Severity != severityError {
				continue
			}
			failures = append(failures, failure{path, d})
		}
		s.notify("textDocument/didClose", didCloseParams{TextDocument: TextDocumentIdentifier{URI: uri}})
		s.waitDiagnostics(uri)
	}

	if len(failures) > 0 {
		for i, f := range failures {
			if i >= 20 {
				t.Errorf("... and %d more", len(failures)-20)
				break
			}
			rel, _ := filepath.Rel(root, f.path)
			t.Errorf("%s:%d:%d: %s", rel, f.diag.Range.Start.Line+1, f.diag.Range.Start.Character+1, f.diag.Message)
		}
		t.Fatalf("%d real-world files produced a syntax error; each one is a server defect", len(failures))
	}
}

// TestCorpusPositionalRequestsAtEveryOffset drives hover, completion,
// signature help, definition, and highlight at a stride across each file.
//
// Positional handlers are where a language server crashes: an offset that
// lands mid-rune, inside a broken node, or past the end of a line. Real files
// have far stranger shapes than a fixture does.
func TestCorpusPositionalRequestsAtEveryOffset(t *testing.T) {
	root := corpusRoot(t)
	files := corpusFiles(t, root)
	if testing.Short() {
		t.Skip("corpus positional sweep is slow; skipped under -short")
	}
	s := newSession(t)

	const maxFiles = 60
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}

	requests := 0
	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		uri := pathToURI(path)
		s.open(uri, string(source))

		lines := strings.Split(string(source), "\n")
		for lineNo := 0; lineNo < len(lines); lineNo += 3 {
			line := lines[lineNo]
			for col := 0; col <= len(line); col += 7 {
				pos := Position{Line: lineNo, Character: col}
				for _, method := range []string{
					"textDocument/hover",
					"textDocument/completion",
					"textDocument/signatureHelp",
					"textDocument/definition",
					"textDocument/documentHighlight",
				} {
					s.request(method, textDocumentPositionParams{
						TextDocument: TextDocumentIdentifier{URI: uri},
						Position:     pos,
					})
					requests++
				}
			}
		}
		s.notify("textDocument/didClose", didCloseParams{TextDocument: TextDocumentIdentifier{URI: uri}})
		s.waitDiagnostics(uri)
	}
	t.Logf("drove %d positional requests across %d files with no crash", requests, len(files))
}

// TestCorpusOutOfRangePositions asks for positions that cannot exist. An
// editor sends these during fast typing, when its idea of the buffer is one
// keystroke ahead of the server's.
func TestCorpusOutOfRangePositions(t *testing.T) {
	s := newSession(t)
	uri := "file:///tmp/starlsp-test/oob.star"
	s.open(uri, "def main():\n    x = 1\n")

	for _, pos := range []Position{
		{Line: 0, Character: 0},
		{Line: 0, Character: 9999},
		{Line: 9999, Character: 0},
		{Line: 9999, Character: 9999},
		{Line: -1, Character: -1},
		{Line: 1, Character: -5},
		{Line: 2, Character: 0},
	} {
		for _, method := range []string{
			"textDocument/hover",
			"textDocument/completion",
			"textDocument/signatureHelp",
			"textDocument/definition",
			"textDocument/documentHighlight",
			"textDocument/prepareRename",
		} {
			s.request(method, textDocumentPositionParams{
				TextDocument: TextDocumentIdentifier{URI: uri},
				Position:     pos,
			})
		}
	}
}

// TestUnknownDocumentIsAnswered covers the race where an editor requests a
// document the server has already closed.
func TestUnknownDocumentIsAnswered(t *testing.T) {
	s := newSession(t)
	uri := "file:///tmp/starlsp-test/never-opened.star"

	for _, method := range []string{
		"textDocument/hover",
		"textDocument/completion",
		"textDocument/documentSymbol",
		"textDocument/semanticTokens/full",
	} {
		raw := s.request(method, textDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: uri},
		})
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s on an unopened document returned invalid JSON: %v", method, err)
		}
	}
}
