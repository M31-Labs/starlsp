package starlsp

import (
	"encoding/json"
	"strings"
	"testing"

	"go.starlark.net/syntax"
)

// hostFor is the host the session harness uses. It layers a small fake dialect
// over the specification host, so the tests exercise both halves of the
// interface: what the core knows and what a host adds.
func hostFor(t *testing.T) Host {
	t.Helper()
	return Hosts(NewVanilla(), &fakeHost{})
}

// fakeHost is a minimal dialect: one namespace, one object type behind a
// factory, one lint, and one load target. It exists to prove the Host seam
// works without dragging a real dialect into these tests.
type fakeHost struct{}

func (*fakeHost) Name() string { return "fake" }

func (*fakeHost) Globals() []Symbol {
	return []Symbol{
		{Name: "fs", Kind: KindNamespace, Doc: "Filesystem access."},
		{Name: "shout", Kind: KindFunction, Signature: "shout(msg)", Returns: "None", Doc: "Print loudly."},
	}
}

func (*fakeHost) Members(owner string) []Symbol {
	switch owner {
	case "fs":
		return []Symbol{
			{Name: "path", Owner: "fs", Kind: KindFunction, Signature: "fs.path(p)", Returns: "Path", Doc: "Make a Path."},
		}
	case "Path":
		return []Symbol{
			{Name: "read_text", Owner: "Path", Kind: KindMethod, Signature: "read_text()", Returns: "string", Doc: "Read the file."},
			{Name: "exists", Owner: "Path", Kind: KindMethod, Signature: "exists()", Returns: "bool"},
		}
	}
	return nil
}

func (*fakeHost) ResolveType(req TypeRequest) (string, bool) {
	expr := strings.TrimSpace(req.Expr)
	if expr == "fs" {
		return "fs", true
	}
	if callee, ok := calleeOf(expr); ok && callee == "fs.path" {
		return "Path", true
	}
	return "", false
}

func (*fakeHost) ResolveLoad(req LoadRequest) (string, bool) {
	if req.Target == "known/module" {
		return "/tmp/starlsp-test/known/main.star", true
	}
	return "", false
}

var (
	_ Host           = (*fakeHost)(nil)
	_ MemberProvider = (*fakeHost)(nil)
	_ TypeResolver   = (*fakeHost)(nil)
	_ LoadResolver   = (*fakeHost)(nil)
)

func labels(items []CompletionItem) map[string]CompletionItem {
	out := make(map[string]CompletionItem, len(items))
	for _, item := range items {
		out[item.Label] = item
	}
	return out
}

// at returns src without its "|" caret marker, and the marker's position.
func at(t *testing.T, src string) (string, Position) {
	t.Helper()
	idx := strings.Index(src, "|")
	if idx < 0 {
		t.Fatalf("test source has no caret marker")
	}
	line := strings.Count(src[:idx], "\n")
	lineStart := strings.LastIndex(src[:idx], "\n") + 1
	return src[:idx] + src[idx+1:], Position{Line: line, Character: idx - lineStart}
}

// ---------- the specification host ----------

func TestVanillaIntrospectsTheUniverse(t *testing.T) {
	v := NewVanilla()
	names := make(map[string]Symbol)
	for _, sym := range v.Globals() {
		names[sym.Name] = sym
	}

	// Every name here is required by the specification.
	for _, want := range []string{"len", "range", "print", "fail", "True", "False", "None", "dict", "list"} {
		if _, ok := names[want]; !ok {
			t.Errorf("universe is missing %q", want)
		}
	}
	if names["len"].Signature == "" {
		t.Error("len has no signature; the specification documentation is not attached")
	}
	if names["list"].Kind != KindType {
		t.Errorf("list kind = %v, want KindType", names["list"].Kind)
	}
	if names["def"].Kind != KindKeyword {
		t.Errorf("def kind = %v, want KindKeyword", names["def"].Kind)
	}
}

func TestVanillaIntrospectsBuiltinMethods(t *testing.T) {
	v := NewVanilla()
	for owner, want := range map[string]string{
		"string": "startswith",
		"list":   "append",
		"dict":   "setdefault",
		"set":    "union",
	} {
		found := false
		for _, sym := range v.Members(owner) {
			if sym.Name == want {
				found = true
				if sym.Owner != owner {
					t.Errorf("%s.%s has owner %q", owner, want, sym.Owner)
				}
			}
		}
		if !found {
			t.Errorf("%s is missing the method %q", owner, want)
		}
	}
}

