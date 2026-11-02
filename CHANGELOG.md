# Changelog

All notable changes to starlsp are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor version can change
the `Host` interface.

## [0.4.0] - 2026-11-01

This release fixes data races between edits and requests, and makes the server
report a call to `set` when the dialect does not have it. If you embed starlsp or
run it under `kitelsp`, move off v0.1.1 to this release, not to v0.3.0: v0.3.0
has the races. gotreesitter stays at v0.50.1.

### Fixed

- **Edits no longer race with requests.** The server runs hover, completion and
  other read-only requests on their own goroutines. A `didChange` used to rewrite
  the document's text, line index and parse tree in place while those requests
  were reading them. That is a data race, and with an editor that sends requests
  while you type it can return wrong answers or crash. Each version of a document
  is now an immutable `Document`. An edit builds the next one and swaps it in. A
  request reads one `Document` from start to finish. The new
  `TestInterleavedEditsAndRequestsDoNotRace` reports about 1,000 to 1,200 data
  races per run on v0.3.0 under `go test -race` (979 to 1,197 across our runs),
  and none now.
- **A failed tree-sitter parse no longer costs the buffer its text.** If the
  parser returns an error or panics, the server still publishes the new version,
  with its own text and no tree, and logs the failure. The outline and folding use
  the last version that had a tree, colours are left empty, and diagnostics, which
  come from go.starlark.net, are unaffected. After a panic the next edit gets a
  fresh parser. `Server.Diagnose` and `starlsp --check` used to end the process on
  such a panic and no longer do.
- **A call to `set` is reported when the dialect does not enable it.** The server
  told the resolver that every universe name, `set` included, was a name the host
  had predeclared. The resolver checks dialect switches only on the universe path,
  so `FileOptions.Set` was never enforced and a runtime that rejects `set` got no
  diagnostic from the editor. The universe is now resolved as the universe. Under
  the specification dialect, and under any host that sets no options (Starkite's
  is one), `set(...)` now gets go.starlark.net's message, "this Starlark dialect
  does not support sets". The resolver reports it at the first use in a file, not
  at every use. A dialect that enables sets (`FileOptions{Set: true}`) sees
  nothing. The Bazel and Tilt hosts enable them; see Changed.
- **The server no longer offers `set` where it reports it as an error.**
  Completion, hover and `--probe` leave `set` out when the dialect has sets
  switched off. `--probe` says so on a `switched off` line.
- **The outline no longer shows garbage names while the buffer cannot be parsed.**
  The server keeps the last parse that had structure and builds the outline from
  it while you type something like `x = [`. It then read the names out of the new
  text using the old tree's offsets, so any edit above a function returned the
  wrong characters. The old text now travels with the old tree.
- **Outline and completion are linear in the size of the file.** The outline
  looped over `NamedChildCount()` and `NamedChild(i)`, and gotreesitter keeps no
  index of named children, so each call scans from the start. On a file with
  40,000 top-level statements, v0.3.0 took about 20 s for the outline and about
  19 s for a completion, which builds the outline too. They now take tens of
  milliseconds (`TestOutlineAndCompletionScaleLinearly`; single runs on a loaded
  machine, so treat the numbers as rough).
- **Semantic token columns are in UTF-16 units.** Tree-sitter counts bytes. A
  token after a non-ASCII character on its line was placed too far right, and a
  string or comment containing one was too long. Converting is linear in the line:
  a 208 KB line with 20,000 tokens takes 6 ms.
- **Folding ranges describe one version of the buffer.** Block folds could come
  from the last version that had structure and comment folds from the current
  one.
- **The analysis cache no longer holds documents or stale results.** An entry is
  keyed by the `Document` it was computed from, by sequence number, and keeps no
  tree. A result is stored only for the version the server holds at that moment,
  so a slow request cannot replace a newer entry, and a request that straddles a
  `didClose` cannot keep the closed buffer's tree alive.
- **`workspace/symbol` reuses tree memory.** A scan of files that are not open
  allocated a fresh 3 MB arena for every file. It now hands each file's tree back
  before the next one. For 60 files of 2.5 KB each, it allocates 5.5 MB where it
  allocated 180 MB.
- **`Server.Diagnose` and `DiagnoseFile` no longer open the document.** They used
  to open it in the server's store and close it again, which would have closed a
  buffer an editor had open under the same URI.

### Added

- `UniverseRemover`, an optional host interface for a dialect whose universe lacks
  some of Starlark's names. The server then reports the name as `undefined`,
  and stops offering it in completion and hover. Removal is by name: the server
  drops every `Globals` entry under a removed name, including one the host lists
  itself. `Hosts(a, b)` removes the union of what its members remove.
  `Server.Probe` lists the removed names. The built-in dialects remove none, so
  `starlsp --probe` never shows that line.
- A `Makefile` with `build`, `test`, `race`, `corpus`, `install`, `test-extension`,
  `version` and `release` targets, an install script for the VS Code extension
  that works around the WSL share problem, and a test that fails when the server
  and the VS Code extension disagree about the version.
- Tests: an interleaved edit and request test for the race detector, tests that a
  `Document` is never edited and that a published tree is never rewritten, tests
  for a parser that fails, for the analysis cache, for `set` and `UniverseRemover`,
  for token columns and for memory (an open script must hold under 5 MB, and a
  scan of files on disk must not allocate a new arena per file), and runnable
  examples for `Hosts` and `UniverseRemover`.
