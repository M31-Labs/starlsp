# starlsp

A language server for [Starlark](https://github.com/bazelbuild/starlark), written in pure Go.

Starlark has no single dialect. Bazel, Buck, Tilt, and Starkite each add their own
builtins, their own meaning for `load()`, and their own rules. `starlsp` knows the
language specification and nothing else. A dialect supplies the rest through one
small interface, so every host gets the same editor features without writing a
language server.

```bash
go install github.com/M31-Labs/starlsp/cmd/starlsp@latest
starlsp --probe
```

The parser library carries more than 200 grammars. Build with the subset tag to
embed only the one this server needs:

```bash
go build -tags 'grammar_subset,grammar_subset_starlark' ./cmd/starlsp
```

That takes the binary from 31.6 MB to **12.9 MB**, with no change in behaviour —
the test suite passes under both.

## Why another one

[`tilt-dev/starlark-lsp`](https://github.com/tilt-dev/starlark-lsp) came first and
proved the idea. It is worth reading. These are the reasons it did not become the
foundation for this one, each of them checkable:

| | tilt-dev/starlark-lsp | starlsp |
|---|---|---|
| Parser binding | `smacker/go-tree-sitter` — **cgo**, pinned to a 2022 commit | `gotreesitter` — **pure Go** |
| Cross-compilation | needs a C toolchain per target | `GOOS=… go build` |
| Document sync | full document per edit | **incremental** |
| Builtin names | Python stub files, maintained by hand | **introspected** from `starlark.Universe` |
| Type methods | stub files | **introspected** from the runtime values |
| Dialect extension | stub files | a typed `Host` interface |
| Last code commit | July 2024 | — |
| Protocol types | `go.lsp.dev/protocol` v0.11.2 | no protocol dependency |

Capabilities, counted from what each server advertises at `initialize`:

| Capability | tilt-dev | starlsp |
|---|:--:|:--:|
| Diagnostics | ● | ● |
| Completion | ● | ● |
| Hover | ● | ● |
| Signature help | ● | ● |
| Go to definition | ● | ● |
| Document symbols | ● | ● |
| **Find references** | ○ | ● |
| **Document highlight** | ○ | ● |
| **Rename**, with prepare | ○ | ● |
| **Workspace symbols** | ○ | ● |
| **Folding ranges** | ○ | ● |
| **Selection ranges** | ○ | ● |
| **Document links** | ○ | ● |
| **Semantic tokens** | ○ | ● |

Six capabilities become fourteen. The new ones are grouped around a reference
index the resolver does not keep: `go.starlark.net/resolve` establishes which
identifiers bind to which variable, then discards the back-references. Building
that index is what makes find-references, rename, and highlight exact — a local
`x` in one function is not the same variable as a local `x` in another, and
matching by text would merge them.

## Two parsers, one opinion

The server runs two parsers because they answer different questions.

**gotreesitter** answers structural questions on every keystroke. It tolerates a
half-typed buffer, which is exactly when an editor is most useful. It supplies the
outline, folding, semantic tokens, and the caret context that completion needs.

**go.starlark.net** answers every question about correctness, because it is the
same parser and resolver a Starlark host actually runs. Diagnostics, name
binding, and scope come from there and nowhere else.

The division matters. A language server that reimplements Starlark's scope rules
eventually disagrees with the runtime, and the disagreement is always the
server's fault. Here it cannot happen: the core never gets a vote on whether a
file is correct, only on what shape it currently has.

## Extending it for a dialect

Two methods are required. Everything else is an optional interface, checked with
a type assertion, so a host implements only what it has.

```go
type myDialect struct{}

func (myDialect) Name() string { return "mydialect" }

func (myDialect) Globals() []starlsp.Symbol {
	return []starlsp.Symbol{{
		Name:      "greet",
		Kind:      starlsp.KindFunction,
		Signature: "greet(name)",
		Returns:   "None",
		Doc:       "Print a greeting.",
	}}
}

func main() {
	srv, err := starlsp.New(starlsp.Options{
		Host: starlsp.Hosts(starlsp.NewVanilla(), myDialect{}),
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(srv.Run())
}
```

That alone gives `greet` completion, hover with its signature, signature help
with its parameter, correct semantic-token colour, and — because the resolver is
told the name exists — no false "undefined" diagnostic.

The optional interfaces:

| Interface | Adds |
|---|---|
| `MemberProvider` | completion after a dot |
| `TypeResolver` | what `p` is, after `p = fs.path(...)` |
| `LoadResolver` | go to definition across `load()` |
| `Linter` | diagnostics belonging to the dialect |
| `Documenter` | prose and signatures held elsewhere |

`Hosts(a, b, …)` composes hosts. Later hosts win on a name collision, so a
dialect layers over the specification without either knowing about the other.

### A note on introspection

Prefer introspecting your runtime over writing a table. The vanilla host does:
its globals come from `starlark.Universe` and its methods from calling
`AttrNames` on a zero `String`, `List`, `Dict`, `Set`, and `Bytes`. A build of
`go.starlark.net` that adds a method gains completion for it with no change here.

A hand-written table tracking a moving API rots. The only hand-written data in
this package is prose for the Starlark specification, which is frozen.

## Checking a build

`--probe` prints what the server would offer and exits, so a missing completion
can be diagnosed without an editor.

```console
$ starlsp --probe
starlsp 0.1.0

host           starlark
parser         starlark (gotreesitter, pure Go)
globals        49
hooks          members, types

types       10  bool bytes dict float int list range set str tuple
functions   20  abs all any chr dir enumerate fail getattr hasattr hash len max min ord print repr reversed sorted type zip
constants    3  False None True
keywords    16  and break continue def elif else for if in lambda load not or pass return while

members
  bytes            1
  dict             9
  list             7
  set             12
  string          35
```

## Editor setup

### VS Code

Any generic LSP client works. With `vscode-languageclient`, run `starlsp` with no
arguments over stdio.

### Neovim

```lua
vim.filetype.add({ extension = { star = "starlark", bzl = "starlark" } })

vim.lsp.config.starlsp = {
  cmd = { "starlsp" },
  filetypes = { "starlark" },
  root_markers = { ".git" },
}
vim.lsp.enable("starlsp")
```

### Helix

```toml
[language-server.starlsp]
command = "starlsp"

[[language]]
name = "starlark"
scope = "source.star"
file-types = ["star", "bzl", "sky"]
comment-token = "#"
indent = { tab-width = 4, unit = "    " }
language-servers = ["starlsp"]
```

### Zed

```json
{
  "lsp": { "starlsp": { "binary": { "path": "starlsp" } } }
}
```

## Status

Version 0.1.0. The protocol surface and the `Host` interface are settled enough to
build against; both may still change before 1.0. Issues and dialect hosts are
welcome.

## License

Apache 2.0. See [LICENSE](LICENSE).
