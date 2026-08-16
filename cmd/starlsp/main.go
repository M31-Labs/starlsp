// Command starlsp is a language server for Starlark.
//
// Editors start it themselves over stdio. It is rarely run by hand, except
// with --probe, which prints what the server would offer and exits.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/M31-Labs/starlsp"
)

func main() {
	var (
		probe   = flag.Bool("probe", false, "print the server's surface and exit")
		version = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `starlsp — a language server for Starlark.

Usage:
  starlsp             serve over stdin and stdout
  starlsp --probe     print the names the server offers, then exit
  starlsp --version   print the version

Editors launch this command themselves. See the README for VS Code, Neovim,
Helix, and Zed configuration.
`)
	}
	flag.Parse()

	if *version {
		fmt.Println("starlsp", starlsp.Version)
		return
	}

	server, err := starlsp.New(starlsp.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "starlsp:", err)
		os.Exit(1)
	}

	if *probe {
		fmt.Print(server.Probe())
		return
	}

	if err := server.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "starlsp:", err)
		os.Exit(1)
	}
}
