package starlsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"go.starlark.net/syntax"
)

// ---------- completion ----------

func (s *Server) complete(d *Document, pos Position) completionList {
	offset := d.offsetOf(pos)
	ctx := s.contextAt(d, offset)

	if ctx.InComment {
		return completionList{Items: []CompletionItem{}}
	}
	if ctx.InString {
		return completionList{Items: []CompletionItem{}}
	}

	var items []CompletionItem

	if ctx.AfterDot {
		owner, ok := s.resolveOwner(d, ctx.Receiver)
		if !ok || s.caps.members == nil {
			// Neither the core nor the host can say what this receiver is.
			// Offering the global namespace here would be actively wrong.
			return completionList{Items: []CompletionItem{}}
		}
		for _, sym := range s.caps.members.Members(owner) {
			items = append(items, completionItemFor(sym))
		}
		return completionList{Items: items}
	}

	for _, sym := range s.host.Globals() {
		items = append(items, completionItemFor(sym))
	}
	items = append(items, s.documentLocals(d)...)
	return completionList{Items: items}
}

// documentLocals offers the names this file binds, which no host can know.
func (s *Server) documentLocals(d *Document) []CompletionItem {
	seen := make(map[string]bool)
	var items []CompletionItem

	for _, sym := range s.documentSymbols(d) {
		if seen[sym.Name] {
			continue
		}
		seen[sym.Name] = true

		kind, detail := kindVariable, "defined in this file"
		if sym.Kind == symbolFunction {
			kind, detail = kindFunction, "def "+sym.Name+sym.Detail
		}
		items = append(items, CompletionItem{
			Label:    sym.Name,
			Kind:     kind,
			Detail:   detail,
			SortText: "0" + sym.Name, // a file's own names sort first
		})
	}
	return items
}

func completionItemFor(sym Symbol) CompletionItem {
	item := CompletionItem{
		Label:    sym.Name,
		Kind:     sym.Kind.completionKind(),
		Detail:   sym.Detail,
		SortText: sym.SortText,
	}
	if item.Detail == "" {
		switch {
		case sym.Signature != "" && sym.Returns != "":
			item.Detail = sym.Signature + " → " + sym.Returns
		case sym.Signature != "":
			item.Detail = sym.Signature
		case sym.Returns != "":
			item.Detail = sym.Returns
		}
	}
	if sym.Doc != "" {
		item.Documentation = &MarkupContent{Kind: "markdown", Value: sym.Doc}
	}
	return item
}

// ---------- hover ----------

func (s *Server) hover(d *Document, pos Position) *hoverResult {
	offset := d.offsetOf(pos)
	if d.tree == nil {
		return nil
	}
	node := nodeAt(d.tree.RootNode(), offset)
	if node == nil || node.Type(s.lang) != ntIdentifier {
		return nil
	}
	name := d.nodeText(node)
	rng := d.nodeRange(node)

	owner, ok := s.ownerOfMember(d, node)
	if !ok {
		return nil
	}
	if sym, found := s.lookupSymbol(owner, name); found {
		return &hoverResult{
			Contents: MarkupContent{Kind: "markdown", Value: renderHover(sym)},
			Range:    &rng,
		}
	}

	// Not a host name. If the file binds it, say where.
	a := s.analyze(d)
	if ident := a.identAt(d, offset); ident != nil {
		if decl, ok := a.declarationOf(ident); ok {
			line := d.spanRange(decl).Start.Line + 1
			return &hoverResult{
				Contents: MarkupContent{
					Kind:  "markdown",
					Value: "```python\n" + name + "\n```\n\nBound in this file, line " + itoa(line) + ".",
				},
				Range: &rng,
			}
		}
	}
	return nil
}

// ownerOfMember reports which owner an identifier hangs off, and false when
// the identifier is a member of something the server cannot resolve.
func (s *Server) ownerOfMember(d *Document, node *gotreesitter.Node) (string, bool) {
	parent := node.Parent()
	if parent == nil || parent.Type(s.lang) != ntAttribute {
		return "", true // a bare name: the global namespace
	}
	object := childByField(parent, "object", 0, s.lang)
	if object == nil || object.StartByte() == node.StartByte() {
		return "", true // the caret is on the object, not the member
	}
	return s.resolveOwner(d, d.nodeText(object))
}

