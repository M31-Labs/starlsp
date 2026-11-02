package starlsp

// The server answers read-only requests on their own goroutines while didChange
// is handled inline, so an edit and a request for the same buffer routinely
// overlap. These tests drive that overlap on purpose. They are written against
// the wire protocol and a few fields of Document, so they say the same thing
// about any version of the server, and are meant to be run under -race.
//
// The earlier hardening tests do not do this. They hold one Document and read
// it from many goroutines, which never edits anything, and a placeholder test
// there starts goroutines that do nothing.

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// ---------- a reference model of one buffer ----------

// docModel is the client's own record of a buffer. It applies edits with code
// written separately from the server's, so a mistake in the server's edit
// handling shows up as a disagreement at the end.
type docModel struct {
	uri     string
	text    string
	version int
	defs    int // functions defined so far: f0 .. f{defs-1}
	step    int
}

const (
	brokenTop  = "foo(" // an unclosed call: the whole parse loses its structure
	brokenTail = "x = fs."
)

func newDocModel(i int) *docModel {
	return &docModel{
		uri:     fmt.Sprintf("file:///tmp/starlsp-test/interleave%d.star", i),
		text:    "load(\"known/module\", \"thing\")\nNAMES = [\"a\", \"b\"]\n\n",
		version: 1,
	}
}

func defText(n int) string {
	return fmt.Sprintf("def f%d(a, b):\n    total = a + b\n    return total\n\n", n)
}

// refOffset converts an LSP position to a byte offset, counting columns in
// UTF-16 code units.
func refOffset(text string, p Position) int {
	off := 0
	for line := 0; line < p.Line; line++ {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			return len(text)
		}
		off += i + 1
	}
	units := 0
	for _, r := range text[off:] {
		if r == '\n' || units >= p.Character {
			break
		}
		units += len(utf16.Encode([]rune{r}))
		off += len(string(r))
	}
	return off
}

func (m *docModel) eof() Position {
	return Position{Line: strings.Count(m.text, "\n"), Character: 0}
}

// lineOf returns the index of the first line whose content is exactly want, or
// -1.
func (m *docModel) lineOf(want string) int {
	for i, line := range strings.Split(m.text, "\n") {
		if line == want {
			return i
		}
	}
	return -1
}

func (m *docModel) has(want string) bool { return m.lineOf(want) >= 0 }

func (m *docModel) broken() bool { return m.has(brokenTop) || m.has(brokenTail) }

func insertAt(p Position, text string) contentChange {
	return contentChange{Range: &Range{Start: p, End: p}, Text: text}
}

func deleteLine(line int) contentChange {
	return contentChange{Range: &Range{
		Start: Position{Line: line},
		End:   Position{Line: line + 1},
	}}
}

// apply records changes in the model, one at a time, as the protocol says.
func (m *docModel) apply(changes []contentChange) {
	for _, ch := range changes {
		if ch.Range == nil {
			m.text = ch.Text
			continue
		}
		start, end := refOffset(m.text, ch.Range.Start), refOffset(m.text, ch.Range.End)
		m.text = m.text[:start] + ch.Text + m.text[end:]
	}
}

// edits is the script every document follows, each starting at a different point
// so the documents are not in step. Several of them leave the buffer unable to
// parse as Starlark, and several shift every later line.
var edits = []string{
	"append", "comment", "rename", "break-tail", "heal", "append", "break-top",
	"heal", "append+comment", "replace", "break-tail", "heal", "comment",
}

