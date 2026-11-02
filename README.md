# starlsp

starlsp is a language server for [Starlark](https://github.com/bazelbuild/starlark),
the Python-like language used by Bazel, Tilt, Starkite and other tools. It gives
an editor diagnostics, completion, hover, go to definition, rename and the rest of
the Language Server Protocol (LSP) features listed below. It is written in pure Go.
There is no cgo and no C toolchain, so it builds and cross-compiles with `go build`.

Starlark is not one language. Each tool adds its own builtins, its own meaning for
`load()`, and its own rules about what a valid file is. starlsp knows the Starlark
specification and ships dialects for Bazel and Tilt. A dialect for any other tool
plugs in through one small Go interface, described under
[Write a host](#write-a-host-for-your-own-dialect).

Current version: 0.4.0. See [CHANGELOG.md](CHANGELOG.md) for what changed.

## Install the server

You need Go 1.25 or newer.

```bash
go install github.com/M31-Labs/starlsp/cmd/starlsp@latest
starlsp --version     # starlsp 0.4.0
```

`go install` puts `starlsp` in `$(go env GOPATH)/bin`. Make sure that directory is
on your `PATH`.

Without a Go toolchain, download an archive from the
[releases page](https://github.com/M31-Labs/starlsp/releases). They are built for
Linux, macOS, Windows and FreeBSD on amd64 and arm64.

To see what a configured server offers, without an editor:

```bash
starlsp --dialect bazel --probe
```

### A smaller binary

The parser library embeds more than 200 grammars. This server needs one. A build
tag keeps only that one:

```bash
go build -tags 'grammar_subset,grammar_subset_starlark' ./cmd/starlsp
```

| Build | Size (linux/amd64, Go 1.26.0, v0.4.0) |
|---|--:|
| default | 33.5 MB |
| `grammar_subset` tags | 14.0 MB |
| `grammar_subset` tags, `-ldflags '-s -w'` (what the release archives use) | 9.6 MB |

The behaviour is the same. CI runs the tests under both.

## Set up your editor

### VS Code

The extension in [`editors/vscode`](editors/vscode) is not on the Marketplace yet.
From a checkout of this repository:

```bash
make install     # go install the server, build the extension, install it into VS Code
```

`make install` needs Go, Node.js and npm, the `code` command on your `PATH`, and
network access for `npm install`. It checks afterwards that VS Code lists the
extension. `make help` lists the other targets.

The extension handles `.star`, `.bzl`, `.sky`, `BUILD`, `BUILD.bazel`, `WORKSPACE`,
`WORKSPACE.bazel`, `MODULE.bazel` and `Tiltfile`. It has three settings:

| Setting | Default | Meaning |
|---|---|---|
| `starlsp.serverPath` | `starlsp` | Path to the server binary. |
| `starlsp.dialect` | `auto` | `starlark`, `bazel`, `tilt` or `auto`. |
| `starlsp.trace.server` | `off` | Log the traffic between editor and server. |

Setting the environment variable `STARLSP_PATH` overrides `starlsp.serverPath`.

On WSL, `code` is a shim onto the Windows build, and installing a `.vsix` over the
`\\wsl.localhost` share fails now and then. The install script stages the archive
on Windows storage and retries.

### Any other editor

starlsp is a standard LSP server. Point your editor's LSP client at it:

| Setting | Value |
|---|---|
| Command | `starlsp`, optionally with `--dialect bazel` or `--dialect tilt` |
| Transport | stdio |
| Language id | `starlark` |
| Files | `*.star`, `*.bzl`, `*.sky`, `BUILD`, `BUILD.bazel`, `WORKSPACE`, `WORKSPACE.bazel`, `MODULE.bazel`, `Tiltfile` |
| Root markers | `WORKSPACE`, `MODULE.bazel`, `Tiltfile`, `.git` |
| Document sync | incremental |

If your editor does not start the server in the workspace root, pass `--dialect`
yourself. The default, `auto`, looks at the server's working directory.

These examples are starting points. This repository tests the protocol, not each
editor.

**Neovim 0.11 or newer**

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

**Helix** (`languages.toml`)

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

**Zed** is not covered. This repository has no Zed extension, and starlsp has not
been tried in Zed. Use the table above if you want to try it.

## What the server does

| Feature | LSP request |
|---|---|
| Diagnostics | `textDocument/publishDiagnostics` (pushed on open, change and save) |
| Completion | `textDocument/completion`, triggered by `.`, `"` and `'` |
| Hover | `textDocument/hover` |
| Signature help | `textDocument/signatureHelp`, triggered by `(` and `,` |
| Go to definition | `textDocument/definition`, including across `load()` when the dialect can resolve it |
| References | `textDocument/references` |
| Highlight uses of a name | `textDocument/documentHighlight` |
| Rename within the file | `textDocument/prepareRename`, `textDocument/rename` |
| Outline and folding | `textDocument/documentSymbol`, `textDocument/foldingRange` |
| Workspace symbols | `workspace/symbol` |
| Expand selection | `textDocument/selectionRange` |
| Links on `load()` targets | `textDocument/documentLink` |
| Syntax colouring | `textDocument/semanticTokens/full` |

Not provided: formatting, code actions, rename across files, and ranged or delta
semantic tokens. The server re-parses the whole buffer on every edit.

## Dialects

| Dialect | `--dialect` | Files | What it adds |
|---|---|---|---|
| Specification | `starlark` | anything | Starlark's own builtins and methods, read from `go.starlark.net`. No top-level `if`, `for` or `while`. No `set`. |
| Bazel | `bazel` | `BUILD`, `BUILD.bazel`, `WORKSPACE`, `MODULE.bazel`, `.bzl` | Bazel's builtins, from a hand-written table. Accepts `set`. |
| Tilt | `tilt` | `Tiltfile` | Tilt's builtins, from a hand-written table. Allows top-level `if`, `for` and `while`, as Tilt does. Accepts `set`. |
| Starkite | not in this repository | `.star` | See below. |

`--dialect auto`, the default, picks Bazel when the working directory has
`WORKSPACE`, `WORKSPACE.bazel`, `MODULE.bazel`, `BUILD.bazel` or a `.bzl` file,
then Tilt when it has a `Tiltfile`, and the specification otherwise.

The Bazel and Tilt tables are written by hand because neither tool is a Go library
that starlsp could ask. They will fall behind the tools. A name missing from a
table is reported as `undefined`. Adding it means appending a line to
`dialects/bazel/bazel.go` or `dialects/tilt/tilt.go`.

**Starkite** uses starlsp as a library. Its `kitelsp` binary, and `kite lsp`,
compose the specification host with a host built from Starkite's module registry.
That host is in the [Starkite repository](https://github.com/project-starkite/starkite)
(`libkite/lsp`), not here.

**`set` and other dialect switches.** The Starlark `set` builtin exists only in
dialects that enable it. Under the specification dialect, and under any host that
sets no options, a call to `set` is reported with go.starlark.net's message, "this
Starlark dialect does not support sets", at its first use in a file. The server
then does not offer `set` in completion or hover either, and `--probe` lists it on
a `switched off` line. A host that enables sets does so through `DialectOptions`
below. The Bazel and Tilt hosts do. Whether a file may call `set()` depends on the
Bazel or Tilt release, which a language server cannot see, and an error on a file
the tool accepts is worse than an error the server misses.

## Use it as a linter

`--check` is the server without the protocol. It runs the same parser, resolver
and dialect rules that an editor would use, so a file that passes here shows no
errors in the editor.

```console
$ cat Tiltfile
load("ext.star", "x")

mode = "dev"

if mode == "dev":
    docker_build("app", ".")

$ starlsp --dialect tilt --check Tiltfile
$ echo $?
0

$ starlsp --dialect starlark --check Tiltfile
Tiltfile:5:1: error: if statement not within a function
Tiltfile:6:5: error: undefined: docker_build
$ echo $?
1
```

It exits 1 when any file has an error or cannot be read, 2 when you pass no files,
and 0 otherwise.
In Go, `Server.Diagnose(uri, text)` and `Server.DiagnoseFile(path)` do the same
with no client and no transport.

## Write a host for your own dialect

A host tells the server what your dialect adds. Two methods are required:

```go
type greeter struct{}

func (greeter) Name() string { return "greeter" }

func (greeter) Globals() []starlsp.Symbol {
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
		// Compose over NewVanilla so the dialect adds to Starlark's names.
		Host: starlsp.Hosts(starlsp.NewVanilla(), greeter{}),
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(srv.Run()) // serves LSP on stdin and stdout
}
```

That alone gives `greet` in completion, hover with its signature, signature help,
the right colour, and no false "undefined: greet". A name your host does not define
is reported as `undefined`. [`example_test.go`](example_test.go) holds this example
as runnable code, and `go test` checks its output.

Everything else is an optional interface. The server finds it with a type
assertion, so a host implements only what it has.

| Optional interface | What it adds |
|---|---|
| `MemberProvider` | Completion after a dot: `fs.` offers the members of `fs`. |
| `TypeResolver` | The type of an expression, for `p = fs.path(...)` then `p.`. |
| `LoadResolver` | Go to definition across `load()`, and document links. |
| `Linter` | Diagnostics that belong to the dialect, not to the language. |
| `Documenter` | Signatures and prose that live outside the values. |
| `DialectOptions` | The `syntax.FileOptions` the parser and resolver enforce: top-level control flow, `while`, `set`, global reassignment. |
| `UniverseRemover` | Names in Starlark's universe that the dialect does not have, so the server reports them as `undefined`. |

`Hosts(a, b, ...)` combines hosts. The later host wins a name collision, and
`UniverseRemover` removes the union of what the members remove.

Three rules for host authors:

- **A host is called from several goroutines at once.** Read-only requests run
  concurrently, and the analysis after an edit runs beside them. Build your tables
  once, behind a `sync.Once`, and only read them afterwards.
- **`Globals` is read once.** The server keeps the first answer for its lifetime,
  so completion, hover and resolution agree with each other. Start a new server if
  your names change.
- **A `*Document` never changes.** Each edit makes a new one, so a host may keep a
  `*Document` it was handed for as long as it likes. `Text()` returns a slice that
  must not be modified.

Where you can, read your dialect's surface from the runtime instead of writing a
table. The specification host does: it reads globals from `starlark.Universe` and
methods by calling `AttrNames` on a zero `String`, `List`, `Dict`, `Set` and
`Bytes`. A release of `go.starlark.net` that adds a method then gains completion
for it with no change here.

## Run the checks

```bash
make test             # go test ./...
make race             # go test -race ./...
make corpus           # clone three real Starlark repositories and run the corpus tests
make test-extension   # build the server and the extension, run the extension's suites
make release          # all of the above on a clean tree, then print the tag command
```

`make race` includes `TestInterleavedEditsAndRequestsDoNotRace`. It streams edits,
some of which break the syntax, into six documents, and sends 2,526 hover,
completion, definition, outline, token and other requests beside them. The server
runs up to 16 requests at a time. Run it before you change anything in
`document.go` or `dispatch.go`.

`make corpus` clones `bazelbuild/rules_go`, `bazelbuild/bazel-skylib` and
`tilt-dev/tilt` into `.corpus`, which needs network access. You can point the
corpus tests at any directory of Starlark instead:

```bash
GTS_STARLARK_CORPUS=/path/to/starlark go test -run 'TestCorpus|TestRealWorld' ./...
```

The test suite runs under both builds:

```bash
go test ./...
go test -tags 'grammar_subset,grammar_subset_starlark' ./...
```

To run the extension's suites without packaging it (packaging needs network):

```bash
go build -o /tmp/starlsp ./cmd/starlsp
cd editors/vscode
npm install
npm run build:prod
STARLSP_PATH=/tmp/starlsp npm test
```

`make version` prints the version. The Go constant in `server.go`, the extension's
`package.json` and the git tag must agree, and `TestExtensionVersionMatchesTheServer`
checks the first two.

## How it works

Two parsers do different jobs.

- **gotreesitter** answers structural questions on every keystroke: outline,
  folding, colours, and where the cursor sits for completion. It copes with a
  half-typed buffer, which is when an editor is most useful.
- **go.starlark.net** answers every question about correctness. It is the parser
  and resolver a Starlark host runs, so a diagnostic in the editor is one the host
  would report. The server never reimplements Starlark's scope rules.

Each version of a buffer is an immutable `Document`: text, line index and parse
tree. Read-only requests run on their own goroutines. A request takes one
`Document` when it starts and reads only that. An edit builds the next `Document`
and swaps it in, and does not touch the old one. That is why an edit that arrives
while a request is running cannot change what the request sees. starlsp does not
call `Release` on the tree of any `Document` that a request can reach, so a later
parse cannot reuse memory that an earlier tree, or a running request, still reads.
The garbage collector frees a tree when nothing refers to it. The one exception
is the scratch `Document` that `workspace/symbol` builds for each file that is not
open: no one else can see it, and its tree goes back to the library before the
next file. See the comment on `Document` in `document.go`.

With gotreesitter v0.50.1, an open script of a few kilobytes holds about 3 MB, and
one of 52 KB about 10 MB. Expect several megabytes per open file.

When the buffer cannot be parsed, for example just after you type the dot in
`x = foo.`, the outline and folding use the last version that had structure. Colours
do not, because stale ranges would colour the wrong characters. The same holds if
the tree-sitter parser itself fails, by returning an error or by panicking: the
server logs it, keeps tracking the buffer's text, and starts the next parse with a
fresh parser. Diagnostics come from go.starlark.net and do not depend on it.

## Status

Version 0.4.0. The `Host` interface may still change before 1.0. Hosts for Buck,
Isopod, Copybara or any other tool are welcome. A host is a table and two methods.

[`tilt-dev/starlark-lsp`](https://github.com/tilt-dev/starlark-lsp) is an earlier
Starlark language server.

## License

Apache 2.0.