func renderHover(sym Symbol) string {
	var b strings.Builder
	signature := sym.Signature
	if signature == "" {
		signature = sym.Qualified()
	}
	b.WriteString("```python\n")
	b.WriteString(signature)
	if sym.Returns != "" {
		b.WriteString(" → " + sym.Returns)
	}
	b.WriteString("\n```")
	if sym.Doc != "" {
		b.WriteString("\n\n" + sym.Doc)
	}
	if sym.Deprecated {
		b.WriteString("\n\n**Deprecated.**")
	}
	return b.String()
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// ---------- signature help ----------

func (s *Server) signatureHelp(d *Document, pos Position) *signatureHelp {
	offset := d.offsetOf(pos)
	ctx := s.contextAt(d, offset)
	if ctx.Call == nil {
		return nil
	}
	callee := childByField(ctx.Call, "function", 0, s.lang)
	if callee == nil {
		return nil
	}

	owner, name := "", d.nodeText(callee)
	if callee.Type(s.lang) == ntAttribute {
		object := childByField(callee, "object", 0, s.lang)
		member := childByField(callee, "attribute", 1, s.lang)
		if object == nil || member == nil {
			return nil
		}
		resolved, ok := s.resolveOwner(d, d.nodeText(object))
		if !ok {
			return nil
		}
		owner, name = resolved, d.nodeText(member)
	}

	sym, ok := s.lookupSymbol(owner, name)
	if !ok || sym.Signature == "" {
		return nil
	}
	info := SignatureInformation{Label: sym.Signature}
	if sym.Doc != "" {
		info.Documentation = &MarkupContent{Kind: "markdown", Value: sym.Doc}
	}
	for _, p := range sym.Params() {
		info.Parameters = append(info.Parameters, ParameterInformation{Label: p})
	}

	active := ctx.ArgIndex
	if active < 0 {
		active = 0
	}
	if len(info.Parameters) > 0 && active >= len(info.Parameters) {
		active = len(info.Parameters) - 1
	}
	return &signatureHelp{Signatures: []SignatureInformation{info}, ActiveParameter: active}
}

// ---------- definition, references, highlight, rename ----------

func (s *Server) definition(d *Document, pos Position) []Location {
	offset := d.offsetOf(pos)

	// A load() target resolves to another file, when the host can say where.
	if loc, ok := s.loadTargetAt(d, offset); ok {
		return []Location{loc}
	}

	a := s.analyze(d)
	ident := a.identAt(d, offset)
	if ident == nil {
		return nil
	}
	decl, ok := a.declarationOf(ident)
	if !ok {
		return nil
	}
	return []Location{{URI: d.uri, Range: d.spanRange(decl)}}
}

// loadTargetAt reports the file a load() string under the caret refers to.
func (s *Server) loadTargetAt(d *Document, offset int) (Location, bool) {
	if d.tree == nil || s.caps.loads == nil {
		return Location{}, false
	}
	for _, call := range s.loadCalls(d) {
		target := s.loadTargetNode(d, call)
		if target == nil {
			continue
		}
		if offset < int(target.StartByte()) || offset > int(target.EndByte()) {
			continue
		}
		literal := stringLiteralText(d, target, s.lang)
		if literal == "" {
			continue
		}
		path, ok := s.caps.loads.ResolveLoad(LoadRequest{Target: literal, From: d.path})
		if !ok {
			continue
		}
		return Location{URI: pathToURI(path), Range: Range{}}, true
	}
	return Location{}, false
}

func (s *Server) handleReferences(req request) error {
	var params referenceParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.conn.replyError(req.ID, errInvalidParams, err.Error())
	}
	d, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return s.conn.reply(req.ID, []Location{})
	}

	a := s.analyze(d)
	ident := a.identAt(d, d.offsetOf(params.Position))
	if ident == nil {
		return s.conn.reply(req.ID, []Location{})
	}
	decl, hasDecl := a.declarationOf(ident)

	locations := []Location{}
	for _, ref := range a.referencesTo(ident) {
		if !params.Context.IncludeDeclaration && hasDecl && ref == decl {
			continue
		}
		locations = append(locations, Location{URI: d.uri, Range: d.spanRange(ref)})
	}
	sortLocations(locations)
	return s.conn.reply(req.ID, locations)
}

func (s *Server) documentHighlight(d *Document, pos Position) []DocumentHighlight {
	a := s.analyze(d)
	ident := a.identAt(d, d.offsetOf(pos))
	if ident == nil {
		return []DocumentHighlight{}
	}
	decl, hasDecl := a.declarationOf(ident)

	out := []DocumentHighlight{}
	for _, ref := range a.referencesTo(ident) {
		kind := highlightRead
		if hasDecl && ref == decl {
			kind = highlightWrite
		}
		out = append(out, DocumentHighlight{Range: d.spanRange(ref), Kind: kind})
	}
	return out
}

