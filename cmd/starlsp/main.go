// Command starlsp is a language server for Starlark.
//
// Editors start it themselves over stdio. It is rarely run by hand, except
// with --probe, which prints what the server would offer and exits.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/M31-Labs/starlsp"
	"github.com/M31-Labs/starlsp/dialects/bazel"
	"github.com/M31-Labs/starlsp/dialects/tilt"
)

func main() {
	var (
		dialect = flag.String("dialect", "auto", "starlark, bazel, tilt, or auto")
		check   = flag.Bool("check", false, "lint the named files and exit non-zero on any error")
		probe   = flag.Bool("probe", false, "print the server's surface and exit")
		version = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `starlsp — a language server for Starlark.

Usage:
  starlsp                    serve over stdin and stdout
  starlsp --dialect bazel    serve with Bazel's builtins and dialect rules
  starlsp --check FILE...    lint files and exit non-zero on any error
  starlsp --probe            print the names the server offers, then exit
  starlsp --version          print the version

Dialects:
  starlark   the specification only (no host builtins)
  bazel      BUILD, BUILD.bazel, WORKSPACE, MODULE.bazel, .bzl
  tilt       Tiltfile — permits top-level if and for, which Tilt allows
  auto       detect from marker files in the working directory (default)

Auto-detection looks for WORKSPACE, WORKSPACE.bazel, MODULE.bazel, or a .bzl
file (bazel), then Tiltfile (tilt), and falls back to starlark. Editors that
do not start the server in the workspace root should pass --dialect.

Editors launch this command themselves. See the README for VS Code, Neovim,
Helix, and Zed configuration.
`)
	}
	flag.Parse()

	if *version {
		fmt.Println("starlsp", starlsp.Version)
		return
	}

	host, err := hostFor(*dialect)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starlsp:", err)
		os.Exit(2)
	}

	server, err := starlsp.New(starlsp.Options{Host: host})
	if err != nil {
		fmt.Fprintln(os.Stderr, "starlsp:", err)
		os.Exit(1)
	}

	if *probe {
		fmt.Print(server.Probe())
		return
	}

	if *check {
		os.Exit(runCheck(server, flag.Args()))
	}

	if err := server.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "starlsp:", err)
		os.Exit(1)
	}
}

// hostFor builds the host for a dialect name.
//
// Every dialect composes over the specification host, so `len` and
// `"".upper()` work everywhere and a dialect only adds.
func hostFor(name string) (starlsp.Host, error) {
	if name == "auto" {
		name = detectDialect(".")
	}
	switch strings.ToLower(name) {
	case "starlark", "spec", "":
		return starlsp.NewVanilla(), nil
	case "bazel", "bzl", "buck":
		return starlsp.Hosts(starlsp.NewVanilla(), bazel.New()), nil
	case "tilt", "tiltfile":
		return starlsp.Hosts(starlsp.NewVanilla(), tilt.New()), nil
	default:
		return nil, fmt.Errorf("unknown dialect %q: want starlark, bazel, tilt, or auto", name)
	}
}

// detectDialect guesses from marker files in dir.
//
// Bazel is checked first because a repository that has both a WORKSPACE and a
// Tiltfile is a Bazel repository that also uses Tilt, and its .bzl files
// outnumber its Tiltfiles.
func detectDialect(dir string) string {
	for _, marker := range []string{"WORKSPACE", "WORKSPACE.bazel", "MODULE.bazel", "BUILD.bazel"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return "bazel"
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.bzl")); len(matches) > 0 {
		return "bazel"
	}
	if _, err := os.Stat(filepath.Join(dir, "Tiltfile")); err == nil {
		return "tilt"
	}
	return "starlark"
}

// runCheck lints files and returns a process exit code.
//
// This is the server used as a linter. It reports exactly what an editor would
// show, which means a file that is clean here is clean in the editor, and a CI
// gate built on it cannot drift from what a developer sees.
func runCheck(server *starlsp.Server, paths []string) int {
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "starlsp --check: no files given")
		return 2
	}
	errors := 0
	for _, path := range paths {
		diags, err := server.DiagnoseFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "starlsp: %v\n", err)
			errors++
			continue
		}
		for _, d := range diags {
			severity := "warning"
			if d.Severity == starlsp.SeverityError {
				severity = "error"
				errors++
			} else if d.Severity == starlsp.SeverityHint {
				severity = "hint"
			}
			fmt.Printf("%s:%d:%d: %s: %s\n",
				path, d.Range.Start.Line+1, d.Range.Start.Character+1, severity, d.Message)
		}
	}
	if errors > 0 {
		return 1
	}
	return 0
}
