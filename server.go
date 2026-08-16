package starlsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// Version is reported to the client at initialize.
const Version = "0.2.1"

// Options configures a server.
type Options struct {
	// Host supplies the dialect. Defaults to NewVanilla, which is Starlark as
	// the specification defines it.
	Host Host

	// In and Out are the client transport. Both default to stdio.
	In  io.Reader
	Out io.Writer

	// Log receives server-side messages. It must never be Out, because
	// anything written there corrupts the protocol stream.
	Log io.Writer
}

// Server is a Starlark language server.
//
// It is usable two ways. Run drives it over a transport, which is what the
// standalone binary does. A host application can also embed it and supply its
// own Host, which is the reason the type and its Options are exported.
type Server struct {
	opts Options
	host Host
	caps hostCapabilities

	conn *conn
	docs *documentStore
	lang *gotreesitter.Language

	analyses analysisCache

	globalOnce sync.Once
	globals    map[string]Symbol

	rootPath string

	// resilience machinery; see dispatch.go
	pending *inflight
	slots   chan struct{}
	active  sync.WaitGroup

	mu       sync.Mutex
	shutdown bool
	exiting  bool
}

// New builds a server. It touches no transport until Run is called.
func New(opts Options) (*Server, error) {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	if opts.Host == nil {
		opts.Host = NewVanilla()
	}

	entry := grammars.DetectLanguageByName("starlark")
	if entry == nil || entry.Language == nil {
		return nil, errors.New("starlsp: the starlark grammar is not compiled into this binary")
	}
	lang := entry.Language()
	if lang == nil {
		return nil, errors.New("starlsp: the starlark grammar failed to load")
	}

	return &Server{
		opts:    opts,
		host:    opts.Host,
		caps:    capabilitiesOf(opts.Host),
		conn:    newConn(opts.In, opts.Out),
		docs:    newDocumentStore(lang),
		lang:    lang,
		pending: newInflight(),
		slots:   make(chan struct{}, maxConcurrentRequests),
	}, nil
}

// globalIndex returns the host's global names, keyed for lookup.
func (s *Server) globalIndex() map[string]Symbol {
	s.globalOnce.Do(func() {
		symbols := s.host.Globals()
		s.globals = make(map[string]Symbol, len(symbols))
		for _, sym := range symbols {
			s.globals[sym.Name] = sym
		}
	})
	return s.globals
}

// lookupSymbol finds a symbol by owner and name, consulting the host's
// documentation hook when the symbol itself carries none.
func (s *Server) lookupSymbol(owner, name string) (Symbol, bool) {
	var found Symbol
	var ok bool

	if owner == "" {
		found, ok = s.globalIndex()[name]
	} else if s.caps.members != nil {
		for _, m := range s.caps.members.Members(owner) {
			if m.Name == name {
				found, ok = m, true
				break
			}
		}
	}
	if ok && found.Doc == "" && found.Signature == "" && s.caps.docs != nil {
		if documented, has := s.caps.docs.Document(owner, name); has {
			documented.Name, documented.Owner = found.Name, found.Owner
			if documented.Kind == KindUnknown {
				documented.Kind = found.Kind
			}
			return documented, true
		}
	}
	return found, ok
}

func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(s.opts.Log, "starlsp: "+format+"\n", args...)
}

var errExit = errors.New("exit")

func (r request) isNotification() bool { return len(r.ID) == 0 }

