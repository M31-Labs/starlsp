package starlsp

// Resolution has two kinds of known name, and a dialect can lack either.
//
//   - Predeclared names are the ones a host adds. Hosts list them in Globals.
//   - Universe names are the ones Starlark itself defines: len, print, set, and
//     so on. The resolver treats them differently from predeclared names, and
//     only on that path does it enforce the dialect's switches. Reporting a
//     universe name as predeclared, which the server used to do, skipped the
//     check, so a dialect that lacks the name got no diagnostic for it.
//
// These tests use the exported behaviour: what diagnostics a buffer gets.

import (
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"go.starlark.net/resolve"
	"go.starlark.net/syntax"
)

// optionsHost is a dialect that only sets file options.
type optionsHost struct{ opts syntax.FileOptions }

func (*optionsHost) Name() string                       { return "options" }
func (*optionsHost) Globals() []Symbol                  { return nil }
func (h *optionsHost) FileOptions() *syntax.FileOptions { o := h.opts; return &o }

// trimmedHost is a dialect whose universe lacks some of Starlark's names.
type trimmedHost struct {
	optionsHost
	removed []string
}

func (*trimmedHost) Name() string                { return "trimmed" }
func (h *trimmedHost) RemovedUniverse() []string { return h.removed }

var (
	_ DialectOptions  = (*optionsHost)(nil)
	_ UniverseRemover = (*trimmedHost)(nil)
)

func diagnose(t *testing.T, host Host, src string) []Diagnostic {
	t.Helper()
	srv, err := New(Options{Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Diagnose("file:///tmp/starlsp-test/universe.star", src)
}

func messages(diags []Diagnostic) string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Message)
	}
	return strings.Join(out, " | ")
}

