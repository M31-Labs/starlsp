package starlsp

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"

	"go.starlark.net/syntax"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// Document is one version of an open buffer: its text, its line index, and its
// parse tree. It never changes after it is built.
//
// An edit does not modify the Document a request is already reading. It builds
// the next Document from the previous one, and the store then swaps that in for
// the URI. A request takes one Document when it starts and reads only that, so it
// sees a single version of the buffer from start to finish, however many
// didChange notifications arrive while it runs.
//
// Why this is safe to share between goroutines:
//
//   - Every field is written once, before the store publishes the Document under
//     its mutex. Readers get the Document through the same mutex, so they see
//     all of those writes. After that, nobody writes to it.
//   - text and lineStarts are never edited in place. An edit allocates new ones.
//   - The tree belongs to this Document and to any later Document that keeps it
//     as lastGood. gotreesitter documents a Tree as safe for concurrent reads
//     once it is built. starlsp never calls Release, Edit, or ParseIncremental
//     on a tree it has published, so the next parse allocates its own node
//     arena and cannot write into this one. The garbage collector frees a tree
//     when no Document and no running request refers to it. The one Release
//     call is on the scratch Document of a scan of files on disk, which is never
//     published; see release.
//
// The tree is the gotreesitter parse. It is available even when the buffer does
// not parse as valid Starlark, because tree-sitter recovers from syntax errors.
// That is the whole reason the structural half of this server uses tree-sitter:
// a language server is most useful exactly when the buffer is half-typed. The
// one case with no tree is a parser that failed outright, by returning an error
// or by panicking. The Document still carries its own text then, and every
// feature that needs a tree answers from the last version that had one, or with
// nothing.
type Document struct {
	// seq is unique among the Documents of one store and rises with each build.
	// The analysis cache compares it instead of holding the Document, so a cache
	// entry cannot keep a tree alive.
	seq uint64

	uri     string
	path    string // filesystem path, empty for non-file URIs
	version int
	text    []byte

	// lineStarts[i] is the byte offset where line i begins.
	lineStarts []int

	// tree is the parse of text, and root is its root node. root is read once,
	// while the Document is built, so no request calls Tree.RootNode on a tree
	// that other goroutines are reading. Both are nil when the parser failed on
	// this text. They never belong to another version: a tree's byte offsets mean
	// something only against the text it was parsed from.
	tree *gotreesitter.Tree
	root *gotreesitter.Node

	// usable reports whether this version's own parse has structure an outline
	// can be built from.
	usable bool

	// lastGood is the most recent earlier version whose parse had usable
	// structure. It is nil when this version is usable itself, and nil when no
	// earlier version was.
	//
	// Some edits degenerate the whole parse: typing the dot in `x = foo.`
	// yields a single ERROR node holding loose identifiers, with no
	// function_definition anywhere. That is the exact moment a user is asking
	// for completion, and an outline that empties itself mid-keystroke reads
	// as the server crashing.
	//
	// Navigation features fall back to it. Highlighting deliberately does not: a
	// stale outline entry is a small inaccuracy, but stale semantic-token ranges
	// colour the wrong characters, which looks broken.
	//
	// It is a whole Document, not just a tree, because a tree's byte offsets mean
	// something only against the text it was parsed from. Reading an old tree's
	// names out of the new text returns the wrong characters as soon as an edit
	// has shifted them. lastGood never has a lastGood of its own, so the chain
	// cannot grow.
	lastGood *Document
}

// documentStore is the set of open buffers. Every method is safe for concurrent
// use.
//
// docs maps each URI to its current Document. Writers (open, change, close) take
// editMu, so two of them never interleave, then build the next Document without
// holding mu, and take mu only for the pointer swap. Readers take mu for reading
// and do nothing else, so a slow parse never delays a request.
type documentStore struct {
	editMu sync.Mutex

	mu   sync.RWMutex
	docs map[string]*Document

	// parser is not safe for concurrent use. parserMu guards it, and every parse
	// goes through runParser.
	parserMu sync.Mutex
	parser   *gotreesitter.Parser
	lang     *gotreesitter.Language

	// seq numbers the Documents this store builds.
	seq atomic.Uint64

	// logf reports a parse that failed. It may be nil.
	logf func(format string, args ...any)
}

func newDocumentStore(lang *gotreesitter.Language, logf func(format string, args ...any)) *documentStore {
	return &documentStore{
		docs:   make(map[string]*Document),
		parser: gotreesitter.NewParser(lang),
		lang:   lang,
		logf:   logf,
	}
}

// open records a newly opened buffer and parses it.
func (s *documentStore) open(uri string, version int, text string) *Document {
	s.editMu.Lock()
	defer s.editMu.Unlock()

	b := []byte(text)
	d := s.build(nil, uri, version, b, lineStartsOf(b))

	s.mu.Lock()
	s.docs[uri] = d
	s.mu.Unlock()
	return d
}

