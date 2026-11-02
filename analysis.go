package starlsp

import (
	"errors"
	"sync"

	"go.starlark.net/resolve"
	"go.starlark.net/syntax"
)

// Diagnostic sources, so an editor can tell where a message came from.
const (
	sourceStarlark   = "starlark"
	sourceTreeSitter = "starlark (syntax)"
)

// analysis is one authoritative pass over a document.
//
// Everything here comes from go.starlark.net: the same parser and resolver a
// Starlark host runs. That is the point. A language server that reimplements
// scope rules eventually disagrees with the runtime, and the disagreement is
// always the server's fault.
type analysis struct {
	// File is the parsed syntax tree, nil when the document does not parse.
	File *syntax.File

	// Diagnostics are the parse and resolve errors.
	Diagnostics []Diagnostic

	// Refs indexes every identifier by the binding it refers to. The resolver
	// establishes the bindings but does not keep the back-references, so this
	// index is built here. It is what makes find-all-references, rename, and
	// document highlight possible.
	Refs *referenceIndex
}

// referenceIndex groups identifiers by the variable they name.
type referenceIndex struct {
	// byBinding maps a *resolve.Binding to every identifier that resolves to
	// it, in source order. The declaration is included.
	byBinding map[*resolve.Binding][]*syntax.Ident

	// unresolved holds identifiers with no binding — predeclared names, the
	// universe, and names in a file that failed to resolve. They are grouped
	// by text, which is the best available answer for a global.
	unresolved map[string][]*syntax.Ident
}

// analysisCache memoizes the authoritative pass per document version.
//
// A single keystroke can trigger diagnostics, semantic tokens, a highlight and
// a completion, and parsing four times for one edit is waste the user feels.
//
// It is keyed per document rather than holding one entry, because a
// single-entry cache is worse than none once two files are open: every switch
// between them evicts the other, so the common case — alternating between a
// rule file and the BUILD file that uses it — would never hit.
//
// An entry remembers which Document it was computed from, by the Document's seq,
// and answers only a request for that Document. A version number is not enough:
// a client may reuse one, and a request that is slow to finish could store its
// result after a newer version had been published, and the cache would then
// serve the old text's analysis for the new text.
//
// An entry does not hold the Document. A Document owns a parse tree, which is
// megabytes, and the analysis needs none of it. Holding only the seq means the
// cache keeps an analysis alive and nothing else.
//
// A result is stored only for the version the store holds at that moment. A
// request that read a Document and then lost the race with an edit or a close is
// answered, but nobody will ask the cache for its version again.
type analysisCache struct {
	mu sync.Mutex

	// gen counts invalidations. A result computed before one is not stored
	// after it, because the world it was computed in has changed.
	gen uint64

	entries map[string]cachedAnalysis
}

type cachedAnalysis struct {
	seq    uint64
	result analysis
}

// maxCachedAnalyses bounds the cache. Entries are evicted on didClose, so this
// only matters if a client opens documents and never closes them; the cap
// keeps that bounded rather than unbounded.
const maxCachedAnalyses = 64

// get returns the analysis of d, computing it if the cache has none. current
// reports whether d is still the version the store holds; it is called with the
// cache locked, so it must not call back into the cache.
func (c *analysisCache) get(d *Document, current func() bool, compute func() analysis) analysis {
	c.mu.Lock()
	if entry, ok := c.entries[d.uri]; ok && entry.seq == d.seq && entry.result.Refs != nil {
		c.mu.Unlock()
		return entry.result
	}
	gen := c.gen
	c.mu.Unlock()

	// Computed outside the lock: parsing a large file must not block another
	// document's lookup.
	result := compute()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen || !current() {
		return result // right for this caller's Document, but not worth keeping
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedAnalysis)
	}
	if len(c.entries) >= maxCachedAnalyses {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[d.uri] = cachedAnalysis{seq: d.seq, result: result}
	return result
}

// invalidate drops one document's cached analysis.
func (c *analysisCache) invalidate(uri string) {
	c.mu.Lock()
	c.gen++
	delete(c.entries, uri)
	c.mu.Unlock()
}

// invalidateAll drops everything, for when the world changed underneath —
// a configuration change, or a watched file the host resolves through.
func (c *analysisCache) invalidateAll() {
	c.mu.Lock()
	c.gen++
	c.entries = make(map[string]cachedAnalysis)
	c.mu.Unlock()
}