func (s *Server) dispatch(ctx context.Context, req request) error {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "initialized":
		return nil
	case "shutdown":
		s.mu.Lock()
		s.shutdown = true
		s.mu.Unlock()
		return s.conn.reply(req.ID, nil)
	case "exit":
		return errExit
	case "$/cancelRequest":
		return s.handleCancel(req)
	case "$/setTrace":
		return nil
	case "workspace/didChangeConfiguration", "workspace/didChangeWatchedFiles":
		// A dialect host may resolve load() through files on disk, so drop any
		// cached analysis and let the next request rebuild it.
		s.analyses.invalidateAll()
		return nil

	case "textDocument/didOpen":
		return s.handleDidOpen(req)
	case "textDocument/didChange":
		return s.handleDidChange(req)
	case "textDocument/didSave":
		return s.handleDidSave(req)
	case "textDocument/didClose":
		return s.handleDidClose(req)

	case "textDocument/completion":
		return s.positional(ctx, req, func(d *Document, p Position) any { return s.complete(d, p) })
	case "textDocument/hover":
		return s.positional(ctx, req, func(d *Document, p Position) any { return orNil(s.hover(d, p)) })
	case "textDocument/signatureHelp":
		return s.positional(ctx, req, func(d *Document, p Position) any { return orNil(s.signatureHelp(d, p)) })
	case "textDocument/definition":
		return s.positional(ctx, req, func(d *Document, p Position) any { return s.definition(d, p) })
	case "textDocument/documentHighlight":
		return s.positional(ctx, req, func(d *Document, p Position) any { return s.documentHighlight(d, p) })
	case "textDocument/references":
		return s.handleReferences(req)
	case "textDocument/prepareRename":
		return s.positional(ctx, req, func(d *Document, p Position) any { return orNil(s.prepareRename(d, p)) })
	case "textDocument/rename":
		return s.handleRename(req)
	case "textDocument/selectionRange":
		return s.handleSelectionRange(req)

	case "textDocument/documentSymbol":
		return s.perDocument(ctx, req, func(d *Document) any { return s.documentSymbols(d) })
	case "textDocument/foldingRange":
		return s.perDocument(ctx, req, func(d *Document) any { return s.foldingRanges(d) })
	case "textDocument/documentLink":
		return s.perDocument(ctx, req, func(d *Document) any { return s.documentLinks(d) })
	case "textDocument/semanticTokens/full":
		return s.perDocument(ctx, req, func(d *Document) any { return s.semanticTokensFor(d) })
	case "workspace/symbol":
		return s.handleWorkspaceSymbol(ctx, req)

	default:
		if req.isNotification() {
			return nil
		}
		return s.conn.replyError(req.ID, errMethodNotFound, "unsupported method "+req.Method)
	}
}

// orNil converts a typed nil pointer into an untyped nil, so the reply is
// JSON null rather than an object.
func orNil[T any](v *T) any {
	if v == nil {
		return nil
	}
	return v
}

func (s *Server) handleInitialize(req request) error {
	var params initializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return s.conn.replyError(req.ID, errInvalidParams, err.Error())
		}
	}
	s.rootPath = params.RootPath
	if s.rootPath == "" && params.RootURI != "" {
		s.rootPath = uriToPath(params.RootURI)
	}

	// Warm the host's globals now rather than on the first keystroke.
	go func() {
		globals := s.globalIndex()
		s.logf("host %q ready: %d global names", s.host.Name(), len(globals))
	}()

	return s.conn.reply(req.ID, initializeResult{
		ServerInfo: serverInfo{Name: "starlsp (" + s.host.Name() + ")", Version: Version},
		Capabilities: serverCapabilities{
			TextDocumentSync:          2, // incremental
			CompletionProvider:        &completionOptions{TriggerCharacters: []string{".", "\"", "'"}},
			HoverProvider:             true,
			SignatureHelpProvider:     &signatureHelpOptions{TriggerCharacters: []string{"(", ","}, RetriggerCharacters: []string{","}},
			DefinitionProvider:        true,
			ReferencesProvider:        true,
			DocumentHighlightProvider: true,
			RenameProvider:            &renameOptions{PrepareProvider: true},
			DocumentSymbolProvider:    true,
			WorkspaceSymbolProvider:   true,
			FoldingRangeProvider:      true,
			SelectionRangeProvider:    true,
			DocumentLinkProvider:      &struct{}{},
			SemanticTokensProvider: &semanticTokensOptions{
				Legend: semanticTokensLegend{TokenTypes: semanticTokenTypes, TokenModifiers: []string{}},
				Full:   true,
			},
		},
	})
}