// prepareRename reports the span a rename would replace, and refuses names the
// file does not bind.
//
// Renaming a predeclared or universal name would rewrite call sites without
// changing what they call, so the server declines rather than producing an
// edit that silently breaks the file.
func (s *Server) prepareRename(d *Document, pos Position) *prepareRenameResult {
	a := s.analyze(d)
	ident := a.identAt(d, d.offsetOf(pos))
	if ident == nil || !a.isLocalToFile(ident) {
		return nil
	}
	return &prepareRenameResult{Range: d.spanRange(ident), Placeholder: ident.Name}
}

func (s *Server) handleRename(req request) error {
	var params renameParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.conn.replyError(req.ID, errInvalidParams, err.Error())
	}
	d, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return s.conn.reply(req.ID, nil)
	}
	if !isIdentifier(params.NewName) {
		return s.conn.replyError(req.ID, errInvalidParams,
			"a Starlark identifier may contain only letters, digits, and underscores")
	}

	a := s.analyze(d)
	ident := a.identAt(d, d.offsetOf(params.Position))
	if ident == nil || !a.isLocalToFile(ident) {
		return s.conn.replyError(req.ID, errInvalidRequest,
			"only a name bound in this file can be renamed")
	}

	edits := []TextEdit{}
	for _, ref := range a.referencesTo(ident) {
		edits = append(edits, TextEdit{Range: d.spanRange(ref), NewText: params.NewName})
	}
	return s.conn.reply(req.ID, WorkspaceEdit{Changes: map[string][]TextEdit{d.uri: edits}})
}

// ---------- selection ranges ----------

// handleSelectionRange answers expand-selection by walking the tree outward
// from each position.
func (s *Server) handleSelectionRange(req request) error {
	var params selectionRangeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.conn.replyError(req.ID, errInvalidParams, err.Error())
	}
	d, ok := s.docs.get(params.TextDocument.URI)
	if !ok || d.tree == nil {
		return s.conn.reply(req.ID, []SelectionRange{})
	}

	out := make([]SelectionRange, 0, len(params.Positions))
	for _, pos := range params.Positions {
		node := nodeAt(d.tree.RootNode(), d.offsetOf(pos))
		var chain []Range
		for n := node; n != nil; n = n.Parent() {
			if !n.IsNamed() {
				continue
			}
			r := d.nodeRange(n)
			if len(chain) == 0 || r != chain[len(chain)-1] {
				chain = append(chain, r)
			}
		}
		out = append(out, buildSelectionChain(chain))
	}
	return s.conn.reply(req.ID, out)
}

// buildSelectionChain links ranges innermost first into a parent chain.
func buildSelectionChain(ranges []Range) SelectionRange {
	if len(ranges) == 0 {
		return SelectionRange{}
	}
	node := SelectionRange{Range: ranges[len(ranges)-1]}
	for i := len(ranges) - 2; i >= 0; i-- {
		parent := node
		node = SelectionRange{Range: ranges[i], Parent: &parent}
	}
	return node
}

// ---------- workspace symbols ----------