// change applies edits to an open buffer. It builds a new Document and returns
// it; the Document it replaces is left exactly as it was. A change with no range
// replaces the whole text. It returns nil when the buffer is not open.
func (s *documentStore) change(uri string, version int, changes []contentChange) *Document {
	s.editMu.Lock()
	defer s.editMu.Unlock()

	prev, ok := s.get(uri)
	if !ok {
		return nil
	}
	text, starts := applyChanges(prev.text, prev.lineStarts, changes)
	d := s.build(prev, uri, version, text, starts)

	s.mu.Lock()
	s.docs[uri] = d
	s.mu.Unlock()
	return d
}

func (s *documentStore) close(uri string) {
	s.editMu.Lock()
	defer s.editMu.Unlock()

	s.mu.Lock()
	delete(s.docs, uri)
	s.mu.Unlock()
}

// get returns the current Document for uri. The Document is immutable, so the
// caller can use it for as long as it likes.
func (s *documentStore) get(uri string) (*Document, bool) {
	s.mu.RLock()
	d, ok := s.docs[uri]
	s.mu.RUnlock()
	return d, ok
}

// isCurrent reports whether d is the Document the store holds for its URI. It is
// false once the buffer has been edited or closed.
func (s *documentStore) isCurrent(d *Document) bool {
	cur, ok := s.get(d.uri)
	return ok && cur == d
}

// all returns the current Document of every open buffer.
func (s *documentStore) all() []*Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Document, 0, len(s.docs))
	for _, d := range s.docs {
		out = append(out, d)
	}
	return out
}

// snapshot builds a Document without opening it. A one-off analysis, such as
// Server.Diagnose or a scan of files that are not open, uses it so that it
// cannot replace or close a buffer an editor has open under the same URI.
func (s *documentStore) snapshot(uri string, version int, text []byte) *Document {
	return s.build(nil, uri, version, text, lineStartsOf(text))
}

// build parses text and returns the finished Document. prev is the Document
// this one follows, or nil. build does not touch the store; the caller
// publishes the result.
//
// Parsing is from scratch every time. Starlark scripts are small, and a full
// parse stays well inside a keystroke budget. Incremental parsing would need
// care: gotreesitter's Tree.Edit and ParseIncremental write into the old tree,
// and a request may still be reading the old tree, so an incremental reparse
// would have to copy the old tree first.
//
// A parse that fails does not fail the build. If the parser returns an error or
// panics, the Document is still built, with this version's own text and line
// index and no tree. It reads its structure from an earlier version, as any
// Document without usable structure does. The alternative is to let the failure
// escape, and then the edit is lost: the text is published together with the
// Document, so a build that does not finish leaves the store on the old text, and
// every later edit in the same session lands on it.
func (s *documentStore) build(prev *Document, uri string, version int, text []byte, lineStarts []int) *Document {
	d := &Document{
		seq:        s.seq.Add(1),
		uri:        uri,
		path:       uriToPath(uri),
		version:    version,
		text:       text,
		lineStarts: lineStarts,
	}

	parsed, err := s.parse(text)
	if err != nil {
		if s.logf != nil {
			s.logf("no tree for %s (version %d), so structural features use an earlier version: %v", uri, version, err)
		}
		d.lastGood = fallbackFor(prev)
		return d
	}

	d.tree, d.root, d.usable = parsed.tree, parsed.root, parsed.usable
	if !d.usable {
		d.lastGood = fallbackFor(prev)
	}
	return d
}

// release hands the Document's tree back to gotreesitter, which reuses its memory
// for the next parse. Without it every parse allocates a fresh arena of about
// 3 MB, and a scan of 2,000 files allocates 6 GB.
//
// It is only for a Document that no other goroutine can see and that the caller
// has finished with, such as the one a scan of files on disk builds for each
// file. Once the arena is back in the pool the next parse may overwrite it, so
// releasing a tree that a request is still reading would be a use after free.
// A Document that was ever published never calls it. The fields are cleared, so
// a mistake shows up as a missing tree and not as someone else's nodes.
func (d *Document) release() {
	if d.tree != nil {
		d.tree.Release()
	}
	d.tree, d.root, d.lastGood = nil, nil, nil
	d.usable = false
}

// fallbackFor chooses the earlier version a new Document reads when its own parse
// has no usable structure. prev is the version it follows.
//
// That is the last version that had structure. When no version has had any, it
// is prev, if prev has a tree, so that there is something to read. In that case
// every later version keeps the same one, the first without structure, not the
// newest. That is what keeps the chain one deep: the Document returned here never
// has a lastGood of its own.
func fallbackFor(prev *Document) *Document {
	if prev == nil {
		return nil
	}
	if good := prev.goodSource(); good != nil {
		return good
	}
	if prev.tree != nil {
		return prev
	}
	return nil
}