func TestHostsCompositionLetsALaterHostWin(t *testing.T) {
	base := &fixedHost{name: "base", globals: []Symbol{{Name: "shared", Doc: "from base"}, {Name: "only_base"}}}
	over := &fixedHost{name: "over", globals: []Symbol{{Name: "shared", Doc: "from over"}}}

	combined := Hosts(base, over)
	found := make(map[string]Symbol)
	for _, sym := range combined.Globals() {
		found[sym.Name] = sym
	}
	if found["shared"].Doc != "from over" {
		t.Errorf("shared doc = %q, want the later host to win", found["shared"].Doc)
	}
	if _, ok := found["only_base"]; !ok {
		t.Error("composition dropped a name only the base host declares")
	}
	if combined.Name() != "base+over" {
		t.Errorf("composite name = %q", combined.Name())
	}
}

type fixedHost struct {
	name    string
	globals []Symbol
}

func (h *fixedHost) Name() string      { return h.name }
func (h *fixedHost) Globals() []Symbol { return h.globals }

// ---------- capabilities ----------

func TestInitializeAdvertisesEveryCapability(t *testing.T) {
	s := newSession(t)
	raw := s.request("initialize", initializeParams{})

	var result initializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal initialize: %v", err)
	}
	c := result.Capabilities

	if c.TextDocumentSync != 2 {
		t.Errorf("TextDocumentSync = %d, want 2 (incremental)", c.TextDocumentSync)
	}
	for name, present := range map[string]bool{
		"completion":        c.CompletionProvider != nil,
		"hover":             c.HoverProvider,
		"signatureHelp":     c.SignatureHelpProvider != nil,
		"definition":        c.DefinitionProvider,
		"references":        c.ReferencesProvider,
		"documentHighlight": c.DocumentHighlightProvider,
		"rename":            c.RenameProvider != nil,
		"prepareRename":     c.RenameProvider != nil && c.RenameProvider.PrepareProvider,
		"documentSymbol":    c.DocumentSymbolProvider,
		"workspaceSymbol":   c.WorkspaceSymbolProvider,
		"foldingRange":      c.FoldingRangeProvider,
		"selectionRange":    c.SelectionRangeProvider,
		"documentLink":      c.DocumentLinkProvider != nil,
		"semanticTokens":    c.SemanticTokensProvider != nil,
	} {
		if !present {
			t.Errorf("capability %q not advertised", name)
		}
	}
}

func TestCompletionOffersUniverseAndHostNames(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    |\n")
	uri := "file:///tmp/starlsp-test/globals.star"
	s.open(uri, src)

	var list completionList
	json.Unmarshal(s.request("textDocument/completion", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	}), &list)
	got := labels(list.Items)

	for _, want := range []string{"len", "range", "fs", "shout", "main"} {
		if _, ok := got[want]; !ok {
			t.Errorf("completion missing %q", want)
		}
	}
	if got["main"].SortText == "" || got["main"].SortText[0] != '0' {
		t.Errorf("a name defined in the file should sort first; got SortText %q", got["main"].SortText)
	}
}

func TestCompletionAfterStringLiteralOffersStringMethods(t *testing.T) {
	// This works with no host at all: the type comes from the specification.
	s := newSession(t)
	src, pos := at(t, "def main():\n    x = \"hello\".|\n")
	uri := "file:///tmp/starlsp-test/strmethods.star"
	s.open(uri, src)

	var list completionList
	json.Unmarshal(s.request("textDocument/completion", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	}), &list)
	got := labels(list.Items)

	for _, want := range []string{"upper", "startswith", "split"} {
		if _, ok := got[want]; !ok {
			t.Errorf("completion after a string literal missing %q", want)
		}
	}
}

func TestCompletionFollowsHostFactoryReturn(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    p = fs.path(\"/etc/hosts\")\n    p.|\n")
	uri := "file:///tmp/starlsp-test/factory.star"
	s.open(uri, src)

	var list completionList
	json.Unmarshal(s.request("textDocument/completion", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	}), &list)
	got := labels(list.Items)

	for _, want := range []string{"read_text", "exists"} {
		if _, ok := got[want]; !ok {
			t.Errorf("completion after `p.` missing %q; the binding was not followed to the factory", want)
		}
	}
}

