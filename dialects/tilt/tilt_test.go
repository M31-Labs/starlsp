package tilt_test

import (
	"testing"

	"github.com/M31-Labs/starlsp"
	"github.com/M31-Labs/starlsp/dialects/tilt"
)

func TestTiltPermitsTopLevelControl(t *testing.T) {
	// This is the whole reason the package exists. Real Tiltfiles use a
	// top-level if; enforcing the specification reports an error on a file
	// Tilt itself accepts.
	opts := tilt.New().FileOptions()
	if !opts.TopLevelControl {
		t.Error("Tilt permits top-level if and for; the dialect forbids it")
	}
	if !opts.GlobalReassign {
		t.Error("Tilt permits reassigning top-level names")
	}
}

func TestTiltSurfaceCoversWhatRealTiltfilesUse(t *testing.T) {
	got := map[string]starlsp.Symbol{}
	for _, s := range tilt.New().Globals() {
		got[s.Name] = s
	}
	// Observed in tilt-dev/tilt.
	for _, want := range []string{
		"k8s_yaml", "docker_build", "k8s_resource", "local_resource", "local",
		"read_json", "default_registry", "include", "sync", "run", "config",
		"os", "watch_file", "load_dynamic", "local_git_repo", "docker_prune_settings",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("tilt host is missing %q", want)
		}
	}
	if got["k8s_yaml"].Doc == "" {
		t.Error("k8s_yaml has no documentation")
	}
}

// This host cannot tell whether the Tilt a Tiltfile will run under has sets,
// and an error on a file that Tilt accepts is worse than a missed error, so it
// accepts set(). It is the same choice the Bazel host makes.
func TestTiltDoesNotReportSet(t *testing.T) {
	if !tilt.New().FileOptions().Set {
		t.Error("the Tilt dialect switches sets off, so every use of set() is reported")
	}

	srv, err := starlsp.New(starlsp.Options{Host: starlsp.Hosts(starlsp.NewVanilla(), tilt.New())})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, d := range srv.Diagnose("/tmp/starlsp-test/Tiltfile", "images = set(['a', 'b'])\nif 'a' in images:\n    print('a')\n") {
		t.Errorf("set() in a Tiltfile got a diagnostic: %s", d.Message)
	}
}