func (s *Server) handleDidOpen(req request) error {
	var params didOpenParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return err
	}
	d := s.docs.open(params.TextDocument.URI, params.TextDocument.Version, params.TextDocument.Text)
	s.publishDiagnostics(d)
	return nil
}

func (s *Server) handleDidChange(req request) error {
	var params didChangeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return err
	}
	d := s.docs.change(params.TextDocument.URI, params.TextDocument.Version, params.ContentChanges)
	if d == nil {
		return nil
	}
	s.publishDiagnostics(d)
	return nil
}

func (s *Server) handleDidSave(req request) error {
	var params didSaveParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return err
	}
	if d, ok := s.docs.get(params.TextDocument.URI); ok {
		s.publishDiagnostics(d)
	}
	return nil
}

func (s *Server) handleDidClose(req request) error {
	var params didCloseParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return err
	}
	s.docs.close(params.TextDocument.URI)
	s.analyses.invalidate(params.TextDocument.URI)
	return s.conn.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{
		URI:         params.TextDocument.URI,
		Diagnostics: []Diagnostic{},
	})
}

func (s *Server) positional(ctx context.Context, req request, fn func(*Document, Position) any) error {
	var params textDocumentPositionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.conn.replyError(req.ID, errInvalidParams, err.Error())
	}
	d, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return s.conn.reply(req.ID, nil)
	}
	if checkCancelled(ctx) {
		return s.conn.replyError(req.ID, errRequestCancelled, "cancelled")
	}
	return s.conn.reply(req.ID, fn(d, params.Position))
}

func (s *Server) perDocument(ctx context.Context, req request, fn func(*Document) any) error {
	var params documentSymbolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.conn.replyError(req.ID, errInvalidParams, err.Error())
	}
	d, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return s.conn.reply(req.ID, nil)
	}
	if checkCancelled(ctx) {
		return s.conn.replyError(req.ID, errRequestCancelled, "cancelled")
	}
	return s.conn.reply(req.ID, fn(d))
}

// publishDiagnostics sends the merged view of every source.
func (s *Server) publishDiagnostics(d *Document) {
	diags := s.diagnosticsFor(d)
	if diags == nil {
		diags = []Diagnostic{}
	}
	if err := s.conn.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{
		URI:         d.uri,
		Version:     d.version,
		Diagnostics: diags,
	}); err != nil {
		s.logf("publishing diagnostics: %v", err)
	}
}

// diagnosticsFor merges the authoritative pass, the specification lints, and
// whatever the host reports.
//
// The tree-sitter view only contributes when the real parser produced nothing,
// because two parsers reporting one error twice is worse than one reporting it
// once.
func (s *Server) diagnosticsFor(d *Document) []Diagnostic {
	a := s.analyze(d)

	out := append([]Diagnostic(nil), a.Diagnostics...)
	if a.File == nil {
		for _, diag := range s.structuralDiagnostics(d) {
			if diag.Source == sourceTreeSitter {
				out = append(out, diag)
			}
		}
	}
	if s.caps.linter != nil {
		out = append(out, s.caps.linter.Lint(d, a.File)...)
	}
	return dedupeDiagnostics(out)
}

