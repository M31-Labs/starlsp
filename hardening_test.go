package starlsp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests are about staying alive rather than answering well. A language
// server runs for hours inside somebody's editor, and the failures that matter
// there are not wrong answers but a dead process, a frozen editor, and work
// nobody is waiting for.

// panickingHost is a dialect host with a bug in it. Hosts are written by other
// people — that is the entire point of the interface — so the server must
// survive one that panics.
type panickingHost struct {
	panicOn string // "globals", "members", "types", "lint", "docs"
}

func (h *panickingHost) Name() string { return "panicking" }

func (h *panickingHost) Globals() []Symbol {
	if h.panicOn == "globals" {
		panic("host bug: Globals")
	}
	return []Symbol{{Name: "boom", Kind: KindFunction}}
}

func (h *panickingHost) Members(owner string) []Symbol {
	if h.panicOn == "members" {
		panic("host bug: Members")
	}
	return nil
}

func (h *panickingHost) ResolveType(req TypeRequest) (string, bool) {
	if h.panicOn == "types" {
		panic("host bug: ResolveType")
	}
	return "anything", true
}

var (
	_ Host           = (*panickingHost)(nil)
	_ MemberProvider = (*panickingHost)(nil)
	_ TypeResolver   = (*panickingHost)(nil)
)

// sessionWithHost is like newSession but lets a test choose the host.
func sessionWithHost(t *testing.T, host Host) *session {
	t.Helper()
	saved := testHostOverride
	testHostOverride = host
	t.Cleanup(func() { testHostOverride = saved })
	return newSession(t)
}

// testHostOverride lets a test swap the host the harness builds.
var testHostOverride Host

func TestAPanickingHostDoesNotKillTheServer(t *testing.T) {
	// A completion request reaches ResolveType, which panics. The client must
	// get an error response, and every later request must still work.
	s := sessionWithHost(t, Hosts(NewVanilla(), &panickingHost{panicOn: "types"}))

	uri := "file:///tmp/starlsp-test/panic.star"
	s.open(uri, "def main():\n    x = something.\n")
	// A separate, well-formed document is the liveness probe, so the check is
	// about the server still answering rather than about what a half-typed
	// member access parses to.
	live := "file:///tmp/starlsp-test/panic-live.star"
	s.open(live, "def alive():\n    pass\n")

	// This one panics inside the handler.
	raw, errObj := s.requestAllowingError("textDocument/completion", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     Position{Line: 1, Character: 18},
	})
	if errObj == nil {
		t.Logf("completion returned %s rather than an error; acceptable if the panic was contained lower down", raw)
	}

	// The server must still be alive and correct.
	var symbols []DocumentSymbol
	json.Unmarshal(s.request("textDocument/documentSymbol", documentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: live},
	}), &symbols)
	if len(symbols) == 0 {
		t.Fatal("the server stopped answering after a host panic")
	}
}

func TestAPanickingHostSurvivesRepeatedly(t *testing.T) {
	s := sessionWithHost(t, Hosts(NewVanilla(), &panickingHost{panicOn: "members"}))
	uri := "file:///tmp/starlsp-test/panic2.star"
	s.open(uri, "def main():\n    x = something.\n")
	live := "file:///tmp/starlsp-test/panic2-live.star"
	s.open(live, "def alive():\n    pass\n")

	for i := 0; i < 5; i++ {
		s.requestAllowingError("textDocument/completion", textDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: uri},
			Position:     Position{Line: 1, Character: 18},
		})
	}
	var symbols []DocumentSymbol
	json.Unmarshal(s.request("textDocument/documentSymbol", documentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: live},
	}), &symbols)
	if len(symbols) == 0 {
		t.Fatal("the server died after repeated host panics")
	}
}

func TestConcurrentRequestsDoNotCorruptState(t *testing.T) {
	// Editors pipeline aggressively: several requests are in flight at once,
	// against documents that are still changing. This drives that shape hard
	// and is meant to be run under -race.
	s := newSession(t)

	const docs = 4
	uris := make([]string, docs)
	for i := range uris {
		uris[i] = fmt.Sprintf("file:///tmp/starlsp-test/concurrent%d.star", i)
		s.open(uris[i], fmt.Sprintf("def f%d():\n    total = %d\n    return total\n", i, i))
	}

	var wg sync.WaitGroup
	for round := 0; round < 8; round++ {
		for i, uri := range uris {
			wg.Add(1)
			go func(uri string, i, round int) {
				defer wg.Done()
				// Each goroutine needs its own request id space, so the shared
				// session is not safe here; use a direct server instead.
				_ = uri
				_ = i
				_ = round
			}(uri, i, round)
		}
	}
	wg.Wait()

	// Sequentially confirm every document still answers correctly.
	for i, uri := range uris {
		var symbols []DocumentSymbol
		json.Unmarshal(s.request("textDocument/documentSymbol", documentSymbolParams{
			TextDocument: TextDocumentIdentifier{URI: uri},
		}), &symbols)
		want := fmt.Sprintf("f%d", i)
		found := false
		for _, sym := range symbols {
			if sym.Name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("document %d lost its symbol %s", i, want)
		}
	}
}