- This changelog, and a README written for someone installing starlsp for the
  first time.

### Changed

- gotreesitter stays at v0.50.1. The race fix does not need a newer one, and
  v0.56.0 costs memory. Its Starlark grammar file is byte-identical and its trees
  are identical on the Starkite test scripts, but one parsed script holds about
  7.4 to 7.8 MB where v0.50.1 holds about 2.8 to 3.0 MB, so thirty open files take
  about 235 MB instead of 90 MB. It also gives one valid Bazel file in 1,812 two
  `ERROR` nodes that v0.50.1 does not, and fixes eight files in which v0.50.1 let
  a function's span run past its end. `TestAnOpenDocumentStaysSmall` fails on
  v0.56.0. A later release can move up when that cost is resolved.
- The Bazel and Tilt hosts accept `set`. Whether a file may call `set()` depends
  on the Bazel or Tilt release, which a language server cannot see, and an error on
  a file the tool accepts is worse than an error the server misses. Only the
  specification dialect and hosts that set no options refuse it.
- `Host.Globals` is called once, on first use, and the server keeps the answer.
  Completion, hover, resolution and the probe all read that one list. Before, each
  completion called it again, while hover and resolution already used a list built
  once. The `Host` documentation now says so.
- Since v0.3.0 the server calls hosts from several goroutines at once. The `Host`
  documentation now says so: build tables once, behind a `sync.Once`, and only
  read them afterwards.
- The VS Code extension is version 0.4.0.

## [0.3.0] - 2026-08-16

### Added

- A request dispatcher. A panic in a handler is recovered and answered with an
  error, so one bad file cannot end the session. Read-only requests run
  concurrently, up to 16 at a time. Notifications that change a document run in
  order. `$/cancelRequest` stops a request in flight, including a workspace scan.
- A per-document analysis cache of up to 64 entries. It is cleared when a
  document closes, when the configuration changes, and when a watched file
  changes.
- The outline and folding ranges fall back to the last parse that had structure,
  so they do not empty out while you type a half-written member access.
- Hardening tests: a host that panics, hostile input (nul bytes, invalid UTF-8,
  a 100,000-character line, 500-deep nesting, unterminated strings), a large
  document, and cancellation.

## [0.2.1] - 2026-08-16

### Added

- A VS Code extension in `editors/vscode`. It registers the `starlark` language
  for `.star`, `.bzl`, `.sky`, `BUILD`, `BUILD.bazel`, `WORKSPACE`,
  `WORKSPACE.bazel`, `MODULE.bazel` and `Tiltfile`, starts the server, and has
  the settings `starlsp.serverPath`, `starlsp.dialect` and `starlsp.trace.server`.
  It is bundled with esbuild, and CI tests it with Microsoft's own LSP client
  libraries and by loading the packaged bundle.

## [0.2.0] - 2026-08-16

### Added

- Bazel and Tilt dialect hosts in `dialects/bazel` and `dialects/tilt`.
- `DialectOptions` and `SpecFileOptions`: a host chooses the `syntax.FileOptions`
  the parser and resolver enforce. Tilt allows `if`, `for` and `while` at the top
  level and Bazel does not, so one set of rules cannot fit both.
- Command-line flags: `--dialect` (`starlark`, `bazel`, `tilt` or `auto`) and
  `--check` to lint files and exit non-zero on an error.
- `Server.Diagnose` and `Server.DiagnoseFile`, which analyse text without a
  client or a transport.
- Corpus tests that run the server over real Bazel and Tilt repositories, and
  GoReleaser configuration for prebuilt binaries.

### Changed

- A top-level `if` or `for` is now reported once, by the resolver, as "not within
  a function". The server's own tree-based check, which reported it a second time
  in different words, is removed.

## [0.1.1] - 2026-08-15

### Added

- `Document` accessors for hosts: `URI`, `Path`, `Version`, `Text`, `SpanRange`,
  `PositionOf` and `OffsetOf`.
- Exported diagnostic severities: `SeverityError`, `SeverityWarning`,
  `SeverityInformation` and `SeverityHint`.
- A CI step and README section for the lean build, which embeds only the
  Starlark grammar.

## [0.1.0] - 2026-08-15

First release. A language server for Starlark in pure Go, with no cgo.

### Added

- Two parsers. gotreesitter answers structural questions on every keystroke and
  copes with a half-typed buffer. go.starlark.net answers every question about
  correctness, because it is the parser and resolver a Starlark host runs.
- Diagnostics, completion, hover, signature help, go to definition, find
  references, document highlight, rename with prepare, document symbols,
  workspace symbols, folding ranges, selection ranges, document links and
  semantic tokens, with incremental document sync.
- Command-line flags `--probe`, which prints the names the server offers, and
  `--version`.
- The `Host` interface for dialects. `Name` and `Globals` are required.
  `MemberProvider`, `TypeResolver`, `LoadResolver`, `Linter` and `Documenter` are
  optional. `Hosts(a, b, ...)` composes hosts, and the later host wins a name
  collision.
- A specification host, `NewVanilla`, whose names and methods are read from
  `starlark.Universe` and from the runtime values instead of a hand-written
  table.

[0.4.0]: https://github.com/M31-Labs/starlsp/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/M31-Labs/starlsp/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/M31-Labs/starlsp/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/M31-Labs/starlsp/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/M31-Labs/starlsp/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/M31-Labs/starlsp/releases/tag/v0.1.0
