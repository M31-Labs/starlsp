package starlsp

import (
	"sort"

	"go.starlark.net/syntax"
)

// A Host teaches the server about one Starlark dialect.
//
// Starlark has no single dialect. Bazel, Buck, Tilt, and Starkite each add
// their own builtins, their own meaning for load(), and their own rules about
// what a well-formed file looks like. The core of this server knows the
// language specification and nothing else; a Host supplies the rest.
//
// Only Name and Globals are required. Everything else is an optional interface,
// checked with a type assertion, so a host that merely adds a few builtins
// implements two methods rather than six.
//
// The vanilla host in this package implements Host alone, and is what the
// standalone binary uses when no dialect is configured.
//
// The server calls a host from several goroutines at once: read-only requests
// run concurrently with each other and with the analysis that follows an edit.
// A host must therefore be safe for concurrent use. The usual way is to build
// its tables once, behind a sync.Once, and only read them afterwards.
type Host interface {
	// Name identifies the dialect in server logs and in the initialize
	// response, for example "bazel" or "starkite".
	Name() string

	// Globals returns the names the host predeclares, beyond the universe the
	// Starlark specification defines. The server treats these as defined, so
	// they are also what stops the resolver reporting them as undefined names.
	//
	// A host that composes over NewVanilla also returns the universe names, so
	// completion and hover can offer them. For resolution the server still
	// treats every universe name as the universe, not as a name this host
	// predeclared. See UniverseRemover for a dialect that lacks some of them.
	//
	// The server calls Globals once, the first time it needs the names, and keeps
	// the answer for its lifetime. Completion, hover, resolution and the probe
	// then agree with each other. A host whose list changes while the server runs
	// is not supported; start a new Server instead.
	Globals() []Symbol
}

// A MemberProvider supplies the names reachable through a dot.
//
// owner is either a namespace the host predeclared — "fs" — or a type name
// that a TypeResolver produced — "fs.path". A host that has no member
// structure does not implement this, and the server offers nothing after a dot
// rather than guessing.
type MemberProvider interface {
	Members(owner string) []Symbol
}

// A TypeResolver maps an expression to the owner whose members it exposes.
//
// The server calls it with the receiver text left of the caret's dot, already
// trimmed: "fs", "fs.path(\"/etc\")", or a bare identifier the server has
// resolved to its binding expression. Returning false means the server offers
// no completion, which is deliberately better than offering the wrong list.
type TypeResolver interface {
	ResolveType(req TypeRequest) (owner string, ok bool)
}

// TypeRequest describes one type-inference question.
type TypeRequest struct {
	// Expr is the receiver expression, for example "fs.path(\"/etc/hosts\")".
	Expr string

	// Document is the buffer the expression appears in, so a host can consult
	// the file's own bindings.
	Document *Document

	// Resolve re-enters the server's inference for a sub-expression. A host
	// uses it to walk a chain without reimplementing the walk.
	Resolve func(expr string) (owner string, ok bool)
}

// A LoadResolver maps a load() target to a file.
//
// This is the most dialect-specific hook in the interface. Bazel takes a label,
// Tilt takes a path, and Starkite takes either a path or a "namespace/name"
// identity pinned by a lockfile. The core never guesses; without this hook a
// load() target is highlighted but not navigable.
type LoadResolver interface {
	ResolveLoad(req LoadRequest) (path string, ok bool)
}

// LoadRequest describes one load() target to resolve.
type LoadRequest struct {
	// Target is the literal text inside the load() call, without quotes.
	Target string

	// From is the filesystem path of the file containing the load(), or empty
	// when the buffer is not on disk.
	From string
}

// A Linter reports diagnostics that belong to the dialect rather than to the
// language, such as an entry-point convention or a permission rule.
//
// The server calls it after its own analysis. File is nil when the buffer does
// not parse, so a linter that needs an AST must check for that and return
// nothing rather than guess at a broken buffer.
type Linter interface {
	Lint(doc *Document, file *syntax.File) []Diagnostic
}

// A Documenter supplies prose and signatures the server cannot introspect.
//
// A Starlark builtin carries neither its parameter names nor its description,
// so a host that has documentation elsewhere — reference files, stub files, Go
// doc comments — surfaces it here. Symbols returned from Globals and Members
// may carry their own documentation instead; this hook exists for hosts whose
// documentation is not attached to the values.
type Documenter interface {
	Document(owner, name string) (Symbol, bool)
}

// hostCapabilities is the resolved set of optional interfaces one host
// implements, computed once so hot paths do not repeat type assertions.
type hostCapabilities struct {
	members MemberProvider
	types   TypeResolver
	loads   LoadResolver
	linter  Linter
	docs    Documenter
	dialect DialectOptions
}

func capabilitiesOf(h Host) hostCapabilities {
	var c hostCapabilities
	c.members, _ = h.(MemberProvider)
	c.types, _ = h.(TypeResolver)
	c.loads, _ = h.(LoadResolver)
	c.linter, _ = h.(Linter)
	c.docs, _ = h.(Documenter)
	c.dialect, _ = h.(DialectOptions)
	return c
}

// Hosts composes several hosts into one.
//
// Names from later hosts take precedence on collision, which lets a dialect
// layer over a base without either knowing about the other. Optional
// interfaces are honoured for every member: a composite answers a member
// query from the first host that has an answer.
func Hosts(hosts ...Host) Host { return composite(hosts) }