// analyze parses and resolves a document, then indexes its references.
func (s *Server) analyze(d *Document) analysis {
	return s.analyses.get(d,
		func() bool { return s.docs.isCurrent(d) },
		func() analysis { return s.analyzeUncached(d) })
}

func (s *Server) analyzeUncached(d *Document) analysis {
	out := analysis{Refs: newReferenceIndex()}

	opts := s.fileOptions()
	file, err := opts.Parse(d.filename(), d.text, syntax.RetainComments)
	if err != nil {
		out.Diagnostics = s.syntaxDiagnostics(d, err)
		return out
	}
	out.File = file

	// Resolution needs to know which names the host provides, so that a
	// dialect builtin is not reported as undefined.
	//
	// The two predicates are not interchangeable. The resolver asks isPredeclared
	// first, and only a name that is not predeclared reaches isUniversal, which
	// is where it enforces the dialect's own switches: a universe name `set` is
	// refused unless FileOptions.Set is on. Reporting a universe name as
	// predeclared skips that check, and a dialect that does not have the name
	// then gets no diagnostic for it. So the universe is answered as the
	// universe, and only the names beyond it count as predeclared.
	isPredeclared := func(name string) bool {
		if universeNames[name] {
			return false
		}
		_, ok := s.globalIndex()[name]
		return ok
	}
	isUniversal := func(name string) bool { return universeNames[name] && !s.removed[name] }

	// The resolver reads its dialect from the file's own options, which the
	// parse above attached.
	if err := resolve.File(file, isPredeclared, isUniversal); err != nil {
		out.Diagnostics = append(out.Diagnostics, s.resolveDiagnostics(d, err)...)
	}

	out.Refs.build(file)
	return out
}

func newReferenceIndex() *referenceIndex {
	return &referenceIndex{
		byBinding:  make(map[*resolve.Binding][]*syntax.Ident),
		unresolved: make(map[string][]*syntax.Ident),
	}
}

// build walks the file once and groups every identifier by its binding.
func (idx *referenceIndex) build(file *syntax.File) {
	syntax.Walk(file, func(n syntax.Node) bool {
		ident, ok := n.(*syntax.Ident)
		if !ok {
			return true
		}
		if binding, ok := ident.Binding.(*resolve.Binding); ok && binding != nil {
			idx.byBinding[binding] = append(idx.byBinding[binding], ident)
			return true
		}
		idx.unresolved[ident.Name] = append(idx.unresolved[ident.Name], ident)
		return true
	})
}

// identAt returns the identifier covering a byte offset.
func (a analysis) identAt(d *Document, offset int) *syntax.Ident {
	if a.File == nil {
		return nil
	}
	var found *syntax.Ident
	syntax.Walk(a.File, func(n syntax.Node) bool {
		ident, ok := n.(*syntax.Ident)
		if !ok {
			return true
		}
		start, end := ident.Span()
		if offset >= d.offsetOfStarlarkPos(start) && offset <= d.offsetOfStarlarkPos(end) {
			found = ident
		}
		return true
	})
	return found
}

// referencesTo returns every identifier naming the same variable as ident.
//
// A resolved identifier is matched by binding, which is exact: a local x in
// one function is not the same variable as a local x in another. An
// unresolved identifier — a predeclared or universal name — is matched by
// text, which is the only answer available for a name the file does not bind.
func (a analysis) referencesTo(ident *syntax.Ident) []*syntax.Ident {
	if ident == nil || a.Refs == nil {
		return nil
	}
	if binding, ok := ident.Binding.(*resolve.Binding); ok && binding != nil {
		return a.Refs.byBinding[binding]
	}
	return a.Refs.unresolved[ident.Name]
}

// declarationOf returns the identifier that first bound this variable, and
// reports whether the variable is bound in this file at all.
func (a analysis) declarationOf(ident *syntax.Ident) (*syntax.Ident, bool) {
	if ident == nil {
		return nil, false
	}
	binding, ok := ident.Binding.(*resolve.Binding)
	if !ok || binding == nil || binding.First == nil {
		return nil, false
	}
	return binding.First, true
}

