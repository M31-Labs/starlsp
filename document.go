package starlsp

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"go.starlark.net/syntax"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// Document holds one open buffer, its line index, and its parse tree.
//
// The tree is produced by gotreesitter and is always available, even when the
// buffer does not parse as valid Starlark. That is the whole reason the
// structural half of this server uses tree-sitter: a language server is most
// useful exactly when the buffer is half-typed.
type Document struct {
	uri     string
	path    string // filesystem path, empty for non-file URIs
	version int
	text    []byte

	// lineStarts[i] is the byte offset where line i begins.
	lineStarts []int

	tree *gotreesitter.Tree

	// lastGood is the most recent parse that had usable structure.
	//
	// Some edits degenerate the whole parse: typing the dot in `x = foo.`
	// yields a single ERROR node holding loose identifiers, with no
	// function_definition anywhere. That is the exact moment a user is asking
	// for completion, and an outline that empties itself mid-keystroke reads
	// as the server crashing.
	//
	// Navigation features fall back to this tree. Highlighting deliberately
	// does not: a stale outline entry is a small inaccuracy, but stale
	// semantic-token ranges colour the wrong characters, which looks broken.
	lastGood *gotreesitter.Tree
}

// documentStore is the set of open buffers. Every exported method is safe for
// concurrent use; the JSON-RPC loop is single-reader but replies may race.
type documentStore struct {
	mu   sync.RWMutex
	docs map[string]*Document

	parserMu sync.Mutex
	parser   *gotreesitter.Parser
	lang     *gotreesitter.Language
}

func newDocumentStore(lang *gotreesitter.Language) *documentStore {
	return &documentStore{
		docs:   make(map[string]*Document),
		parser: gotreesitter.NewParser(lang),
		lang:   lang,
	}
}

// open records a newly opened buffer and parses it.
func (s *documentStore) open(uri string, version int, text string) *Document {
	d := &Document{
		uri:     uri,
		path:    uriToPath(uri),
		version: version,
		text:    []byte(text),
	}
	d.indexLines()
	s.reparse(d)

	s.mu.Lock()
	s.docs[uri] = d
	s.mu.Unlock()
	return d
}

// change applies edits to an open buffer and reparses it. A change with no
// range replaces the whole Document.
func (s *documentStore) change(uri string, version int, changes []contentChange) *Document {
	s.mu.Lock()
	d, ok := s.docs[uri]
	s.mu.Unlock()
	if !ok {
		return nil
	}

	for _, ch := range changes {
		if ch.Range == nil {
			d.text = []byte(ch.Text)
			d.indexLines()
			continue
		}
		start := d.offsetOf(ch.Range.Start)
		end := d.offsetOf(ch.Range.End)
		if start > end {
			start, end = end, start
		}
		next := make([]byte, 0, len(d.text)-(end-start)+len(ch.Text))
		next = append(next, d.text[:start]...)
		next = append(next, ch.Text...)
		next = append(next, d.text[end:]...)
		d.text = next
		d.indexLines()
	}
	d.version = version
	s.reparse(d)
	return d
}

func (s *documentStore) close(uri string) {
	s.mu.Lock()
	delete(s.docs, uri)
	s.mu.Unlock()
}

func (s *documentStore) get(uri string) (*Document, bool) {
	s.mu.RLock()
	d, ok := s.docs[uri]
	s.mu.RUnlock()
	return d, ok
}

// reparse rebuilds the tree for d.
//
// This is a full reparse rather than an incremental one. Starlark scripts are
// small — the largest in the Starkite repository is a few hundred lines — and
// a full parse stays well inside a keystroke budget. Wiring Tree.Edit for
// incremental reuse is a contained follow-up if profiling ever asks for it.
func (s *documentStore) reparse(d *Document) {
	s.parserMu.Lock()
	defer s.parserMu.Unlock()
	tree, err := s.parser.Parse(d.text)
	if err != nil {
		// A parser error is not a syntax error: the tree-sitter parse is
		// error tolerant and returns a tree with ERROR nodes instead. Keep
		// the previous tree so structural features degrade rather than fail.
		return
	}
	if hasUsableStructure(tree, s.lang) {
		d.lastGood = tree
	} else if d.tree != nil && d.lastGood == nil {
		d.lastGood = d.tree
	}
	d.tree = tree
}

// hasUsableStructure reports whether a tree has at least one named top-level
// node that is not an error, which is what an outline can be built from.
func hasUsableStructure(tree *gotreesitter.Tree, lang *gotreesitter.Language) bool {
	if tree == nil {
		return false
	}
	root := tree.RootNode()
	if root == nil {
		return false
	}
	for i := 0; i < root.NamedChildCount(); i++ {
		child := root.NamedChild(i)
		if child != nil && !child.IsError() {
			return true
		}
	}
	return false
}

// structureTree returns the tree navigation features should read.
//
// It prefers the current parse and falls back to the last one that had usable
// structure, so an outline does not empty itself while a member access is
// half-typed.
func (d *Document) structureTree(lang *gotreesitter.Language) *gotreesitter.Tree {
	if hasUsableStructure(d.tree, lang) {
		return d.tree
	}
	if d.lastGood != nil {
		return d.lastGood
	}
	return d.tree
}

