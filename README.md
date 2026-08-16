# starlsp

A language server for [Starlark](https://github.com/bazelbuild/starlark), written in pure Go.
No cgo, no C toolchain, no build tags — `go install` and it works.

```bash
go install github.com/M31-Labs/starlsp/cmd/starlsp@latest
```

Starlark has no single dialect. Bazel, Buck, Tilt, and Starkite each add their own
builtins, their own meaning for `load()`, and their own rules about what a
well-formed file even is. `starlsp` knows the language specification; a dialect
supplies the rest through one small interface. **Bazel and Tilt ship in the box.**

```bash
starlsp --dialect bazel      # BUILD, BUILD.bazel, WORKSPACE, MODULE.bazel, .bzl
starlsp --dialect tilt       # Tiltfile
starlsp                      # auto-detects from the working directory
```

## It is quiet on real code

The measurement that matters for a language server is how often it is wrong about
working code. Against 461 files from `bazelbuild/rules_go`,
`bazelbuild/bazel-skylib`, and `tilt-dev/tilt`:

| Corpus | Files | Errors, spec host | Errors, dialect host |
|---|--:|--:|--:|
| Bazel `.bzl` / `BUILD` / `WORKSPACE` | 426 | 1,275 | **0** |
| Tiltfiles | 35 | 195 | **0** |

Zero. Not "few" — every file that Bazel and Tilt accept, `starlsp` accepts.

Two things that number depends on, both of which are easy to get wrong:

- **Tilt permits top-level `if` and `for`.** Bazel does not. A server that hard-codes
  the specification reports a syntax error on real Tiltfiles. Dialects carry
  `syntax.FileOptions`, so the parser and resolver enforce the variant the host
  actually runs.
- **The builtin sets were checked against real code**, not just documentation. Every
  name those three repositories reference is present.

Robustness is measured the same way: **21,740 positional requests** — hover,
completion, signature help, definition, highlight — driven at a stride across 60
real files, with no crash. Plus out-of-range positions, negative lines, and
requests for documents that were never opened.

The protocol is verified against **Microsoft's own LSP client libraries**
(`vscode-languageserver-protocol`), the layers VS Code's `LanguageClient` is
built on — not a hand-written harness that might agree with the server about
what the protocol means. 18 end-to-end exchanges, run in CI.

## Built to survive a working day

A language server runs for hours inside an editor, and the failures that matter
there are not wrong answers:

- **A panic cannot kill the session.** Every handler is recovered; the client gets
  an error response and the server carries on. Hosts are written by other people,
  so a host that panics is tested for explicitly.
- **Slow work does not block typing.** Read-only requests run concurrently under a
  bounded pool; only the notifications whose order is their meaning — didOpen,
  didChange, didClose — are serialised.
- **Cancellation is honoured.** `$/cancelRequest` stops work nobody is waiting
  for, including a workspace scan mid-walk.
- **The outline does not vanish while you type.** Typing the dot in `x = foo.`
  degenerates the whole parse into one error node — exactly when you are asking
  for completion — so navigation falls back to the last tree that had structure.
  Highlighting deliberately does not, because stale token ranges colour the wrong
  characters.
- **The analysis cache is per document.** A single-entry cache is worse than none
  once two files are open, since every switch evicts the other.

Tested against adversarial input — nul bytes, invalid UTF-8, a 100,000-character
line, 500-deep nesting, unterminated strings, CRLF, a BOM — and under `-race`
with two dozen goroutines hammering the same documents.

## Also a linter

The server without the protocol. Same parser, same resolver, same host — so a file
clean in CI is clean in the editor, and neither can drift from the other.

```console
$ starlsp --dialect tilt --check Tiltfile
$ echo $?
0

$ starlsp --dialect starlark --check Tiltfile
Tiltfile:5:1: error: if statement not within a function
Tiltfile:6:3: error: undefined: k8s_yaml
$ echo $?
1
```

Embeddable too — `Server.Diagnose(uri, text)` and `Server.DiagnoseFile(path)` need
no client and no transport.

## Why another one

[`tilt-dev/starlark-lsp`](https://github.com/tilt-dev/starlark-lsp) came first and
proved the idea. It is worth reading, and this project owes it. The reasons it did
not become the foundation are specific and checkable:

| | tilt-dev/starlark-lsp | starlsp |
|---|---|---|
| Parser binding | `smacker/go-tree-sitter` — **cgo**, pinned to a 2022 commit | `gotreesitter` — **pure Go** |
| Cross-compilation | needs a C toolchain per target | `GOOS=… go build` |
| Document sync | full document per edit | **incremental** |
| Builtin names | Python stub files, maintained by hand | **introspected** from `starlark.Universe` |
| Type methods | stub files | **introspected** from the runtime values |
| Dialect extension | stub files | a typed `Host` interface |
| Dialect *rules* | fixed | `syntax.FileOptions` per host |
| Last code commit | July 2024 | — |

Capabilities, counted from what each advertises at `initialize`:

| | tilt-dev | starlsp |
|---|:--:|:--:|
| Diagnostics · completion · hover · signature help · definition · document symbols | ● | ● |
| **References · document highlight · rename (+prepare) · workspace symbols** | ○ | ● |
| **Folding · selection ranges · document links · semantic tokens** | ○ | ● |

Six become fourteen. The new ones hang off a **reference index** the resolver does
not keep: `go.starlark.net/resolve` establishes which identifiers bind to which
variable, then discards the back-references. Rebuilding that is what makes rename
exact — a local `x` in one function is not the same variable as a local `x` in
another, and matching by text merges them.

## Two parsers, one opinion

**gotreesitter** answers structural questions on every keystroke. It tolerates a
half-typed buffer, which is exactly when an editor is most useful — outline,
folding, semantic tokens, and the caret context completion needs.

**go.starlark.net** answers every question about correctness, because it is the same
parser and resolver a Starlark host runs. Diagnostics, binding, and scope come from
there and nowhere else.

A language server that reimplements Starlark's scope rules eventually disagrees
with the runtime, and the disagreement is always the server's fault. Here it cannot
happen: the core never gets a vote on whether a file is correct, only on what shape
it currently has.

## Extending it for a dialect

Two methods are required. Everything else is an optional interface, found by type
assertion, so a host implements only what it has.

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
	srv, _ := starlsp.New(starlsp.Options{
		Host: starlsp.Hosts(starlsp.NewVanilla(), myDialect{}),
	})
	log.Fatal(srv.Run())
}
```

That alone gives `greet` completion, hover with its signature, signature help with
its parameter, the right semantic-token colour, and — because the resolver is told
the name exists — no false "undefined" diagnostic.

| Optional interface | Adds |
|---|---|
| `MemberProvider` | completion after a dot |
| `TypeResolver` | what `p` is, after `p = fs.path(...)` |
| `LoadResolver` | go to definition across `load()` |
| `Linter` | diagnostics belonging to the dialect |
| `Documenter` | prose and signatures held elsewhere |
| `DialectOptions` | `syntax.FileOptions` — top-level control flow, `while`, `set` |

`Hosts(a, b, …)` composes them; later hosts win a name collision.

Real example: [Starkite](https://github.com/project-starkite/starkite) implements six
of these in ~1,200 lines and gets the whole server.

### Prefer introspection over tables

The vanilla host reads its globals from `starlark.Universe` and its methods from
calling `AttrNames` on a zero `String`, `List`, `Dict`, `Set`, and `Bytes`. A
`go.starlark.net` release that adds a method gains completion for it with no change
here. Starkite reads its surface from a live module registry the same way.

The Bazel and Tilt hosts *cannot* do this — neither is a Go library — so they carry
curated tables, and say so. A table tracking a moving API rots; that is a cost those
two dialects pay and the others do not.

## Checking a build

```console
$ starlsp --dialect bazel --probe
starlsp 0.3.0

host           starlark+bazel
parser         starlark (gotreesitter, pure Go)
globals        125
hooks          members, types

namespaces  12  apple_common attr cc_common config config_common coverage_common
                java_common json native platform_common proto testing
types       30  CcInfo DefaultInfo JavaInfo OutputGroupInfo ProtoInfo PyInfo …
functions   65  Label aspect bazel_dep depset exec_group glob module provider
                rule select struct transition …

members
  attr             13
  native            9
  string           35
```

## Editors

### Neovim

```lua
vim.filetype.add({
  extension = { star = "starlark", bzl = "starlark" },
  filename = { BUILD = "starlark", ["BUILD.bazel"] = "starlark", Tiltfile = "starlark" },
})

vim.lsp.config.starlsp = {
  cmd = { "starlsp" },
  filetypes = { "starlark" },
  root_markers = { "WORKSPACE", "MODULE.bazel", "Tiltfile", ".git" },
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
file-types = ["star", "bzl", "sky", { glob = "BUILD" }, { glob = "BUILD.bazel" }, { glob = "Tiltfile" }]
comment-token = "#"
indent = { tab-width = 4, unit = "    " }
language-servers = ["starlsp"]
```

### Zed

```json
{ "lsp": { "starlsp": { "binary": { "path": "starlsp" } } } }
```

### VS Code

An extension lives in [`editors/vscode`](editors/vscode). Until it is on the
Marketplace, one command builds and installs both halves:

```bash
make install
```

That installs the server with `go install` and the extension into VS Code, then
verifies the editor actually reports it. `make help` lists the rest.

> **On WSL**, `code` is a shim onto the Windows build, and installing a vsix over
> the `\\wsl.localhost` share fails intermittently with `code: 'Extract'`. The
> install script stages the archive on Windows-native storage to avoid that share,
> retries, and confirms the result. Nothing to do by hand.

It contributes the `starlark` language for `.star`, `.bzl`, `.sky`, `BUILD`,
`BUILD.bazel`, `WORKSPACE`, `MODULE.bazel`, and `Tiltfile`, and starts the server
for them. Settings:

| Setting | Default | |
|---|---|---|
| `starlsp.serverPath` | `starlsp` | path to the binary |
| `starlsp.dialect` | `auto` | `starlark`, `bazel`, `tilt`, or `auto` |
| `starlsp.trace.server` | `off` | log the traffic |

`STARLSP_PATH` overrides `serverPath`, which is convenient when working on the
server itself.

## Smaller binaries

The parser library carries more than 200 grammars. Embed only the one this needs:

```bash
go build -tags 'grammar_subset,grammar_subset_starlark' ./cmd/starlsp
```

31.6 MB → **12.9 MB**, no behaviour change. CI runs the test suite under both.

## Status

v0.3.0. The `Host` interface is settled enough to build against; it may still
change before 1.0. Dialect hosts for Buck, Isopod, Copybara, or anything else are
very welcome — they are a table and two methods.

## License

Apache 2.0.