type composite []Host

func (c composite) Name() string {
	names := make([]string, 0, len(c))
	for _, h := range c {
		names = append(names, h.Name())
	}
	return joinNonEmpty(names, "+")
}

func (c composite) Globals() []Symbol {
	seen := make(map[string]int)
	var out []Symbol
	for _, h := range c {
		for _, sym := range h.Globals() {
			if idx, ok := seen[sym.Name]; ok {
				out[idx] = sym // a later host wins
				continue
			}
			seen[sym.Name] = len(out)
			out = append(out, sym)
		}
	}
	return out
}

func (c composite) Members(owner string) []Symbol {
	for i := len(c) - 1; i >= 0; i-- {
		if mp, ok := c[i].(MemberProvider); ok {
			if members := mp.Members(owner); len(members) > 0 {
				return members
			}
		}
	}
	return nil
}

func (c composite) ResolveType(req TypeRequest) (string, bool) {
	for i := len(c) - 1; i >= 0; i-- {
		if tr, ok := c[i].(TypeResolver); ok {
			if owner, found := tr.ResolveType(req); found {
				return owner, true
			}
		}
	}
	return "", false
}

func (c composite) ResolveLoad(req LoadRequest) (string, bool) {
	for i := len(c) - 1; i >= 0; i-- {
		if lr, ok := c[i].(LoadResolver); ok {
			if path, found := lr.ResolveLoad(req); found {
				return path, true
			}
		}
	}
	return "", false
}

func (c composite) Lint(doc *Document, file *syntax.File) []Diagnostic {
	var out []Diagnostic
	for _, h := range c {
		if l, ok := h.(Linter); ok {
			out = append(out, l.Lint(doc, file)...)
		}
	}
	return out
}

func (c composite) FileOptions() *syntax.FileOptions {
	for i := len(c) - 1; i >= 0; i-- {
		if d, ok := c[i].(DialectOptions); ok {
			if opts := d.FileOptions(); opts != nil {
				return opts
			}
		}
	}
	return nil
}

// RemovedUniverse is the union of what the members remove. A dialect that lacks
// a name lacks it however the hosts are ordered, so unlike Globals this does not
// let a later host undo an earlier one.
func (c composite) RemovedUniverse() []string {
	var out []string
	seen := make(map[string]bool)
	for _, h := range c {
		r, ok := h.(UniverseRemover)
		if !ok {
			continue
		}
		for _, name := range r.RemovedUniverse() {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (c composite) Document(owner, name string) (Symbol, bool) {
	for i := len(c) - 1; i >= 0; i-- {
		if d, ok := c[i].(Documenter); ok {
			if sym, found := d.Document(owner, name); found {
				return sym, true
			}
		}
	}
	return Symbol{}, false
}

var (
	_ Host           = composite(nil)
	_ MemberProvider = composite(nil)
	_ TypeResolver   = composite(nil)
	_ LoadResolver   = composite(nil)
	_ Linter         = composite(nil)
	_ Documenter     = composite(nil)
	_ DialectOptions = composite(nil)

	_ UniverseRemover = composite(nil)
)

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

// A DialectOptions host controls the language variant the parser and resolver
// enforce.
//
// Starlark is not one language here either. go.starlark.net gates several
// behaviours behind syntax.FileOptions, and real dialects differ on them:
// Tilt permits if and for at the top level, Bazel does not; some hosts enable
// the set builtin, most do not. Getting this wrong produces a syntax error on
// a file that its own runtime accepts, which is the worst failure a language
// server has.
//
// A host that does not implement this gets the specification defaults.
//
// The Set field decides whether the universe's set builtin exists. With it off,
// which is the default, a call to set is reported with the message that
// go.starlark.net gives: "this Starlark dialect does not support sets". The
// resolver reports it at the first use in a file, not at every use. The server
// also stops offering set, in completion, hover and the probe, so it does not
// suggest a name it will then call an error. A dialect that cannot tell whether
// its runtime has sets should turn the field on: an error on a file the runtime
// accepts is worse than an error the server misses.
type DialectOptions interface {
	FileOptions() *syntax.FileOptions
}

// A UniverseRemover host runs a dialect whose universe lacks some of the names
// the Starlark specification defines.
//
// The server resolves a universe name as the universe, so a dialect that has
// the name needs to say nothing. A dialect that does not have it says so here,
// and the server then reports every use of the name as "undefined", the same
// message the dialect's runtime gives. It also stops offering the name in
// completion and hover.
//
// Removal is by name. The server drops every entry of Globals under a removed
// name, including one the host lists itself. It cannot tell such an entry from a
// copy of the universe's own: a catalog built from a runtime lists every universe
// name as its own global, so the entry under a removed name is usually that copy.
// A dialect that replaces a universe function with its own lists the replacement
// in Globals and does not remove the name; the replacement is then what
// completion offers.
//
// This is for names that are absent from the universe. A name that is present
// but switched off by FileOptions, such as set, is handled by DialectOptions.
// Names that are not in the universe, and keywords, are ignored.
//
// Hosts(a, b, ...) removes the union of what its members remove.
type UniverseRemover interface {
	RemovedUniverse() []string
}

// SpecFileOptions is the Starlark specification's own dialect: no top-level
// control flow, no while, no set, no global reassignment.
func SpecFileOptions() *syntax.FileOptions {
	return &syntax.FileOptions{}
}
