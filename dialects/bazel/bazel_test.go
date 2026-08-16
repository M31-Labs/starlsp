package bazel_test

import (
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