func dedupeDiagnostics(in []Diagnostic) []Diagnostic {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, d := range in {
		key := fmt.Sprintf("%d:%d:%s", d.Range.Start.Line, d.Range.Start.Character, d.Message)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// Probe renders what this server would offer, without a client.
//
// It exists so a host's surface can be inspected directly. Anyone debugging a
// missing completion can see exactly which names and owners the configured
// host exposes, rather than inferring it from an editor.
func (s *Server) Probe() string {
	globals := s.host.Globals()

	var b strings.Builder
	fmt.Fprintf(&b, "starlsp %s\n\n", Version)
	fmt.Fprintf(&b, "host           %s\n", s.host.Name())
	fmt.Fprintf(&b, "parser         starlark (gotreesitter, pure Go)\n")
	fmt.Fprintf(&b, "globals        %d\n", len(globals))

	fmt.Fprintf(&b, "hooks          ")
	var hooks []string
	if s.caps.members != nil {
		hooks = append(hooks, "members")
	}
	if s.caps.types != nil {
		hooks = append(hooks, "types")
	}
	if s.caps.loads != nil {
		hooks = append(hooks, "loads")
	}
	if s.caps.linter != nil {
		hooks = append(hooks, "lint")
	}
	if s.caps.docs != nil {
		hooks = append(hooks, "docs")
	}
	if len(hooks) == 0 {
		hooks = append(hooks, "(none beyond Globals)")
	}
	fmt.Fprintf(&b, "%s\n\n", strings.Join(hooks, ", "))

	byKind := make(map[Kind][]string)
	for _, sym := range globals {
		byKind[sym.Kind] = append(byKind[sym.Kind], sym.Name)
	}
	for _, k := range []Kind{KindNamespace, KindType, KindFunction, KindConstant, KindKeyword, KindVariable} {
		names := byKind[k]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "%-11s %2d  %s\n", kindLabel(k), len(names), strings.Join(names, " "))
	}

	if s.caps.members != nil {
		fmt.Fprintf(&b, "\nmembers\n")
		for _, owner := range s.probeOwners() {
			fmt.Fprintf(&b, "  %-16s %d\n", owner, len(s.caps.members.Members(owner)))
		}
	}
	return b.String()
}

// probeOwners lists the owners worth reporting: every namespace or type the
// host declares as a global, plus the builtin types the specification defines.
func (s *Server) probeOwners() []string {
	seen := map[string]bool{}
	var owners []string
	for _, name := range []string{"string", "list", "dict", "set", "bytes"} {
		if len(s.caps.members.Members(name)) > 0 {
			seen[name] = true
			owners = append(owners, name)
		}
	}
	for _, sym := range s.host.Globals() {
		if sym.Kind != KindNamespace || seen[sym.Name] {
			continue
		}
		if len(s.caps.members.Members(sym.Name)) == 0 {
			continue
		}
		seen[sym.Name] = true
		owners = append(owners, sym.Name)
	}
	sort.Strings(owners)
	return owners
}

func kindLabel(k Kind) string {
	switch k {
	case KindNamespace:
		return "namespaces"
	case KindType:
		return "types"
	case KindFunction:
		return "functions"
	case KindConstant:
		return "constants"
	case KindKeyword:
		return "keywords"
	default:
		return "variables"
	}
}

// Diagnose analyses one document and returns its diagnostics, with no client
// and no transport.
//
// It is the whole server minus the protocol: the same parse, the same
// resolver, the same host lints an editor would see. That makes it usable as a
// linter — in CI, in a pre-commit hook, or in a program that embeds this
// package — without pretending to be an editor.
//
// uri may be a file:// URI or a plain path. It is used for error messages and
// for any host hook that resolves relative to the file.
func (s *Server) Diagnose(uri, text string) []Diagnostic {
	if !strings.HasPrefix(uri, "file://") && strings.HasPrefix(uri, "/") {
		uri = pathToURI(uri)
	}
	d := s.docs.open(uri, 1, text)
	defer s.docs.close(uri)
	out := s.diagnosticsFor(d)
	if out == nil {
		return []Diagnostic{}
	}
	return out
}

// DiagnoseFile reads a file from disk and analyses it.
func (s *Server) DiagnoseFile(path string) ([]Diagnostic, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return s.Diagnose(pathToURI(path), string(source)), nil
}