// next returns the next edit for the buffer and applies it to the model.
func (m *docModel) next(offset int) []contentChange {
	kind := edits[(m.step+offset)%len(edits)]
	m.step++

	var changes []contentChange
	switch {
	case kind == "append" && !m.broken():
		changes = []contentChange{insertAt(m.eof(), defText(m.defs))}
		m.defs++
	case kind == "append+comment" && !m.broken():
		changes = []contentChange{
			insertAt(m.eof(), defText(m.defs)),
			insertAt(Position{}, "# 🚀 note\n"), // the second range refers to the text after the first
		}
		m.defs++
	case kind == "comment" && !m.broken():
		changes = []contentChange{insertAt(Position{}, "# 🚀 note\n")}
	case kind == "rename" && (strings.HasPrefix(m.text, "# 🚀 note\n") || strings.HasPrefix(m.text, "# 🚀 memo\n")):
		// Columns 5 to 9 are the last word of the first line once the rocket has
		// counted for two UTF-16 units. A server that counted bytes or runes
		// would cut the wrong characters.
		word := "memo"
		if strings.HasPrefix(m.text, "# 🚀 memo\n") {
			word = "note"
		}
		changes = []contentChange{{
			Range: &Range{Start: Position{Line: 0, Character: 5}, End: Position{Line: 0, Character: 9}},
			Text:  word,
		}}
	case kind == "break-tail" && !m.broken():
		changes = []contentChange{insertAt(m.eof(), brokenTail+"\n")}
	case kind == "break-top" && !m.broken():
		changes = []contentChange{insertAt(Position{}, brokenTop+"\n")}
	case kind == "heal" && m.broken():
		line := m.lineOf(brokenTop)
		if line < 0 {
			line = m.lineOf(brokenTail)
		}
		changes = []contentChange{deleteLine(line)}
	case kind == "replace" && !m.broken():
		next := m.text + defText(m.defs) // a whole-document change: no range
		m.defs++
		changes = []contentChange{{Text: next}}
	default:
		if m.broken() {
			return m.next(offset) // keep going until something applies
		}
		changes = []contentChange{insertAt(m.eof(), defText(m.defs))}
		m.defs++
	}
	m.apply(changes)
	return changes
}

// restore leaves the buffer able to parse, so the last answer can be checked
// exactly.
func (m *docModel) restore() []contentChange {
	if !m.broken() {
		return nil
	}
	line := m.lineOf(brokenTop)
	if line < 0 {
		line = m.lineOf(brokenTail)
	}
	changes := []contentChange{deleteLine(line)}
	m.apply(changes)
	return changes
}

// position of the n-th occurrence of needle, plus skip columns into it.
func (m *docModel) at(needle string, n, skip int) Position {
	off := -1
	rest := m.text
	base := 0
	for i := 0; i <= n; i++ {
		j := strings.Index(rest, needle)
		if j < 0 {
			return Position{}
		}
		off = base + j
		base += j + len(needle)
		rest = m.text[base:]
	}
	line := strings.Count(m.text[:off], "\n")
	lineStart := strings.LastIndex(m.text[:off], "\n") + 1
	col := len(utf16.Encode([]rune(m.text[lineStart:off]))) + skip
	return Position{Line: line, Character: col}
}

// ---------- the client ----------

type sent struct {
	method string
	doc    int
}

// requestKinds are the requests sent for every buffer in every round. Each one
// reads the Document it is given: the text, the line index, the tree, or the
// analysis.
var requestKinds = []string{
	"textDocument/hover",
	"textDocument/completion",
	"textDocument/definition",
	"textDocument/documentSymbol",
	"textDocument/semanticTokens/full",
	"textDocument/foldingRange",
	"textDocument/documentHighlight",
	"textDocument/references",
	"textDocument/selectionRange",
	"textDocument/documentLink",
	"textDocument/signatureHelp",
	"textDocument/prepareRename",
	"textDocument/rename",
	"workspace/symbol",
}

func requestParams(method string, uri string, p Position) any {
	td := TextDocumentIdentifier{URI: uri}
	switch method {
	case "textDocument/documentSymbol", "textDocument/foldingRange",
		"textDocument/documentLink", "textDocument/semanticTokens/full":
		return documentSymbolParams{TextDocument: td}
	case "textDocument/references":
		r := referenceParams{TextDocument: td, Position: p}
		r.Context.IncludeDeclaration = true
		return r
	case "textDocument/selectionRange":
		return selectionRangeParams{TextDocument: td, Positions: []Position{p}}
	case "textDocument/rename":
		return renameParams{TextDocument: td, Position: p, NewName: "renamed"}
	case "workspace/symbol":
		return workspaceSymbolParams{Query: "f"}
	default:
		return textDocumentPositionParams{TextDocument: td, Position: p}
	}
}