func TestCompletionIsSilentOnAnUnknownReceiver(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    mystery.|\n")
	uri := "file:///tmp/starlsp-test/unknown.star"
	s.open(uri, src)

	var list completionList
	json.Unmarshal(s.request("textDocument/completion", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	}), &list)

	if len(list.Items) != 0 {
		t.Errorf("an unresolved receiver offered %d items; a wrong list is worse than none", len(list.Items))
	}
}

func TestHoverShowsSpecificationDocumentation(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    n = le|n([1, 2])\n")
	uri := "file:///tmp/starlsp-test/hover.star"
	s.open(uri, src)

	raw := s.request("textDocument/hover", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if string(raw) == "null" {
		t.Fatal("hover on len returned null")
	}
	var got hoverResult
	json.Unmarshal(raw, &got)
	if !strings.Contains(got.Contents.Value, "len(x)") {
		t.Errorf("hover does not show the signature: %q", got.Contents.Value)
	}
}

// ---------- the capabilities the previous generation lacked ----------

func TestReferencesFindEveryUseOfALocal(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    cou|nt = 1\n    count = count + 1\n    print(count)\n")
	uri := "file:///tmp/starlsp-test/refs.star"
	s.open(uri, src)

	params := referenceParams{TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos}
	params.Context.IncludeDeclaration = true

	var locs []Location
	json.Unmarshal(s.request("textDocument/references", params), &locs)

	// count appears four times: the binding, both sides of the increment, and
	// the print.
	if len(locs) != 4 {
		t.Errorf("found %d references to count, want 4: %+v", len(locs), locs)
	}
	if len(locs) > 1 && locs[0].Range.Start.Line > locs[1].Range.Start.Line {
		t.Error("references are not sorted by position")
	}
}

func TestReferencesDistinguishTwoLocalsWithOneName(t *testing.T) {
	// A local x in one function is a different variable from a local x in
	// another. Matching by text would merge them; matching by binding does not.
	s := newSession(t)
	src, pos := at(t, "def a():\n    x| = 1\n    return x\n\ndef b():\n    x = 2\n    return x\n")
	uri := "file:///tmp/starlsp-test/shadow.star"
	s.open(uri, src)

	params := referenceParams{TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos}
	params.Context.IncludeDeclaration = true

	var locs []Location
	json.Unmarshal(s.request("textDocument/references", params), &locs)

	if len(locs) != 2 {
		t.Errorf("found %d references, want 2 — the other function's x leaked in", len(locs))
	}
	for _, loc := range locs {
		if loc.Range.Start.Line > 2 {
			t.Errorf("reference at line %d belongs to the other function", loc.Range.Start.Line)
		}
	}
}

func TestDocumentHighlightMarksTheBindingAsAWrite(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    total = 0\n    print(tot|al)\n")
	uri := "file:///tmp/starlsp-test/highlight.star"
	s.open(uri, src)

	var hs []DocumentHighlight
	json.Unmarshal(s.request("textDocument/documentHighlight", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	}), &hs)

	if len(hs) != 2 {
		t.Fatalf("got %d highlights, want 2", len(hs))
	}
	var writes int
	for _, h := range hs {
		if h.Kind == highlightWrite {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("got %d write highlights, want exactly 1 (the binding)", writes)
	}
}

func TestRenameRewritesEveryUse(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    tot|al = 0\n    total = total + 1\n    return total\n")
	uri := "file:///tmp/starlsp-test/rename.star"
	s.open(uri, src)

	var edit WorkspaceEdit
	json.Unmarshal(s.request("textDocument/rename", renameParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos, NewName: "sum",
	}), &edit)

	edits := edit.Changes[uri]
	if len(edits) != 4 {
		t.Errorf("rename produced %d edits, want 4", len(edits))
	}
	for _, e := range edits {
		if e.NewText != "sum" {
			t.Errorf("edit inserts %q", e.NewText)
		}
	}
}