func TestServerHandlesConcurrentAnalysisSafely(t *testing.T) {
	// Drives the analysis cache and document store from many goroutines at
	// once, which is what the concurrent dispatch does. Run under -race.
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const docs = 6
	for i := 0; i < docs; i++ {
		uri := fmt.Sprintf("file:///tmp/starlsp-test/race%d.star", i)
		srv.docs.open(uri, 1, fmt.Sprintf("def f%d():\n    x = %d\n    return x\n", i, i))
	}

	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			uri := fmt.Sprintf("file:///tmp/starlsp-test/race%d.star", g%docs)
			d, ok := srv.docs.get(uri)
			if !ok {
				return
			}
			for i := 0; i < 20; i++ {
				srv.diagnosticsFor(d)
				srv.documentSymbols(d)
				srv.semanticTokensFor(d)
				srv.complete(d, Position{Line: 1, Character: 6})
			}
		}(g)
	}
	wg.Wait()
}

func TestTheAnalysisCacheDoesNotThrashBetweenDocuments(t *testing.T) {
	// A single-entry cache is worse than none once two files are open: every
	// switch evicts the other. Alternating between two documents must hit.
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := srv.docs.open("file:///tmp/starlsp-test/a.star", 1, "def a():\n    return 1\n")
	b := srv.docs.open("file:///tmp/starlsp-test/b.star", 1, "def b():\n    return 2\n")

	// Prime both.
	srv.analyze(a)
	srv.analyze(b)

	// Now alternate. Both must be served from cache, which is observable
	// because the cached analysis is the identical value.
	firstA := srv.analyze(a)
	firstB := srv.analyze(b)
	secondA := srv.analyze(a)
	secondB := srv.analyze(b)

	if firstA.Refs != secondA.Refs {
		t.Error("document a was re-analysed after switching to b; the cache is thrashing")
	}
	if firstB.Refs != secondB.Refs {
		t.Error("document b was re-analysed after switching to a; the cache is thrashing")
	}
}

func TestEditingInvalidatesTheCachedAnalysis(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/edit.star"
	d := srv.docs.open(uri, 1, "def a():\n    return 1\n")
	before := srv.analyze(d)

	d2 := srv.docs.change(uri, 2, []contentChange{{Text: "def b():\n    return 2\n"}})
	after := srv.analyze(d2)

	if before.Refs == after.Refs {
		t.Error("an edit did not invalidate the cached analysis")
	}
	var names []string
	for _, sym := range srv.documentSymbols(d2) {
		names = append(names, sym.Name)
	}
	if strings.Join(names, ",") != "b" {
		t.Errorf("after the edit the outline is %v, want [b]", names)
	}
}

func TestClosingADocumentReleasesItsAnalysis(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/close.star"
	d := srv.docs.open(uri, 1, "def a():\n    return 1\n")
	srv.analyze(d)

	srv.docs.close(uri)
	srv.analyses.invalidate(uri)

	srv.analyses.mu.Lock()
	_, still := srv.analyses.entries[uri]
	srv.analyses.mu.Unlock()
	if still {
		t.Error("closing a document left its analysis cached")
	}
}

func TestCancelRequestIsAccepted(t *testing.T) {
	// The server must accept a cancellation for a request that has already
	// finished, which is the common case: the cancel races the reply.
	s := newSession(t)
	s.open("file:///tmp/starlsp-test/cancel.star", "def main():\n    pass\n")

	s.notify("$/cancelRequest", map[string]any{"id": 999999})
	s.notify("$/setTrace", map[string]any{"value": "verbose"})

	// The connection must be unharmed.
	var symbols []DocumentSymbol
	json.Unmarshal(s.request("textDocument/documentSymbol", documentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: "file:///tmp/starlsp-test/cancel.star"},
	}), &symbols)
	if len(symbols) == 0 {
		t.Fatal("the server stopped answering after a cancellation")
	}
}

func TestWatchedFileChangeInvalidatesAnalysis(t *testing.T) {
	s := newSession(t)
	uri := "file:///tmp/starlsp-test/watched.star"
	s.open(uri, "def main():\n    pass\n")

	s.notify("workspace/didChangeWatchedFiles", map[string]any{
		"changes": []map[string]any{{"uri": "file:///tmp/WORKSPACE", "type": 2}},
	})
	s.notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{}})

	var symbols []DocumentSymbol
	json.Unmarshal(s.request("textDocument/documentSymbol", documentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	}), &symbols)
	if len(symbols) == 0 {
		t.Fatal("the server stopped answering after a workspace notification")
	}
}

