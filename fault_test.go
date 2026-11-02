package starlsp

// A parse that fails must not cost the buffer its text.
//
// The tree-sitter half of the server is a library with its own bugs, and a
// parse can fail: it can return an error, or it can panic. Neither is the
// user's fault, and neither may change what the server thinks is in the buffer.
// If the server loses one edit, every later edit lands on the wrong text, and
// from then on diagnostics, hover and rename all answer for a file the user
// does not have. Rename is the worst of these, because the editor applies its
// edits to the user's file.
//
// These tests break the parser on purpose and then keep editing.

import (
	"bytes"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"weak"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// lockedBuffer is a buffer that the server's goroutines can write while a test
// reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b.Bytes()...)
}

func (l *lockedBuffer) String() string { return string(l.Bytes()) }

// inlineClient delivers notifications to a server the way Run does: inline, one
// at a time, with a panic turned into a log line.
type inlineClient struct {
	t   *testing.T
	srv *Server
	out *lockedBuffer
	log *lockedBuffer
}

func newInlineClient(t *testing.T) *inlineClient {
	t.Helper()
	c := &inlineClient{t: t, out: &lockedBuffer{}, log: &lockedBuffer{}}
	srv, err := New(Options{Host: NewVanilla(), In: strings.NewReader(""), Out: c.out, Log: c.log})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.srv = srv
	return c
}

func (c *inlineClient) notify(method string, params any) {
	c.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		c.t.Fatalf("marshal %s: %v", method, err)
	}
	if !c.srv.handleInline(request{JSONRPC: "2.0", Method: method, Params: raw}) {
		c.t.Fatalf("%s is not handled inline", method)
	}
}

func (c *inlineClient) open(uri string, version int, text string) {
	c.t.Helper()
	c.notify("textDocument/didOpen", didOpenParams{
		TextDocument: TextDocumentItem{URI: uri, LanguageID: "starlark", Version: version, Text: text},
	})
}

func (c *inlineClient) change(uri string, version int, changes ...contentChange) {
	c.t.Helper()
	c.notify("textDocument/didChange", didChangeParams{
		TextDocument:   VersionedTextDocumentIdentifier{URI: uri, Version: version},
		ContentChanges: changes,
	})
}

func (c *inlineClient) doc(uri string) *Document {
	c.t.Helper()
	d, ok := c.srv.docs.get(uri)
	if !ok {
		c.t.Fatalf("%s is not open", uri)
	}
	return d
}