// handleWorkspaceSymbol searches open documents, then the workspace on disk.
//
// The scan is bounded: it reads .star, .bzl, and .sky files under the root,
// skips hidden and vendor directories, and stops after a fixed budget. A
// language server that walks an unbounded tree on every keystroke is worse
// than one that admits a limit.
func (s *Server) handleWorkspaceSymbol(ctx context.Context, req request) error {
	var params workspaceSymbolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.conn.replyError(req.ID, errInvalidParams, err.Error())
	}
	query := strings.ToLower(params.Query)
	out := []SymbolInformation{}
	seen := make(map[string]bool)

	for _, d := range s.docs.all() {
		for _, sym := range s.documentSymbols(d) {
			if !matchesQuery(sym.Name, query) {
				continue
			}
			key := d.uri + "\x00" + sym.Name
			seen[key] = true
			out = append(out, SymbolInformation{
				Name:     sym.Name,
				Kind:     sym.Kind,
				Location: Location{URI: d.uri, Range: sym.Range},
			})
		}
	}

	for _, path := range s.workspaceFiles() {
		if checkCancelled(ctx) {
			// The user typed again; this answer is already stale.
			return s.conn.reply(req.ID, out)
		}
		uri := pathToURI(path)
		if _, open := s.docs.get(uri); open {
			continue // already covered above, with the buffer's live content
		}
		source, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		scratch := &Document{uri: uri, path: path, text: source}
		scratch.indexLines()
		s.docs.reparse(scratch)
		for _, sym := range s.documentSymbols(scratch) {
			if !matchesQuery(sym.Name, query) {
				continue
			}
			out = append(out, SymbolInformation{
				Name:     sym.Name,
				Kind:     sym.Kind,
				Location: Location{URI: uri, Range: sym.Range},
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return s.conn.reply(req.ID, out)
}

func matchesQuery(name, query string) bool {
	return query == "" || strings.Contains(strings.ToLower(name), query)
}

// maxWorkspaceFiles bounds the workspace scan.
const maxWorkspaceFiles = 2000

// starlarkExtensions are the file suffixes this server treats as Starlark.
var starlarkExtensions = map[string]bool{
	".star": true, ".bzl": true, ".sky": true, ".BUILD": true,
}

func (s *Server) workspaceFiles() []string {
	if s.rootPath == "" {
		return nil
	}
	var out []string
	_ = filepath.Walk(s.rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || len(out) >= maxWorkspaceFiles {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if starlarkExtensions[filepath.Ext(path)] {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func sortLocations(locs []Location) {
	sort.Slice(locs, func(i, j int) bool {
		a, b := locs[i].Range.Start, locs[j].Range.Start
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Character < b.Character
	})
}

// ---------- semantic tokens ----------

// semanticTokensFor encodes the whole document.
//
// Classification prefers what the host says over what the shape suggests, so a
// dialect namespace reads as a namespace and a dialect member reads as a
// method — a distinction a generic Starlark highlighter cannot make.
func (s *Server) semanticTokensFor(d *Document) semanticTokens {
	if d.tree == nil {
		return semanticTokens{Data: []uint32{}}
	}
	type token struct{ line, col, length, kind int }
	var tokens []token

	walk(d.tree.RootNode(), func(n *gotreesitter.Node) bool {
		if !n.IsNamed() {
			return true
		}
		kind := -1
		switch n.Type(s.lang) {
		case ntComment:
			kind = tokComment
		case ntString:
			kind = tokString
		case "integer", "float":
			kind = tokNumber
		case ntIdentifier:
			kind = s.classifyIdentifier(d, n)
		}
		if kind < 0 {
			return true
		}
		start, end := n.StartPoint(), n.EndPoint()
		if start.Row != end.Row {
			return true // a multi-line token cannot be one entry
		}
		tokens = append(tokens, token{
			line: int(start.Row), col: int(start.Column),
			length: int(end.Column - start.Column), kind: kind,
		})
		return true
	})

	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].line != tokens[j].line {
			return tokens[i].line < tokens[j].line
		}
		return tokens[i].col < tokens[j].col
	})

	data := make([]uint32, 0, len(tokens)*5)
	prevLine, prevCol := 0, 0
	for _, t := range tokens {
		deltaLine := t.line - prevLine
		deltaCol := t.col
		if deltaLine == 0 {
			deltaCol = t.col - prevCol
		}
		data = append(data, uint32(deltaLine), uint32(deltaCol), uint32(t.length), uint32(t.kind), 0)
		prevLine, prevCol = t.line, t.col
	}
	return semanticTokens{Data: data}
}

func (s *Server) classifyIdentifier(d *Document, n *gotreesitter.Node) int {
	name := d.nodeText(n)
	if name == "" {
		return -1
	}
	parent := n.Parent()

	if parent != nil {
		switch parent.Type(s.lang) {
		case ntFuncDef:
			if declared := childByField(parent, "name", 0, s.lang); declared != nil && declared.StartByte() == n.StartByte() {
				return tokFunction
			}
		case "parameters", "default_parameter", "typed_parameter":
			return tokParameter
		case ntAttribute:
			object := childByField(parent, "object", 0, s.lang)
			if object != nil && object.StartByte() == n.StartByte() {
				break // fall through to the global classification below
			}
			return tokMethod
		}
	}
	if sym, ok := s.globalIndex()[name]; ok {
		return sym.Kind.tokenType()
	}
	return tokVariable
}

// ---------- shared syntax helper ----------

// walkFile visits every syntax node, used where a tree-sitter walk will not do.
func walkFile(file *syntax.File, fn func(syntax.Node) bool) {
	if file == nil {
		return
	}
	syntax.Walk(file, fn)
}
