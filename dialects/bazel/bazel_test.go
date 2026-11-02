package bazel_test

import (
	"strings"
	"testing"

	"github.com/M31-Labs/starlsp"
	"github.com/M31-Labs/starlsp/dialects/bazel"
)

func TestBazelSurfaceCoversWhatRealCodeUses(t *testing.T) {
	h := bazel.New()
	got := map[string]starlsp.Symbol{}
	for _, s := range h.Globals() {
		got[s.Name] = s
	}

	// Every name below was observed in bazelbuild/rules_go or
	// bazelbuild/bazel-skylib. Losing one reintroduces a false "undefined".
	for _, want := range []string{
		"rule", "aspect", "provider", "depset", "select", "struct", "Label",
		"glob", "native", "attr", "package", "licenses", "exports_files",
		"filegroup", "genrule", "alias", "test_suite", "config_setting",
		"repository_rule", "module_extension", "tag_class", "transition",
		"exec_group", "local_repository", "label_flag", "config",
		"DefaultInfo", "OutputGroupInfo", "AnalysisFailureInfo",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("bazel host is missing %q", want)
		}
	}
	if got["attr"].Kind != starlsp.KindNamespace {
		t.Errorf("attr kind = %v, want KindNamespace", got["attr"].Kind)
	}
	if got["DefaultInfo"].Kind != starlsp.KindType {
		t.Errorf("DefaultInfo kind = %v, want KindType", got["DefaultInfo"].Kind)
	}
}

func TestAttrMembersComplete(t *testing.T) {
	// `attr.` is referenced more than any other name in real rule code.
	members := map[string]bool{}
	for _, s := range bazel.New().Members("attr") {
		members[s.Name] = true
		if s.Signature == "" {
			t.Errorf("attr.%s has no signature", s.Name)
		}
	}
	for _, want := range []string{"string", "label", "label_list", "string_list", "bool", "int", "output"} {
		if !members[want] {
			t.Errorf("attr is missing %q", want)
		}
	}
}

func TestBazelForbidsTopLevelControl(t *testing.T) {
	if bazel.New().FileOptions().TopLevelControl {
		t.Error("Bazel does not permit top-level control flow; the dialect says it does")
	}
}

// A language server can tell which Bazel a file will run under no better than a
// guess, and Bazel's Starlark has gained a set type in recent releases. The host
// accepts set(), because an error on a file that its own runtime accepts is the
// worst thing a language server can say. The cost is a missed error under a
// Bazel that lacks sets, which is what this server did before it checked.
func TestBazelDoesNotReportSet(t *testing.T) {
	if !bazel.New().FileOptions().Set {
		t.Error("the Bazel dialect switches sets off, so every use of set() is reported")
	}

	srv, err := starlsp.New(starlsp.Options{Host: starlsp.Hosts(starlsp.NewVanilla(), bazel.New())})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, d := range srv.Diagnose("/tmp/starlsp-test/sets.bzl", "def _impl(ctx):\n    return set(ctx.attr.srcs)\n") {
		t.Errorf("set() in a .bzl file got a diagnostic: %s", d.Message)
	}
	if !strings.Contains(srv.Probe(), " set ") {
		t.Errorf("the probe no longer lists set:\n%s", srv.Probe())
	}
}