func rpc(id int, method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func rpcNotification(method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
}

var fnName = regexp.MustCompile(`^f[0-9]+$`)

// checkOutline requires the symbol names to be ones the buffer has ever had, in
// order: NAMES, then f0, f1, and so on without a gap. A Document that mixed one
// version's tree with another's text would produce other names, because
// the tree's offsets would cut the wrong characters out of the text.
func checkOutline(names []string, maxDefs int) error {
	next := 0
	for _, n := range names {
		switch {
		case n == "NAMES":
		case fnName.MatchString(n):
			var k int
			fmt.Sscanf(n, "f%d", &k)
			if k != next {
				return fmt.Errorf("outline %v: %s is out of order, want f%d", names, n, next)
			}
			next++
		default:
			return fmt.Errorf("outline %v: %q is not a name this buffer ever had", names, n)
		}
	}
	if next > maxDefs {
		return fmt.Errorf("outline %v has more functions than the buffer ever did (%d)", names, maxDefs)
	}
	return nil
}

func checkTokens(data []uint32) error {
	if len(data)%5 != 0 {
		return fmt.Errorf("token data length %d is not a multiple of 5", len(data))
	}
	for i := 0; i < len(data); i += 5 {
		length, kind := data[i+2], data[i+3]
		if int(kind) >= len(semanticTokenTypes) {
			return fmt.Errorf("token %d has type %d outside the legend", i/5, kind)
		}
		if length == 0 || length > 1<<20 {
			return fmt.Errorf("token %d has length %d", i/5, length)
		}
	}
	return nil
}

func validRange(r Range) bool {
	a, b := r.Start, r.End
	return a.Line >= 0 && a.Character >= 0 && (a.Line < b.Line || (a.Line == b.Line && a.Character <= b.Character))
}

// TestInterleavedEditsAndRequestsDoNotRace sends several buffers a steady stream
// of edits, some of which break the syntax, while a flood of requests for the
// same buffers is in flight. Under -race it fails on any unsynchronised access
// between an edit and a request. Whether or not the race detector is on, it also
// fails when a request is answered with something no version of the buffer could
// have produced, or an edit is applied wrongly.
func TestInterleavedEditsAndRequestsDoNotRace(t *testing.T) {
	const docs = 6
	rounds := 30
	if testing.Short() {
		rounds = 10
	}

	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	srv, err := New(Options{Host: Hosts(NewVanilla(), &fakeHost{}), In: toServerR, Out: fromServerW, Log: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run() }()
	client := newConn(fromServerR, toServerW)

	// A stuck server must fail the test rather than hang the run.
	watchdog := time.AfterFunc(3*time.Minute, func() {
		toServerW.CloseWithError(io.ErrClosedPipe)
		fromServerR.CloseWithError(io.ErrClosedPipe)
	})
	defer watchdog.Stop()

	models := make([]*docModel, docs)
	for i := range models {
		models[i] = newDocModel(i)
	}

	// Plan the whole conversation before sending any of it, so the goroutine that
	// sends and the one that checks share only read-only data.
	var (
		plan      []map[string]any
		pending   = map[int]sent{}
		nextID    = 1
		finalID   = map[int]int{} // doc -> id of its last outline request
		finalDefs = map[int]int{} // doc -> functions it ends with
	)
	add := func(method string, doc int, params any) {
		id := nextID
		nextID++
		pending[id] = sent{method: method, doc: doc}
		plan = append(plan, rpc(id, method, params))
	}
	plan = append(plan, rpc(0, "initialize", initializeParams{}))
	pending[0] = sent{method: "initialize"}
	plan = append(plan, rpcNotification("initialized", struct{}{}))
	for _, m := range models {
		plan = append(plan, rpcNotification("textDocument/didOpen", didOpenParams{
			TextDocument: TextDocumentItem{URI: m.uri, LanguageID: "starlark", Version: m.version, Text: m.text},
		}))
	}

	lastVersion := map[string]int{}
	for round := 0; round < rounds; round++ {
		for i, m := range models {
			// A burst of requests for one buffer, with its next edit right behind
			// them. The server starts a goroutine for each request and goes on to
			// the edit, so the edit lands while those goroutines are reading the
			// buffer. Two positions: on `total` in a function body, and on its use.
			decl := m.at("total", 0, 2)
			use := m.at("return total", 0, 9)
			for _, method := range requestKinds {
				p := decl
				if method == "textDocument/definition" || method == "textDocument/references" {
					p = use
				}
				if method == "textDocument/signatureHelp" {
					p = m.at("load(", 0, 5)
				}
				add(method, i, requestParams(method, m.uri, p))
			}

			changes := m.next(i * 5)
			m.version++
			lastVersion[m.uri] = m.version
			plan = append(plan, rpcNotification("textDocument/didChange", didChangeParams{
				TextDocument:   VersionedTextDocumentIdentifier{URI: m.uri, Version: m.version},
				ContentChanges: changes,
			}))
		}
	}
	// Finish with a buffer that parses, then ask for its outline once more.
	for i, m := range models {
		if changes := m.restore(); changes != nil {
			m.version++
			lastVersion[m.uri] = m.version
			plan = append(plan, rpcNotification("textDocument/didChange", didChangeParams{
				TextDocument:   VersionedTextDocumentIdentifier{URI: m.uri, Version: m.version},
				ContentChanges: changes,
			}))
		}
		finalID[i] = nextID
		finalDefs[i] = m.defs
		add("textDocument/documentSymbol", i, requestParams("textDocument/documentSymbol", m.uri, Position{}))
	}

	go func() {
		for _, msg := range plan {
			if err := client.write(msg); err != nil {
				return
			}
		}
	}()

	var (
		responses    = map[int]json.RawMessage{}
		diagVersions = map[string][]int{}
		failures     []string
		nonEmpty     int
		hovers       int
	)
	fail := func(format string, args ...any) {
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}

	for len(responses) < len(pending) {
		body, err := client.read()
		if err != nil {
			t.Fatalf("reading from the server after %d of %d replies: %v", len(responses), len(pending), err)
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *responseError  `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("unreadable message from the server: %v\n%s", err, body)
		}
		if msg.ID == nil {
			if msg.Method == "textDocument/publishDiagnostics" {
				var p publishDiagnosticsParams
				if err := json.Unmarshal(msg.Params, &p); err == nil {
					diagVersions[p.URI] = append(diagVersions[p.URI], p.Version)
				}
			}
			continue
		}
		req, known := pending[*msg.ID]
		if !known {
			fail("reply to an id nobody sent: %d", *msg.ID)
			continue
		}
		if _, dup := responses[*msg.ID]; dup {
			fail("two replies to request %d (%s)", *msg.ID, req.method)
		}
		responses[*msg.ID] = msg.Result
		if msg.Error != nil {
			// Refusing to rename a name the file does not bind is an answer, not a failure.
			if req.method == "textDocument/rename" && msg.Error.Code == errInvalidRequest {
				continue
			}
			fail("%s answered with an error: %d %s", req.method, msg.Error.Code, msg.Error.Message)
			continue
		}

		switch req.method {
		case "textDocument/documentSymbol":
			var syms []DocumentSymbol
			if err := json.Unmarshal(msg.Result, &syms); err != nil {
				fail("documentSymbol result: %v", err)
				break
			}
			names := make([]string, 0, len(syms))
			for _, s := range syms {
				names = append(names, s.Name)
				if !validRange(s.Range) {
					fail("documentSymbol %s has the backwards range %+v", s.Name, s.Range)
				}
			}
			if err := checkOutline(names, finalDefs[req.doc]); err != nil {
				fail("documentSymbol for doc %d: %v", req.doc, err)
			}
			if *msg.ID == finalID[req.doc] {
				want := []string{"NAMES"}
				for k := 0; k < finalDefs[req.doc]; k++ {
					want = append(want, fmt.Sprintf("f%d", k))
				}
				if strings.Join(names, ",") != strings.Join(want, ",") {
					fail("the last outline for doc %d is %v, want %v", req.doc, names, want)
				}
			}
		case "textDocument/semanticTokens/full":
			var tok semanticTokens
			if err := json.Unmarshal(msg.Result, &tok); err != nil {
				fail("semanticTokens result: %v", err)
			} else if err := checkTokens(tok.Data); err != nil {
				fail("semanticTokens for doc %d: %v", req.doc, err)
			}
		case "textDocument/completion":
			var list completionList
			if err := json.Unmarshal(msg.Result, &list); err != nil {
				fail("completion result: %v", err)
			} else if len(list.Items) > 0 {
				nonEmpty++
			}
		case "textDocument/hover":
			if string(msg.Result) != "null" {
				var h hoverResult
				if err := json.Unmarshal(msg.Result, &h); err != nil || h.Contents.Value == "" {
					fail("hover result %s: %v", msg.Result, err)
				}
				hovers++
			}
		case "textDocument/definition", "textDocument/references":
			var locs []Location
			if err := json.Unmarshal(msg.Result, &locs); err != nil {
				fail("%s result: %v", req.method, err)
			}
			for _, l := range locs {
				if l.URI != models[req.doc].uri || !validRange(l.Range) {
					fail("%s returned %+v for doc %d", req.method, l, req.doc)
				}
			}
		case "textDocument/documentLink":
			var links []DocumentLink
			if err := json.Unmarshal(msg.Result, &links); err != nil {
				fail("documentLink result: %v", err)
			}
			for _, l := range links {
				if l.Target != "" && !strings.HasSuffix(l.Target, "/known/main.star") {
					fail("documentLink target %q", l.Target)
				}
			}
		case "workspace/symbol":
			var syms []SymbolInformation
			if err := json.Unmarshal(msg.Result, &syms); err != nil {
				fail("workspace/symbol result: %v", err)
			}
			for _, s := range syms {
				if s.Name != "NAMES" && !fnName.MatchString(s.Name) {
					fail("workspace/symbol returned %q, which no version of the buffer had", s.Name)
				}
			}
		case "textDocument/foldingRange":
			var folds []FoldingRange
			if err := json.Unmarshal(msg.Result, &folds); err != nil {
				fail("foldingRange result: %v", err)
			}
			for _, f := range folds {
				if f.StartLine < 0 || f.EndLine < f.StartLine {
					fail("foldingRange %+v", f)
				}
			}
		}
	}

	// Everything is answered. Closing the client's end is the exit.
	toServerW.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("the server did not stop after the client closed the stream")
	}

	for _, f := range failures {
		t.Error(f)
	}
	if nonEmpty == 0 {
		t.Error("no completion request ever returned an item, so the requests were not doing real work")
	}
	if hovers == 0 {
		t.Error("no hover request ever found anything, so the requests were not doing real work")
	}

	for i, m := range models {
		d, ok := srv.docs.get(m.uri)
		if !ok {
			t.Errorf("doc %d is no longer open", i)
			continue
		}
		if string(d.text) != m.text {
			t.Errorf("doc %d: the server's text differs from the client's after %d versions\nserver: %q\nclient: %q",
				i, m.version, d.text, m.text)
		}
		if d.version != m.version {
			t.Errorf("doc %d: the server is at version %d, the client at %d", i, d.version, m.version)
		}
		// Diagnostics are published in order, one per change.
		versions := diagVersions[m.uri]
		for k := 1; k < len(versions); k++ {
			if versions[k] <= versions[k-1] {
				t.Errorf("doc %d: diagnostics went from version %d to %d", i, versions[k-1], versions[k])
				break
			}
		}
		if len(versions) == 0 || versions[len(versions)-1] != lastVersion[m.uri] {
			t.Errorf("doc %d: the last diagnostics were for version %v, want %d", i, versions, lastVersion[m.uri])
		}
	}
	t.Logf("%d documents, %d rounds, %d requests, %d edits", docs, rounds, len(pending)-1, len(diagVersions[models[0].uri])-1)
}

// TestADocumentIsNeverEditedInPlace is the plain statement of what the stream
// above relies on. A request holds the Document it got; an edit must leave that
// Document alone and make a new one.
func TestADocumentIsNeverEditedInPlace(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/inplace.star"
	held := srv.docs.open(uri, 1, "def a():\n    return 1\n")
	text, version, lines := string(held.text), held.version, len(held.lineStarts)
	start := held.positionOf(len(held.text))

	edited := srv.docs.change(uri, 2, []contentChange{
		{Range: &Range{Start: Position{Line: 0, Character: 4}, End: Position{Line: 0, Character: 5}}, Text: "renamed_function"},
		{Range: &Range{Start: Position{Line: 2}, End: Position{Line: 2}}, Text: "\n\ndef b():\n    pass\n"},
	})

	if edited == held {
		t.Fatal("an edit returned the very Document it should have left alone")
	}
	if string(held.text) != text || held.version != version || len(held.lineStarts) != lines {
		t.Errorf("the Document a request holds changed under it: text %q, version %d, %d lines; was %q, %d, %d",
			held.text, held.version, len(held.lineStarts), text, version, lines)
	}
	if got := held.positionOf(len(held.text)); got != start {
		t.Errorf("the held Document's end moved from %+v to %+v", start, got)
	}
	if current, _ := srv.docs.get(uri); current != edited {
		t.Error("the store does not hold the edited Document")
	}
	if edited.version != 2 || !strings.Contains(string(edited.text), "renamed_function") || !strings.Contains(string(edited.text), "def b()") {
		t.Errorf("the edit was not applied: version %d, text %q", edited.version, edited.text)
	}
}

// TestManyRequestsReadOneDocumentWhileTheNextIsBuilt holds a Document and has
// readers walk it while other buffers are parsed on the same parser, which is
// the case gotreesitter's arena pooling would turn into a use-after-free if a
// tree were ever released. Under -race it is the direct test that a published
// tree is never written again.
func TestManyRequestsReadOneDocumentWhileTheNextIsBuilt(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/held.star"
	var src strings.Builder
	for i := 0; i < 40; i++ {
		src.WriteString(defText(i))
	}
	held := srv.docs.open(uri, 1, src.String())
	want := held.tree.RootNode().SExpr(srv.lang)
	wantSyms := fmt.Sprint(srv.documentSymbols(held))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad sync.Once
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := fmt.Sprint(srv.documentSymbols(held)); got != wantSyms {
					bad.Do(func() {
						t.Errorf("the held outline changed while other buffers were parsed:\n%s\nwas\n%s", got, wantSyms)
					})
					return
				}
				srv.semanticTokensFor(held)
				srv.hover(held, Position{Line: 1, Character: 6})
			}
		}()
	}

	// Parse other buffers on the same parser, enough times to recycle any arena
	// that was ever put back in a pool.
	for i := 0; i < 60; i++ {
		other := fmt.Sprintf("file:///tmp/starlsp-test/other%d.star", i%3)
		srv.docs.open(other, i, defText(i)+defText(i+1))
		srv.docs.change(other, i+1, []contentChange{{Text: defText(i + 2)}})
	}
	close(stop)
	wg.Wait()

	if got := held.tree.RootNode().SExpr(srv.lang); got != want {
		t.Error("the tree of a published Document changed after later parses")
	}
}

func symbolNames(syms []DocumentSymbol) []string {
	names := make([]string, 0, len(syms))
	for _, s := range syms {
		names = append(names, s.Name)
	}
	return names
}

// TestOutlineNamesComeFromTheTextTheTreeWasParsedFrom covers the fallback to the
// last parse that had structure. An unclosed bracket at the top of the buffer
// leaves nothing to build an outline from, and shifts every byte after it. The
// outline then comes from the earlier tree, and its names must be read from the
// earlier text. Reading them from the new text with the old tree's offsets
// returns the wrong characters.
func TestOutlineNamesComeFromTheTextTheTreeWasParsedFrom(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/fallback.star"
	srv.docs.open(uri, 1, "def alpha():\n    pass\n\ndef beta():\n    pass\n")
	d := srv.docs.change(uri, 2, []contentChange{{Range: &Range{}, Text: "x = [\n"}})

	if got, want := strings.Join(symbolNames(srv.documentSymbols(d)), ","), "alpha,beta"; got != want {
		t.Errorf("outline after an edit that shifted the text is %q, want %q", got, want)
	}

	// Healing the buffer returns the live outline.
	d = srv.docs.change(uri, 3, []contentChange{{Range: &Range{End: Position{Line: 1}}, Text: ""}})
	if got, want := strings.Join(symbolNames(srv.documentSymbols(d)), ","), "alpha,beta"; got != want {
		t.Errorf("outline after healing is %q, want %q", got, want)
	}
}
