package starlsp_test

// This gate is the project's central claim: against real code, with the
// matching dialect configured, the server reports nothing.
//
// It lives in the external test package because it imports the dialect
// packages, which import the package under test. It uses only the exported
// API — which is itself worth proving, since that is the API an embedder gets.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/M31-Labs/starlsp"
	"github.com/M31-Labs/starlsp/dialects/bazel"
	"github.com/M31-Labs/starlsp/dialects/tilt"
)

func TestRealWorldCorpusIsClean(t *testing.T) {
	root := os.Getenv("GTS_STARLARK_CORPUS")
	if root == "" {
		t.Skip("set GTS_STARLARK_CORPUS to a directory of real Starlark")
	}

	cases := []struct {
		dialect string
		host    starlsp.Host
		matches func(base string) bool
	}{
		{
			dialect: "bazel",
			host:    starlsp.Hosts(starlsp.NewVanilla(), bazel.New()),
			matches: func(b string) bool {
				// BUILD.<something>.bazel files are frequently templates
				// carrying {placeholder} braces, which are not Starlark at all.
				if strings.HasPrefix(b, "BUILD.") && b != "BUILD.bazel" {
					return false
				}
				return strings.HasSuffix(b, ".bzl") || b == "BUILD" || b == "BUILD.bazel" ||
					b == "WORKSPACE" || b == "MODULE.bazel"
			},
		},
		{
			dialect: "tilt",
			host:    starlsp.Hosts(starlsp.NewVanilla(), tilt.New()),
			matches: func(b string) bool { return b == "Tiltfile" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			server, err := starlsp.New(starlsp.Options{Host: tc.host})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			var files, errorCount int
			_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() || !tc.matches(info.Name()) {
					return nil
				}
				files++
				diags, err := server.DiagnoseFile(path)
				if err != nil {
					return nil
				}
				for _, d := range diags {
					if d.Severity != starlsp.SeverityError {
						continue
					}
					errorCount++
					if errorCount <= 20 {
						rel, _ := filepath.Rel(root, path)
						t.Errorf("%s:%d:%d: %s", rel, d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
					}
				}
				return nil
			})

			if files == 0 {
				t.Skipf("no %s files under %s", tc.dialect, root)
			}
			t.Logf("%s: %d files, %d errors", tc.dialect, files, errorCount)
			if errorCount > 0 {
				t.Fatalf("%s dialect reported %d errors on code its own tool accepts", tc.dialect, errorCount)
			}
		})
	}
}