func TestRenameRefusesAPredeclaredName(t *testing.T) {
	// Renaming len would rewrite call sites without changing what they call.
	s := newSession(t)
	src, pos := at(t, "def main():\n    return le|n([1])\n")
	uri := "file:///tmp/starlsp-test/rename_builtin.star"
	s.open(uri, src)

	raw := s.request("textDocument/prepareRename", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if string(raw) != "null" {
		t.Errorf("prepareRename allowed renaming a predeclared name: %s", raw)
	}
}

func TestPrepareRenameAllowsALocal(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    tot|al = 0\n    return total\n")
	uri := "file:///tmp/starlsp-test/prepare.star"
	s.open(uri, src)

	raw := s.request("textDocument/prepareRename", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: pos,
	})
	if string(raw) == "null" {
		t.Fatal("prepareRename refused a local binding")
	}
	var got prepareRenameResult
	json.Unmarshal(raw, &got)
	if got.Placeholder != "total" {
		t.Errorf("placeholder = %q, want total", got.Placeholder)
	}
}

func TestSelectionRangeExpandsOutward(t *testing.T) {
	s := newSession(t)
	src, pos := at(t, "def main():\n    x = fo|o(1, 2)\n")
	uri := "file:///tmp/starlsp-test/selection.star"
	s.open(uri, src)

	var ranges []SelectionRange
	json.Unmarshal(s.request("textDocument/selectionRange", selectionRangeParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Positions: []Position{pos},
	}), &ranges)

	if len(ranges) != 1 {
		t.Fatalf("got %d selection ranges, want 1", len(ranges))
	}
	depth := 0
	for r := &ranges[0]; r != nil; r = r.Parent {
		depth++
	}
	if depth < 3 {
		t.Errorf("selection chain has depth %d; expand-selection needs several steps", depth)
	}
}

func TestSemanticTokensClassifyHostNamespaces(t *testing.T) {
	s := newSession(t)
	uri := "file:///tmp/starlsp-test/tokens.star"
	s.open(uri, "# note\ndef main():\n    fs.path(\"/tmp\")\n")

	var tokens semanticTokens
	json.Unmarshal(s.request("textDocument/semanticTokens/full", semanticTokensParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	}), &tokens)

	if len(tokens.Data) == 0 || len(tokens.Data)%5 != 0 {
		t.Fatalf("token data length %d is not a positive multiple of 5", len(tokens.Data))
	}
	if tokens.Data[3] != uint32(tokComment) {
		t.Errorf("first token kind = %d, want comment (%d)", tokens.Data[3], tokComment)
	}
	var sawNamespace bool
	for i := 3; i < len(tokens.Data); i += 5 {
		if tokens.Data[i] == uint32(tokNamespace) {
			sawNamespace = true
		}
	}
	if !sawNamespace {
		t.Error("the host namespace fs was not classified as a namespace")
	}
}

func TestDocumentLinkUsesTheHostLoadResolver(t *testing.T) {
	s := newSession(t)
	uri := "file:///tmp/starlsp-test/links.star"
	s.open(uri, "load(\"known/module\", \"thing\")\nload(\"unknown/module\", \"other\")\n")

	var links []DocumentLink
	json.Unmarshal(s.request("textDocument/documentLink", documentLinkParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	}), &links)

	if len(links) != 2 {
		t.Fatalf("got %d links, want 2", len(links))
	}
	var resolved int
	for _, l := range links {
		if l.Target != "" {
			resolved++
		}
	}
	if resolved != 1 {
		t.Errorf("%d links resolved to a target, want exactly the known one", resolved)
	}
}

// ---------- diagnostics ----------

func TestDiagnosticsComeFromTheStarlarkParser(t *testing.T) {
	s := newSession(t)
	diags := s.open("file:///tmp/starlsp-test/broken.star", "def main(:\n    pass\n")

	if len(diags) == 0 {
		t.Fatal("a syntax error produced no diagnostics")
	}
	var authoritative bool
	for _, d := range diags {
		if d.Source == sourceStarlark {
			authoritative = true
		}
	}
	if !authoritative {
		t.Errorf("no diagnostic attributed to the Starlark parser: %+v", diags)
	}
}

func TestHostNamesAreNotReportedAsUndefined(t *testing.T) {
	s := newSession(t)
	diags := s.open("file:///tmp/starlsp-test/hostnames.star",
		"def main():\n    shout(\"hi\")\n    p = fs.path(\"/x\")\n    missing_name()\n")

	var sawMissing bool
	for _, d := range diags {
		if strings.Contains(d.Message, "missing_name") {
			sawMissing = true
		}
		for _, hostName := range []string{"shout", "fs"} {
			if strings.Contains(d.Message, hostName) {
				t.Errorf("host name %q reported as a problem: %s", hostName, d.Message)
			}
		}
	}
	if !sawMissing {
		t.Errorf("an undefined name produced no diagnostic: %+v", diags)
	}
}