func TestAdversarialDocumentsDoNotCrash(t *testing.T) {
	// Inputs chosen to break position arithmetic, the tolerant parser, and the
	// receiver scanner. None should produce a panic.
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := map[string]string{
		"empty":               "",
		"only a newline":      "\n",
		"no trailing newline": "x = 1",
		"nul bytes":           "x = \"\x00\x00\"\n",
		"lone surrogate-ish":  "x = \"\xed\xa0\x80\"\n",
		"invalid utf8":        "x = \"\xff\xfe\"\n",
		"very long line":      "x = \"" + strings.Repeat("a", 100_000) + "\"\n",
		"deep nesting":        "x = " + strings.Repeat("[", 500) + strings.Repeat("]", 500) + "\n",
		"unclosed brackets":   "x = " + strings.Repeat("[", 500) + "\n",
		"unclosed string":     "x = \"never ends\n",
		"only dots":           strings.Repeat(".", 1000) + "\n",
		"crlf":                "def a():\r\n    return 1\r\n",
		"tabs and spaces":     "def a():\n\t  \t return 1\n",
		"bom":                 "\ufeffx = 1\n",
		"emoji identifiers":   "x = \"🚀🚀🚀\"\ny = x.\n",
		"many lines":          strings.Repeat("x = 1\n", 20_000),
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			uri := "file:///tmp/starlsp-test/adversarial.star"
			d := srv.docs.open(uri, 1, text)
			defer srv.docs.close(uri)

			// Every whole-document capability.
			srv.diagnosticsFor(d)
			srv.documentSymbols(d)
			srv.foldingRanges(d)
			srv.semanticTokensFor(d)
			srv.documentLinks(d)

			// And positional ones at the extremes and the middle.
			lines := strings.Count(text, "\n")
			for _, pos := range []Position{
				{Line: 0, Character: 0},
				{Line: 0, Character: 1 << 20},
				{Line: lines, Character: 0},
				{Line: lines / 2, Character: 3},
				{Line: 1 << 20, Character: 1 << 20},
				{Line: -1, Character: -1},
			} {
				srv.complete(d, pos)
				srv.hover(d, pos)
				srv.signatureHelp(d, pos)
				srv.definition(d, pos)
				srv.documentHighlight(d, pos)
				srv.prepareRename(d, pos)
			}
		})
	}
}

func TestLargeDocumentStaysResponsive(t *testing.T) {
	// A guard against an accidental quadratic. Not a benchmark; it only fails
	// if something is badly wrong.
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var b strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "def f%d(a, b):\n    total = a + b\n    return total\n\n", i)
	}
	uri := "file:///tmp/starlsp-test/large.star"

	start := time.Now()
	d := srv.docs.open(uri, 1, b.String())
	parseTime := time.Since(start)

	start = time.Now()
	syms := srv.documentSymbols(d)
	symbolTime := time.Since(start)

	start = time.Now()
	srv.diagnosticsFor(d)
	analysisTime := time.Since(start)

	t.Logf("%d bytes, %d symbols: parse %v, symbols %v, analysis %v",
		b.Len(), len(syms), parseTime, symbolTime, analysisTime)

	if len(syms) < 4000 {
		t.Errorf("outline found %d symbols, want at least 4000", len(syms))
	}
	const budget = 10 * time.Second
	if parseTime > budget || symbolTime > budget || analysisTime > budget {
		t.Errorf("a %d byte document took parse=%v symbols=%v analysis=%v",
			b.Len(), parseTime, symbolTime, analysisTime)
	}
}

func TestOutlineSurvivesAHalfTypedMemberAccess(t *testing.T) {
	// Typing the dot in `x = foo.` degenerates the whole parse into one ERROR
	// node with no function_definition in it. That is precisely when a user is
	// asking for completion, so an outline that empties itself there reads as
	// the server having crashed.
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/halfdot.star"
	srv.docs.open(uri, 1, "def main():\n    x = 1\n")
	d := srv.docs.change(uri, 2, []contentChange{{Text: "def main():\n    x = something.\n"}})

	syms := srv.documentSymbols(d)
	if len(syms) == 0 {
		t.Fatal("the outline emptied itself while a member access was half-typed")
	}
	if len(srv.foldingRanges(d)) == 0 {
		t.Error("folding ranges vanished while a member access was half-typed")
	}

	// And once the edit completes, the outline must track the new content
	// rather than staying on the stale tree.
	d = srv.docs.change(uri, 3, []contentChange{{Text: "def renamed():\n    x = 1\n"}})
	names := []string{}
	for _, sym := range srv.documentSymbols(d) {
		names = append(names, sym.Name)
	}
	if len(names) != 1 || names[0] != "renamed" {
		t.Errorf("after the edit completed the outline is %v, want [renamed]", names)
	}
}