// diagnosticVersions lists the document versions the server has published
// diagnostics for, in order.
func (c *inlineClient) diagnosticVersions(uri string) []int {
	c.t.Helper()
	var versions []int
	r := newConn(bytes.NewReader(c.out.Bytes()), io.Discard)
	for {
		body, err := r.read()
		if err != nil {
			return versions
		}
		var msg struct {
			Method string                   `json:"method"`
			Params publishDiagnosticsParams `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		if msg.Method == "textDocument/publishDiagnostics" && msg.Params.URI == uri {
			versions = append(versions, msg.Params.Version)
		}
	}
}

// setParser swaps the shared parser, as a stand-in for a parser that has gone
// wrong.
func setParser(s *documentStore, p *gotreesitter.Parser) *gotreesitter.Parser {
	s.parserMu.Lock()
	defer s.parserMu.Unlock()
	old := s.parser
	s.parser = p
	return old
}

// requirePanics checks that the fault the test injects does panic. If the
// library ever stops panicking on it, the test would pass without testing
// anything, so it fails instead.
func requirePanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s no longer panics, so this test no longer injects a fault; pick another way to break the parser", name)
		}
	}()
	fn()
}

func TestAParserPanicDoesNotLoseTheEdit(t *testing.T) {
	// A nil parser panics inside Parse, as a bug in the library would.
	requirePanics(t, "a nil *gotreesitter.Parser", func() { _, _ = (*gotreesitter.Parser)(nil).Parse([]byte("x = 1\n")) })

	c := newInlineClient(t)
	uri := "file:///tmp/starlsp-test/panic.star"
	client := "def a():\n    return 1\n"
	c.open(uri, 1, client)
	if c.doc(uri).tree == nil {
		t.Fatal("the first parse failed before any fault was injected")
	}

	setParser(c.srv.docs, nil)
	c.change(uri, 2, insertAt(Position{}, "x = 1\n"))
	client = "x = 1\n" + client

	d := c.doc(uri)
	if string(d.text) != client || d.version != 2 {
		t.Fatalf("a panic in the parser lost the edit.\nserver v%d %q\nclient v2 %q", d.version, d.text, client)
	}

	// The version that could not be parsed has no tree of its own, so it has
	// no live structure to offer. Its outline is the earlier version's, whole.
	if d.tree != nil || d.root != nil || d.usable {
		t.Error("a version whose parse panicked claims to have a tree")
	}
	if got := strings.Join(symbolNames(c.srv.documentSymbols(d)), ","); got != "a" {
		t.Errorf("outline of the version that could not be parsed is %q, want the earlier version's %q", got, "a")
	}
	if data := c.srv.semanticTokensFor(d).Data; len(data) != 0 {
		t.Errorf("a version without a tree got %d highlighting numbers; stale ranges would colour the wrong text", len(data))
	}
	// Completion reads the text of the caret's line, so it works without a tree.
	if _, ok := labels(c.srv.complete(d, Position{Line: 0, Character: 0}).Items)["len"]; !ok {
		t.Error("completion stopped working on a version whose parse failed")
	}

	// The editor goes on typing against its own copy of the text. The server
	// must not have kept the broken parser, and the next edit must apply to
	// the right text.
	c.change(uri, 3, contentChange{
		Range: &Range{Start: Position{Line: 1, Character: 4}, End: Position{Line: 1, Character: 5}},
		Text:  "renamed",
	})
	client = "x = 1\ndef renamed():\n    return 1\n"

	d = c.doc(uri)
	if string(d.text) != client || d.version != 3 {
		t.Fatalf("the edit after the panic landed on the wrong text.\nserver v%d %q\nclient v3 %q", d.version, d.text, client)
	}
	if d.root == nil {
		t.Fatal("the parser still fails after the panic; the broken parser was kept")
	}
	if got := strings.Join(symbolNames(c.srv.documentSymbols(d)), ","); got != "x,renamed" {
		t.Errorf("outline after the parser recovered is %q, want %q", got, "x,renamed")
	}

	// Diagnostics do not need the tree, so the client still hears about every
	// version, including the one that could not be parsed.
	if got := c.diagnosticVersions(uri); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("diagnostics were published for versions %v, want [1 2 3]", got)
	}
	if !strings.Contains(c.log.String(), "panicked") {
		t.Errorf("the panic left no trace in the server log:\n%s", c.log.String())
	}
}

func TestAParserPanicWhileOpeningStillOpensTheBuffer(t *testing.T) {
	c := newInlineClient(t)
	uri := "file:///tmp/starlsp-test/panic-open.star"
	setParser(c.srv.docs, nil)
	c.open(uri, 1, "def a():\n    return undefined_name\n")

	d := c.doc(uri)
	if string(d.text) != "def a():\n    return undefined_name\n" {
		t.Errorf("the buffer opened with text %q", d.text)
	}
	if d.tree != nil {
		t.Error("the buffer has a tree although the parser panicked")
	}
	// The authoritative pass is independent of the tree-sitter parse.
	got := c.diagnosticVersions(uri)
	if len(got) != 1 {
		t.Fatalf("diagnostics were published %d times, want once", len(got))
	}
	if diags := c.srv.diagnosticsFor(d); !strings.Contains(messages(diags), "undefined: undefined_name") {
		t.Errorf("the resolver's diagnostics are missing: %q", messages(diags))
	}
}

func TestDiagnoseSurvivesAParserPanic(t *testing.T) {
	// Diagnose is what `starlsp --check` and an embedding program call. A panic
	// in the parser must not take that caller down.
	c := newInlineClient(t)
	setParser(c.srv.docs, nil)

	diags := c.srv.Diagnose("/tmp/starlsp-test/diagnose-panic.star", "x = (\n")
	if len(diags) == 0 {
		t.Error("a file with a syntax error got no diagnostics")
	}
}

// TestAParserErrorKeepsEachVersionWhole is the second half of the same rule.
// When Parse returns an error instead of panicking, the new version used to be
// built from its own text and the previous version's tree. Byte offsets from one
// text mean nothing in the other, so the outline read garbage.
func TestAParserErrorKeepsEachVersionWhole(t *testing.T) {
	c := newInlineClient(t)
	uri := "file:///tmp/starlsp-test/error.star"
	c.open(uri, 1, "def alpha():\n    return 1\n")

	good := setParser(c.srv.docs, gotreesitter.NewParser(nil)) // Parse returns ErrNoLanguage
	c.change(uri, 2, insertAt(Position{}, "# 12345\n"))
	setParser(c.srv.docs, good)

	d := c.doc(uri)
	if string(d.text) != "# 12345\ndef alpha():\n    return 1\n" || d.version != 2 {
		t.Fatalf("the edit was lost: v%d %q", d.version, d.text)
	}
	if d.tree != nil {
		t.Error("a version whose parse failed carries the previous version's tree")
	}
	if got := strings.Join(symbolNames(c.srv.documentSymbols(d)), ","); got != "alpha" {
		t.Errorf("outline of the version that could not be parsed is %q, want %q (read from the version that was)", got, "alpha")
	}

	// The next edit parses again and the outline is live.
	c.change(uri, 3, insertAt(Position{}, "# more\n"))
	d = c.doc(uri)
	if d.tree == nil || !d.usable {
		t.Fatal("the parser did not recover")
	}
	if got := strings.Join(symbolNames(c.srv.documentSymbols(d)), ","); got != "alpha" {
		t.Errorf("outline after recovery is %q, want %q", got, "alpha")
	}
	if lg := c.log.String(); !strings.Contains(lg, "error.star") {
		t.Errorf("the failed parse did not say which buffer it was for:\n%s", lg)
	}
}

// TestARunOfFailedParsesDoesNotBuildAChain: each version that has no tree falls
// back to the last version that had one. It must not fall back to the failed
// version before it, or a long run of failures would keep every version alive.
func TestARunOfFailedParsesDoesNotBuildAChain(t *testing.T) {
	c := newInlineClient(t)
	uri := "file:///tmp/starlsp-test/chain.star"
	c.open(uri, 1, "def alpha():\n    return 1\n")
	first := c.doc(uri)

	setParser(c.srv.docs, gotreesitter.NewParser(nil)) // every parse now returns an error
	var refs []weak.Pointer[Document]
	const edits = 20
	for v := 2; v <= edits+1; v++ {
		c.change(uri, v, insertAt(Position{}, "# edit\n"))
		d := c.doc(uri)
		if d.tree != nil {
			t.Fatalf("version %d has a tree although every parse fails", v)
		}
		if d.lastGood != first {
			t.Fatalf("version %d falls back to version %d, want the last version that had a tree (1)", v, d.lastGood.version)
		}
		refs = append(refs, weak.Make(d))
	}

	// Only the latest version and the one it falls back to may be alive.
	collect()
	live := 0
	for _, w := range refs {
		if w.Value() != nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d of %d failed versions are still reachable, want only the current one", live, len(refs))
	}
	runtime.KeepAlive(c)
}