// isLocalToFile reports whether a variable is bound in this file, which is
// what makes it safe to rename. A predeclared or universal name is not.
func (a analysis) isLocalToFile(ident *syntax.Ident) bool {
	binding, ok := ident.Binding.(*resolve.Binding)
	if !ok || binding == nil {
		return false
	}
	switch binding.Scope {
	case resolve.Local, resolve.Cell, resolve.Free, resolve.Global:
		return true
	default:
		return false
	}
}

// ---------- diagnostics ----------

func (s *Server) syntaxDiagnostics(d *Document, err error) []Diagnostic {
	var syntaxErr syntax.Error
	if errors.As(err, &syntaxErr) {
		return []Diagnostic{s.diagnosticAt(d, syntaxErr.Pos, syntaxErr.Msg)}
	}
	return []Diagnostic{{
		Range:    Range{},
		Severity: severityError,
		Source:   sourceStarlark,
		Message:  err.Error(),
	}}
}

func (s *Server) resolveDiagnostics(d *Document, err error) []Diagnostic {
	var list resolve.ErrorList
	if !errors.As(err, &list) {
		return []Diagnostic{{
			Range:    Range{},
			Severity: severityError,
			Source:   sourceStarlark,
			Message:  err.Error(),
		}}
	}
	out := make([]Diagnostic, 0, len(list))
	for _, e := range list {
		out = append(out, s.diagnosticAt(d, e.Pos, e.Msg))
	}
	return out
}

// diagnosticAt converts a Starlark position — a one-based line and one-based
// rune column — into a range covering the token that starts there.
func (s *Server) diagnosticAt(d *Document, pos syntax.Position, msg string) Diagnostic {
	offset := d.offsetOfStarlarkPos(pos)
	end := offset
	for end < len(d.text) && isIdentByte(d.text[end]) {
		end++
	}
	if end == offset && end < len(d.text) {
		end++
	}
	return Diagnostic{
		Range:    d.rangeOf(offset, end),
		Severity: severityError,
		Source:   sourceStarlark,
		Message:  msg,
	}
}

// filename returns a name for error messages.
func (d *Document) filename() string {
	if d.path != "" {
		return d.path
	}
	return d.uri
}

// offsetOfStarlarkPos converts a one-based line and rune column to a byte
// offset.
func (d *Document) offsetOfStarlarkPos(pos syntax.Position) int {
	line := int(pos.Line) - 1
	if line < 0 {
		line = 0
	}
	if line >= len(d.lineStarts) {
		return len(d.text)
	}
	offset := d.lineStarts[line]
	col := int(pos.Col) - 1
	for i := 0; i < col && offset < len(d.text); i++ {
		if d.text[offset] == '\n' {
			break
		}
		_, size := decodeRune(d.text[offset:])
		offset += size
	}
	return offset
}

// spanRange converts a syntax node's span into an LSP range.
func (d *Document) spanRange(n syntax.Node) Range {
	start, end := n.Span()
	return d.rangeOf(d.offsetOfStarlarkPos(start), d.offsetOfStarlarkPos(end))
}

func decodeRune(b []byte) (rune, int) {
	if len(b) == 0 {
		return 0, 0
	}
	if b[0] < 0x80 {
		return rune(b[0]), 1
	}
	for size := 2; size <= 4 && size <= len(b); size++ {
		if size == len(b) || b[size]&0xC0 != 0x80 {
			return rune(b[0]), size
		}
	}
	return rune(b[0]), 1
}

// universeNames is the specification's universal name set, taken from the
// runtime rather than written down.
var universeNames = func() map[string]bool {
	out := make(map[string]bool)
	for _, sym := range NewVanilla().Globals() {
		if sym.Kind != KindKeyword {
			out[sym.Name] = true
		}
	}
	return out
}()

// switchedOff reports whether a dialect's file options turn off a name that the
// universe defines. The resolver refuses such a name when it is used, so the
// server does not offer it either: a completion that the diagnostics then call an
// error would be the server contradicting itself. Only set is gated this way.
func switchedOff(opts *syntax.FileOptions, name string) bool {
	return name == "set" && !opts.Set
}

// fileOptions returns the dialect the configured host asks for, or the
// specification defaults when it asks for nothing.
func (s *Server) fileOptions() *syntax.FileOptions {
	if s.caps.dialect != nil {
		if opts := s.caps.dialect.FileOptions(); opts != nil {
			return opts
		}
	}
	return SpecFileOptions()
}