// indexLines recomputes the line offset table.
func (d *Document) indexLines() {
	starts := d.lineStarts[:0]
	if starts == nil {
		starts = make([]int, 0, 256)
	}
	starts = append(starts, 0)
	for i, b := range d.text {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	d.lineStarts = starts
}

// lineCount returns the number of lines in the Document.
func (d *Document) lineCount() int { return len(d.lineStarts) }

// lineText returns the bytes of line n without its terminator.
func (d *Document) lineText(n int) []byte {
	if n < 0 || n >= len(d.lineStarts) {
		return nil
	}
	start := d.lineStarts[n]
	end := len(d.text)
	if n+1 < len(d.lineStarts) {
		end = d.lineStarts[n+1]
	}
	line := d.text[start:end]
	return []byte(strings.TrimRight(string(line), "\r\n"))
}

// offsetOf converts an LSP position — line plus UTF-16 code-unit column — into
// a byte offset into the Document.
func (d *Document) offsetOf(p Position) int {
	if p.Line < 0 {
		return 0
	}
	if p.Line >= len(d.lineStarts) {
		return len(d.text)
	}
	lineStart := d.lineStarts[p.Line]
	lineEnd := len(d.text)
	if p.Line+1 < len(d.lineStarts) {
		lineEnd = d.lineStarts[p.Line+1]
	}
	line := d.text[lineStart:lineEnd]

	// Walk the line converting UTF-8 runes to UTF-16 code units until the
	// requested column is reached.
	units := 0
	off := 0
	for off < len(line) {
		if units >= p.Character {
			break
		}
		r, size := utf8.DecodeRune(line[off:])
		if r == utf8.RuneError && size <= 1 {
			off++
			units++
			continue
		}
		if r == '\n' {
			break
		}
		units += utf16Len(r)
		off += size
	}
	return lineStart + off
}

// positionOf converts a byte offset into an LSP position.
func (d *Document) positionOf(offset int) Position {
	if offset < 0 {
		offset = 0
	}
	if offset > len(d.text) {
		offset = len(d.text)
	}
	// Binary search for the line containing offset.
	lo, hi := 0, len(d.lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if d.lineStarts[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	lineStart := d.lineStarts[lo]
	units := 0
	for off := lineStart; off < offset; {
		r, size := utf8.DecodeRune(d.text[off:])
		if r == utf8.RuneError && size <= 1 {
			off++
			units++
			continue
		}
		units += utf16Len(r)
		off += size
	}
	return Position{Line: lo, Character: units}
}

// rangeOf converts a byte span into an LSP range.
func (d *Document) rangeOf(startByte, endByte int) Range {
	return Range{Start: d.positionOf(startByte), End: d.positionOf(endByte)}
}

// nodeRange converts a tree-sitter node's byte span into an LSP range.
func (d *Document) nodeRange(n *gotreesitter.Node) Range {
	if n == nil {
		return Range{}
	}
	return d.rangeOf(int(n.StartByte()), int(n.EndByte()))
}

// nodeText returns the source text a node covers.
func (d *Document) nodeText(n *gotreesitter.Node) string {
	if n == nil {
		return ""
	}
	start, end := int(n.StartByte()), int(n.EndByte())
	if start < 0 || end > len(d.text) || start > end {
		return ""
	}
	return string(d.text[start:end])
}

// utf16Len is the number of UTF-16 code units a rune occupies.
func utf16Len(r rune) int {
	if r > 0xFFFF {
		return len(utf16.Encode([]rune{r}))
	}
	return 1
}

// uriToPath converts a file:// URI to a filesystem path. It returns "" for
// any other scheme, which callers treat as "not on disk".
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		// file:///C:/x -> C:/x
		p = strings.TrimPrefix(p, "/")
	}
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}
	return filepath.FromSlash(p)
}

// pathToURI converts a filesystem path to a file:// URI.
func pathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{Scheme: "file", Path: slashed}
	return u.String()
}

// all returns every open document.
func (s *documentStore) all() []*Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Document, 0, len(s.docs))
	for _, d := range s.docs {
		out = append(out, d)
	}
	return out
}

// The accessors below are what a Host needs from a document. Everything else
// about a Document is the server's business.

// URI is the document's client-supplied identifier.
func (d *Document) URI() string { return d.uri }

// Path is the document's filesystem path, empty when the buffer is not on disk.
func (d *Document) Path() string { return d.path }

// Version is the client's version counter for this buffer.
func (d *Document) Version() int { return d.version }

// Text returns the buffer's current bytes. The slice must not be modified.
func (d *Document) Text() []byte { return d.text }

// SpanRange converts a Starlark syntax node's span into an LSP range, so a
// host can report a diagnostic against a node it found in the AST.
func (d *Document) SpanRange(n syntax.Node) Range { return d.spanRange(n) }

// PositionOf converts a byte offset into an LSP position.
func (d *Document) PositionOf(offset int) Position { return d.positionOf(offset) }

// OffsetOf converts an LSP position into a byte offset.
func (d *Document) OffsetOf(p Position) int { return d.offsetOf(p) }