func TestVanillaKeepsEveryNameTheUniverseDefines(t *testing.T) {
	// The server must not hard-code a list of names it dislikes. Whatever
	// go.starlark.net defines stays on offer, including set.
	names := map[string]Symbol{}
	for _, sym := range NewVanilla().Globals() {
		names[sym.Name] = sym
	}
	set, ok := names["set"]
	if !ok {
		t.Fatal("the specification host lost the set builtin")
	}
	if set.Signature == "" {
		t.Error("set has no signature")
	}
	if len(NewVanilla().Members("set")) == 0 {
		t.Error("set has no methods to complete")
	}

	// The host's list keeps set. Whether the server offers it depends on the
	// dialect; see TestSetIsOfferedOnlyWhereTheDialectAcceptsIt.
	srv, err := New(Options{Host: Hosts(NewVanilla(), &optionsHost{opts: syntax.FileOptions{Set: true}})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := srv.docs.snapshot("file:///tmp/starlsp-test/offer.star", 1, []byte("x = 1\n"))
	if _, ok := labels(srv.complete(d, Position{Line: 0, Character: 0}).Items)["set"]; !ok {
		t.Error("completion no longer offers set in a dialect that has sets")
	}
}

func TestSetIsRefusedUnlessTheDialectTurnsItOn(t *testing.T) {
	src := "def f(xs):\n    return set(xs)\n"

	t.Run("specification dialect", func(t *testing.T) {
		got := messages(diagnose(t, NewVanilla(), src))
		if !strings.Contains(got, "does not support sets") {
			t.Errorf("set under the specification dialect: got %q, want the runtime's own message", got)
		}
		if strings.Contains(got, "undefined") {
			t.Errorf("set is part of the universe, not undefined: %q", got)
		}
	})
	t.Run("a host composed over the specification host", func(t *testing.T) {
		got := messages(diagnose(t, Hosts(NewVanilla(), &optionsHost{}), src))
		if !strings.Contains(got, "does not support sets") {
			t.Errorf("composing over the specification host hid the check: %q", got)
		}
	})
	t.Run("a dialect that turns it on", func(t *testing.T) {
		got := diagnose(t, Hosts(NewVanilla(), &optionsHost{opts: syntax.FileOptions{Set: true}}), src)
		if len(got) != 0 {
			t.Errorf("set is enabled but the file got diagnostics: %q", messages(got))
		}
	})
	t.Run("a dialect without the universe composed in", func(t *testing.T) {
		// A host need not compose over NewVanilla. The universe is still the universe.
		got := messages(diagnose(t, &optionsHost{}, "x = len([1])\n"))
		if got != "" {
			t.Errorf("len is a universe name but was reported: %q", got)
		}
	})
}

func TestAHostCanRemoveANameFromTheUniverse(t *testing.T) {
	host := Hosts(NewVanilla(), &trimmedHost{removed: []string{"set", "print"}})

	got := diagnose(t, host, "x = set([1])\nprint(x)\ny = len([1])\n")
	text := messages(got)
	for _, want := range []string{"undefined: set", "undefined: print"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "undefined: len") {
		t.Errorf("len was not removed but was reported: %q", text)
	}

	// Removal wins over a dialect that turns the builtin on: the name is not
	// switched off, it is not there.
	on := Hosts(NewVanilla(), &trimmedHost{optionsHost: optionsHost{opts: syntax.FileOptions{Set: true}}, removed: []string{"set"}})
	if text := messages(diagnose(t, on, "x = set([1])\n")); !strings.Contains(text, "undefined: set") {
		t.Errorf("a removed name was accepted because the options enable it: %q", text)
	}

	t.Run("the surface follows", func(t *testing.T) {
		srv, err := New(Options{Host: host})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		d := srv.docs.snapshot("file:///tmp/starlsp-test/surface.star", 1, []byte("x = 1\n"))
		offered := labels(srv.complete(d, Position{}).Items)
		for _, name := range []string{"set", "print"} {
			if _, ok := offered[name]; ok {
				t.Errorf("completion still offers the removed name %q", name)
			}
		}
		if _, ok := offered["len"]; !ok {
			t.Error("completion lost len, which was not removed")
		}
		if probe := srv.Probe(); !strings.Contains(probe, "removed        print set") {
			t.Errorf("the probe does not say what was removed:\n%s", probe)
		}

		// Hover on a removed name finds no documentation.
		h := srv.docs.snapshot("file:///tmp/starlsp-test/hover.star", 1, []byte("x = set([1])\n"))
		if got := srv.hover(h, Position{Line: 0, Character: 5}); got != nil {
			t.Errorf("hover documents a removed name: %+v", got)
		}
	})

	t.Run("only universe names can be removed", func(t *testing.T) {
		odd := Hosts(NewVanilla(), &trimmedHost{removed: []string{"def", "shout", "nonsense"}})
		srv, err := New(Options{Host: odd})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if len(srv.removed) != 0 {
			t.Errorf("names outside the universe were accepted as removals: %v", srv.removed)
		}
		if text := messages(diagnose(t, odd, "def f():\n    return 1\n")); text != "" {
			t.Errorf("a keyword was treated as removed: %q", text)
		}
	})
}

func TestComposedHostsRemoveTheUnionOfTheirMembers(t *testing.T) {
	h := Hosts(
		&trimmedHost{removed: []string{"set"}},
		NewVanilla(),
		&trimmedHost{removed: []string{"print", "set"}},
	)
	got := h.(UniverseRemover).RemovedUniverse()
	if strings.Join(got, ",") != "print,set" {
		t.Errorf("RemovedUniverse = %v, want [print set]", got)
	}
}

func TestTheUniverseIsResolvedAsTheUniverse(t *testing.T) {
	// The white-box form of the fix. If len were reported as predeclared again,
	// the resolver would never ask whether the dialect supports it.
	srv, err := New(Options{Host: Hosts(NewVanilla(), &fakeHost{})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := srv.docs.snapshot("file:///tmp/starlsp-test/scope.star", 1, []byte("n = len([1])\nshout(n)\n"))
	a := srv.analyze(d)
	if a.File == nil {
		t.Fatal("the file did not parse")
	}

	scopes := map[string]resolve.Scope{}
	syntax.Walk(a.File, func(n syntax.Node) bool {
		if id, ok := n.(*syntax.Ident); ok {
			if b, ok := id.Binding.(*resolve.Binding); ok && b != nil {
				scopes[id.Name] = b.Scope
			}
		}
		return true
	})
	if scopes["len"] != resolve.Universal {
		t.Errorf("len resolved as %v, want the universe", scopes["len"])
	}
	if scopes["shout"] != resolve.Predeclared {
		t.Errorf("shout, which only the host defines, resolved as %v, want predeclared", scopes["shout"])
	}
}

// ---------- what the server offers follows what it accepts ----------

// catalogHost lists every universe name as one of its own globals, with its own
// documentation. A catalog built from a runtime's predeclared names and its
// universe does exactly this, and Starkite's does.
type catalogHost struct{}

func (catalogHost) Name() string { return "catalog" }

func (catalogHost) Globals() []Symbol {
	var out []Symbol
	for name := range universeNames {
		out = append(out, Symbol{Name: name, Kind: KindFunction, Doc: "documented by the catalog"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// removingCatalog is a catalog whose dialect lacks some universe names.
type removingCatalog struct {
	catalogHost
	removed []string
}

func (h removingCatalog) RemovedUniverse() []string { return h.removed }

// probeLists reports whether the probe names something as part of what the
// server offers. The lines that explain what is missing do not count.
func probeLists(probe, name string) bool {
	for _, line := range strings.Split(probe, "\n") {
		if strings.HasPrefix(line, "switched off") || strings.HasPrefix(line, "removed") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if field == name {
				return true
			}
		}
	}
	return false
}

// TestSetIsOfferedOnlyWhereTheDialectAcceptsIt: a server that offers a name in
// completion and then reports the name as an error in the same buffer is
// contradicting itself.
func TestSetIsOfferedOnlyWhereTheDialectAcceptsIt(t *testing.T) {
	src := []byte("x = set([1])\n")

	cases := []struct {
		name     string
		host     Host
		offered  bool // does this dialect have sets
		explains bool // does the probe say that set was left out
	}{
		{"the specification host", NewVanilla(), false, true},
		{"a host that sets nothing", Hosts(NewVanilla(), &optionsHost{}), false, true},
		{"a catalog that lists the universe itself", Hosts(NewVanilla(), catalogHost{}), false, true},
		{"a host that never listed set", &optionsHost{}, false, false},
		{"a dialect with sets", Hosts(NewVanilla(), &optionsHost{opts: syntax.FileOptions{Set: true}}), true, false},
		{"a catalog in a dialect with sets", Hosts(NewVanilla(), catalogHost{}, &optionsHost{opts: syntax.FileOptions{Set: true}}), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Options{Host: tc.host})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			d := srv.docs.snapshot("file:///tmp/starlsp-test/offer-set.star", 1, src)
			_, offered := labels(srv.complete(d, Position{Line: 0, Character: 0}).Items)["set"]
			hovered := srv.hover(d, Position{Line: 0, Character: 5}) != nil
			probe := srv.Probe()
			listed := probeLists(probe, "set")
			rejected := strings.Contains(messages(srv.Diagnose("file:///tmp/starlsp-test/offer-set.star", string(src))), "does not support sets")

			if offered != tc.offered {
				t.Errorf("completion offers set = %v, want %v", offered, tc.offered)
			}
			if hovered != tc.offered {
				t.Errorf("hover documents set = %v, want %v", hovered, tc.offered)
			}
			if listed != tc.offered {
				t.Errorf("the probe lists set = %v, want %v:\n%s", listed, tc.offered, probe)
			}
			if rejected == tc.offered {
				t.Errorf("diagnostics reject set = %v while completion offers it = %v", rejected, offered)
			}
			if says := strings.Contains(probe, "switched off   set"); says != tc.explains {
				t.Errorf("the probe says set is switched off = %v, want %v:\n%s", says, tc.explains, probe)
			}
		})
	}

	// The host's own list is not edited: the server decides what to offer.
	found := false
	for _, sym := range NewVanilla().Globals() {
		found = found || sym.Name == "set"
	}
	if !found {
		t.Error("the specification host no longer lists set")
	}
}

// TestRemovalIsByName pins what UniverseRemover means. A host that removes a
// name gets that name gone, whatever else it lists under it. The server cannot
// tell a replacement from a copy of the universe's own entry: a catalog lists
// the universe's names as its own, so the entry under a removed name is
// usually just that copy. A dialect that replaces a universe function lists the
// replacement and does not remove it.
func TestRemovalIsByName(t *testing.T) {
	host := Hosts(NewVanilla(), removingCatalog{removed: []string{"set", "print"}})

	got := messages(diagnose(t, host, "x = set([1])\nprint(x)\ny = len([1])\n"))
	for _, want := range []string{"undefined: set", "undefined: print"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "undefined: len") {
		t.Errorf("len was not removed but was reported: %q", got)
	}

	srv, err := New(Options{Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := srv.docs.snapshot("file:///tmp/starlsp-test/removal.star", 1, []byte("x = 1\n"))
	offered := labels(srv.complete(d, Position{}).Items)
	for _, name := range []string{"set", "print"} {
		if _, ok := offered[name]; ok {
			t.Errorf("the catalog's own entry for the removed name %q is still offered", name)
		}
	}
	if item, ok := offered["len"]; !ok || item.Documentation == nil {
		t.Error("len, which was not removed, lost the catalog's entry")
	}

	// And replacing without removing works: the replacement is the entry that
	// completion offers, and uses of the name are not errors.
	replaced := Hosts(NewVanilla(), catalogHost{})
	srv, err = New(Options{Host: replaced})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if item := labels(srv.complete(d, Position{}).Items)["print"]; item.Documentation == nil || item.Documentation.Value != "documented by the catalog" {
		t.Errorf("a host's replacement for print is not what completion offers: %+v", item)
	}
	if got := messages(diagnose(t, replaced, "print(1)\n")); got != "" {
		t.Errorf("print is replaced, not removed, but was reported: %q", got)
	}
}

// countingHost counts how often the server asks for its globals.
type countingHost struct {
	calls atomic.Int32
}

func (h *countingHost) Name() string { return "counting" }
func (h *countingHost) Globals() []Symbol {
	h.calls.Add(1)
	return []Symbol{{Name: "shout", Kind: KindFunction}}
}

// TestTheHostIsAskedForItsGlobalsOnce pins the documented contract of
// Host.Globals: the server reads the list once and keeps it. Completion, hover,
// resolution and the probe then all see the same names, which they could not if
// each asked again and the host answered differently.
func TestTheHostIsAskedForItsGlobalsOnce(t *testing.T) {
	host := &countingHost{}
	srv, err := New(Options{Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := srv.docs.open("file:///tmp/starlsp-test/once.star", 1, "shout(1)\nx = 1\n")
	for i := 0; i < 5; i++ {
		srv.complete(d, Position{Line: 1, Character: 0})
		srv.hover(d, Position{Line: 0, Character: 2})
		srv.semanticTokensFor(d)
		srv.diagnosticsFor(d)
		srv.Probe()
	}
	if n := host.calls.Load(); n != 1 {
		t.Errorf("Host.Globals was called %d times, want once", n)
	}
}
