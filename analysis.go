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

// analysisCache memoizes one analysis per document version, because a single
// keystroke can trigger diagnostics, semantic tokens, and a highlight request,
// and parsing three times for one edit is waste the editor feels.
type analysisCache struct {
	mu      sync.Mutex
	uri     string
	version int
	result  analysis
}

func (c *analysisCache) get(d *Document, compute func() analysis) analysis {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.uri == d.uri && c.version == d.version && c.result.Refs != nil {
		return c.result
	}
	c.result = compute()
	c.uri, c.version = d.uri, d.version
	return c.result
}

// analyze parses and resolves a document, then indexes its references.
func (s *Server) analyze(d *Document) analysis {
	return s.analyses.get(d, func() analysis { return s.analyzeUncached(d) })
}

func (s *Server) analyzeUncached(d *Document) analysis {
	out := analysis{Refs: newReferenceIndex()}

	file, err := syntax.Parse(d.filename(), d.text, syntax.RetainComments)
	if err != nil {
		out.Diagnostics = s.syntaxDiagnostics(d, err)
		return out
	}
	out.File = file

	// Resolution needs to know which names the host provides, so that a
	// dialect builtin is not reported as undefined.
	isPredeclared := func(name string) bool {
		_, ok := s.globalIndex()[name]
		return ok
	}
	isUniversal := func(name string) bool { return universeNames[name] }

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
