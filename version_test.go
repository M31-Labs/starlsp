package starlsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The server binary, the VS Code extension, and the git tag all carry a
// version, and nothing stops them drifting apart. They have drifted already:
// a release bumped the constant here and the extension's manifest, but the
// installed binary still reported the previous version, which makes a bug
// report impossible to interpret.
//
// This test makes the three agree by construction.
func TestExtensionVersionMatchesTheServer(t *testing.T) {
	manifest := filepath.Join("editors", "vscode", "package.json")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Skipf("no extension manifest at %s", manifest)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("parsing %s: %v", manifest, err)
	}
	if pkg.Version != Version {
		t.Errorf("version drift: server is %s, the VS Code extension is %s.\n"+
			"Update both, or a user's bug report cannot be matched to a build.",
			Version, pkg.Version)
	}
}