// parsedTree is a tree that is ready to be shared.
type parsedTree struct {
	tree   *gotreesitter.Tree
	root   *gotreesitter.Node
	usable bool
}

// parse runs the shared parser over text and finishes the tree. A failure of any
// kind, including a panic inside gotreesitter, comes back as an error. One buffer
// that trips a bug in the library must not take the session's edits with it.
func (s *documentStore) parse(text []byte) (parsedTree, error) {
	tree, err := s.runParser(text)
	if err != nil {
		return parsedTree{}, err
	}
	return finishTree(tree)
}

// runParser is the only caller of Parser.Parse. It holds parserMu throughout,
// even if the parse panics.
//
// After a panic the parser may have stopped half way through a parse, with
// internal state nobody can vouch for. It is thrown away and replaced, so one
// bad parse cannot spoil the ones that follow.
func (s *documentStore) runParser(text []byte) (tree *gotreesitter.Tree, err error) {
	s.parserMu.Lock()
	defer s.parserMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			s.parser = gotreesitter.NewParser(s.lang)
			tree, err = nil, fmt.Errorf("the tree-sitter parser panicked: %v\n%s", r, debug.Stack())
		}
	}()

	tree, err = s.parser.Parse(text)
	if err == nil && tree == nil {
		err = errors.New("the tree-sitter parser returned no tree and no error")
	}
	return tree, err
}

// finishTree does what must happen on the goroutine that built a tree, before
// any other goroutine can see it.
//
// RootNode completes gotreesitter's lazy post-processing of a new tree, and
// settle then visits every node. Either can panic in a library bug, so a panic
// here is an error too.
func finishTree(tree *gotreesitter.Tree) (p parsedTree, err error) {
	defer func() {
		if r := recover(); r != nil {
			p, err = parsedTree{}, fmt.Errorf("finishing the tree-sitter tree panicked: %v\n%s", r, debug.Stack())
		}
	}()

	root := tree.RootNode()
	settle(root)
	return parsedTree{tree: tree, root: root, usable: hasUsableStructure(root)}, nil
}

// settle visits every node once, before the Document that owns the tree is
// shared.
//
// This is a precaution, not a fix for a race we saw. gotreesitter documents a
// Tree as safe for concurrent reads, and the race tests pass without this step.
// But some accessors write the first time they are used: Parent wires parent
// links behind a lock, and Child can materialize child nodes. One pass here makes
// those first writes happen on the goroutine that built the tree, before any
// other goroutine can see it. It costs 1 to 2 percent of a parse: 0.2 ms for a
// 25 KB script, 5 ms for a module of 20,000 statements.
//
// It does not make every tree safe. A tree built with final-child references
// writes on every Child call, not just the first. The library builds those only
// for its benchmark entry point ParseNoResultCompatibilityBenchmarkOnly, which
// this server never calls. A library version that built them from Parse would
// need a different answer, such as copying the tree.
func settle(root *gotreesitter.Node) {
	if root == nil {
		return
	}
	_ = root.Parent()
	walk(root, func(*gotreesitter.Node) bool { return true })
}

// hasUsableStructure reports whether a tree has at least one named top-level
// node that is not an error, which is what an outline can be built from.
func hasUsableStructure(root *gotreesitter.Node) bool {
	if root == nil {
		return false
	}
	for i, n := 0, root.ChildCount(); i < n; i++ {
		child := root.Child(i)
		if child != nil && child.IsNamed() && !child.IsError() {
			return true
		}
	}
	return false
}

// goodSource returns the version whose structure an outline can use: d itself
// when its own parse is usable, otherwise the last earlier version that was.
func (d *Document) goodSource() *Document {
	if d.usable {
		return d
	}
	return d.lastGood
}

// structure returns the Document navigation features should read. It is d when
// d's own parse has usable structure, and the last version that had it
// otherwise, so an outline does not empty itself while a member access is
// half-typed. Names, ranges, and the tree all come from the one Document it
// returns, so they agree with each other.
func (d *Document) structure() *Document {
	if d.usable || d.lastGood == nil {
		return d
	}
	return d.lastGood
}