func TestControlFlowOutsideAFunctionIsReported(t *testing.T) {
	// Under the specification dialect this is an error, and the message comes
	// from the resolver rather than from anything this package writes.
	s := newSession(t)
	diags := s.open("file:///tmp/starlsp-test/toplevel.star", "x = 1\nif x:\n    print(1)\n")

	var found int
	for _, d := range diags {
		if strings.Contains(d.Message, "not within a function") {
			found++
		}
	}
	if found == 0 {
		t.Errorf("a top-level if produced no diagnostic: %+v", diags)
	}
	if found > 1 {
		t.Errorf("a top-level if was reported %d times; the server is double-reporting", found)
	}
}

func TestTopLevelControlIsAllowedWhenTheDialectSaysSo(t *testing.T) {
	// Tilt permits if and for at the top level. A server that reports an error
	// on a file the runtime accepts is worse than one that reports nothing.
	srv, err := New(Options{Host: Hosts(NewVanilla(), &permissiveHost{})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := srv.docs.open("file:///tmp/starlsp-test/permissive.star", 1, "x = 1\nif x:\n    print(1)\n")

	for _, diag := range srv.diagnosticsFor(d) {
		if strings.Contains(diag.Message, "not within a function") {
			t.Errorf("top-level control reported despite the dialect allowing it: %s", diag.Message)
		}
	}
}

// permissiveHost enables the options a Tilt-like dialect needs.
type permissiveHost struct{}

func (*permissiveHost) Name() string      { return "permissive" }
func (*permissiveHost) Globals() []Symbol { return nil }
func (*permissiveHost) FileOptions() *syntax.FileOptions {
	return &syntax.FileOptions{TopLevelControl: true, While: true, GlobalReassign: true}
}

func TestCleanFileHasNoErrors(t *testing.T) {
	s := newSession(t)
	diags := s.open("file:///tmp/starlsp-test/clean.star",
		"NAMES = [\"a\", \"b\"]\n\ndef main():\n    for n in NAMES:\n        print(n.upper())\n")

	for _, d := range diags {
		if d.Severity == severityError {
			t.Errorf("clean file produced an error: %s at %+v", d.Message, d.Range.Start)
		}
	}
}

// ---------- structure ----------

func TestOutlineSurvivesABrokenBuffer(t *testing.T) {
	s := newSession(t)
	uri := "file:///tmp/starlsp-test/half.star"
	s.open(uri, "def ready():\n    pass\n\ndef broken(:\n")

	var symbols []DocumentSymbol
	json.Unmarshal(s.request("textDocument/documentSymbol", documentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	}), &symbols)

	var sawReady bool
	for _, sym := range symbols {
		if sym.Name == "ready" {
			sawReady = true
		}
	}
	if !sawReady {
		t.Errorf("the outline lost a valid function because a later one is broken: %+v", symbols)
	}
}

func TestIncrementalEditKeepsPositionsCorrect(t *testing.T) {
	s := newSession(t)
	uri := "file:///tmp/starlsp-test/incremental.star"
	s.open(uri, "def main():\n    pass\n")

	s.notify("textDocument/didChange", didChangeParams{
		TextDocument: VersionedTextDocumentIdentifier{URI: uri, Version: 2},
		ContentChanges: []contentChange{{
			Range: &Range{Start: Position{Line: 1, Character: 4}, End: Position{Line: 1, Character: 8}},
			Text:  "fs.",
		}},
	})
	s.waitDiagnostics(uri)

	var list completionList
	json.Unmarshal(s.request("textDocument/completion", textDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri}, Position: Position{Line: 1, Character: 7},
	}), &list)

	if _, ok := labels(list.Items)["path"]; !ok {
		t.Error("after an incremental edit, `fs.` did not complete")
	}
}

func TestUTF16PositionsRoundTrip(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := srv.docs.open("file:///tmp/starlsp-test/utf16.star", 1, "x = \"é🚀\"\ny = 1\n")

	for offset := 0; offset <= len(d.text); offset++ {
		if d.text != nil && offset < len(d.text) && d.text[offset]&0xC0 == 0x80 {
			continue // inside a rune; it has no position of its own
		}
		pos := d.positionOf(offset)
		if back := d.offsetOf(pos); back != offset {
			t.Errorf("offset %d round-tripped to %d via %+v", offset, back, pos)
		}
	}
}
