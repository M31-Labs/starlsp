package starlsp_test

import (
	"fmt"
	"log"

	"github.com/M31-Labs/starlsp"
)

// greeter is a dialect with one builtin, greet. A host needs two methods: Name
// and Globals. Everything else is an optional interface.
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

// Compose a host over NewVanilla so the dialect adds to Starlark's own names
// instead of replacing them. A name the host does not define is reported the way
// the runtime would report it.
func ExampleHosts() {
	srv, err := starlsp.New(starlsp.Options{
		Host: starlsp.Hosts(starlsp.NewVanilla(), greeter{}),
	})
	if err != nil {
		log.Fatal(err)
	}

	source := "greet(\"world\")\nshout(\"hi\")\n"
	for _, d := range srv.Diagnose("/tmp/hello.star", source) {
		fmt.Printf("line %d: %s\n", d.Range.Start.Line+1, d.Message)
	}
	// Output:
	// line 2: undefined: shout
}

// noPrint is a dialect whose runtime has no print. It says so by implementing
// UniverseRemover, and the server then reports print the way that runtime does.
type noPrint struct{ greeter }

func (noPrint) RemovedUniverse() []string { return []string{"print"} }

func ExampleUniverseRemover() {
	srv, err := starlsp.New(starlsp.Options{
		Host: starlsp.Hosts(starlsp.NewVanilla(), noPrint{}),
	})
	if err != nil {
		log.Fatal(err)
	}

	source := "print(len([1]))\n"
	for _, d := range srv.Diagnose("/tmp/hello.star", source) {
		fmt.Printf("line %d: %s\n", d.Range.Start.Line+1, d.Message)
	}
	// Output:
	// line 1: undefined: print
}