// applyChanges applies LSP content changes in order and returns the new text and
// its line index. The inputs are not modified. Each change's range refers to the
// text as the changes before it left it, so the line index is rebuilt after
// each one.
func applyChanges(text []byte, lineStarts []int, changes []contentChange) ([]byte, []int) {
	for _, ch := range changes {
		if ch.Range == nil {
			text = []byte(ch.Text)
			lineStarts = lineStartsOf(text)
			continue
		}
		start := offsetIn(text, lineStarts, ch.Range.Start)
		end := offsetIn(text, lineStarts, ch.Range.End)
		if start > end {
			start, end = end, start
		}
		next := make([]byte, 0, len(text)-(end-start)+len(ch.Text))
		next = append(next, text[:start]...)
		next = append(next, ch.Text...)
		next = append(next, text[end:]...)
		text = next
		lineStarts = lineStartsOf(text)
	}
	return text, lineStarts
}

// lineStartsOf computes the line offset table for text.
func lineStartsOf(text []byte) []int {
	starts := make([]int, 0, bytes.Count(text, []byte{'\n'})+1)
	starts = append(starts, 0)
	for i, b := range text {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
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
func (d *Document) offsetOf(p Position) int { return offsetIn(d.text, d.lineStarts, p) }

// offsetIn is offsetOf for a text and line index that are not yet a Document.
func offsetIn(text []byte, lineStarts []int, p Position) int {
	if p.Line < 0 {
		return 0
	}
	if p.Line >= len(lineStarts) {
		return len(text)
	}
	lineStart := lineStarts[p.Line]
	lineEnd := len(text)
	if p.Line+1 < len(lineStarts) {
		lineEnd = lineStarts[p.Line+1]
	}
	line := text[lineStart:lineEnd]

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

// columnConverter turns the byte columns that tree-sitter reports into the UTF-16
// columns that LSP uses. A node's Point counts bytes from the start of its line,
// so every character after a non-ASCII one on the same line would be off.
//
// It builds the table for a line when the first position on that line asks for
// it, and keeps it until a position on another line asks. Asking in document
// order, which is how a walk of the tree goes, costs one pass over each line.
// Converting each position separately would cost a pass over the line prefix
// every time, which is quadratic on a generated file with thousands of tokens on
// one line.
type columnConverter struct {
	d     *Document
	row   int     // the line whose table is built, or -1
	ascii bool    // the line has no byte above 0x7f, so columns need no conversion
	units []int32 // units[i] is the UTF-16 column of byte i of the line
}

func newColumnConverter(d *Document) *columnConverter { return &columnConverter{d: d, row: -1} }

// column returns the UTF-16 column of byte column byteCol on line row.
func (c *columnConverter) column(row, byteCol int) int {
	if row != c.row {
		c.build(row)
	}
	if c.ascii {
		return byteCol
	}
	if byteCol >= len(c.units) {
		byteCol = len(c.units) - 1
	}
	if byteCol < 0 {
		return 0
	}
	return int(c.units[byteCol])
}

func (c *columnConverter) build(row int) {
	c.row = row
	d := c.d
	if row < 0 || row >= len(d.lineStarts) {
		c.ascii, c.units = true, nil
		return
	}
	end := len(d.text)
	if row+1 < len(d.lineStarts) {
		end = d.lineStarts[row+1]
	}
	line := d.text[d.lineStarts[row]:end]

	c.ascii = true
	for _, b := range line {
		if b >= utf8.RuneSelf {
			c.ascii = false
			break
		}
	}
	if c.ascii {
		c.units = nil
		return
	}

	c.units = c.units[:0]
	units := int32(0)
	for off := 0; off < len(line); {
		r, size := utf8.DecodeRune(line[off:])
		if r == utf8.RuneError && size <= 1 {
			size = 1 // a stray byte counts as one unit, as positionOf counts it
		}
		for i := 0; i < size; i++ {
			c.units = append(c.units, units) // a byte inside a rune has the column of its rune
		}
		units += int32(utf16Len(r))
		off += size
	}
	c.units = append(c.units, units) // the column just past the line's last byte
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

// The accessors below are what a Host needs from a document. Everything else
// about a Document is the server's business.

// URI is the document's client-supplied identifier.
func (d *Document) URI() string { return d.uri }

// Path is the document's filesystem path, empty when the buffer is not on disk.
func (d *Document) Path() string { return d.path }

// Version is the client's version counter for this buffer.
func (d *Document) Version() int { return d.version }

// Text returns the buffer's bytes. A Document does not change, so neither does
// the slice. It must not be modified.
func (d *Document) Text() []byte { return d.text }

// SpanRange converts a Starlark syntax node's span into an LSP range, so a
// host can report a diagnostic against a node it found in the AST.
func (d *Document) SpanRange(n syntax.Node) Range { return d.spanRange(n) }

// PositionOf converts a byte offset into an LSP position.
func (d *Document) PositionOf(offset int) Position { return d.positionOf(offset) }

// OffsetOf converts an LSP position into a byte offset.
func (d *Document) OffsetOf(p Position) int { return d.offsetOf(p) }
