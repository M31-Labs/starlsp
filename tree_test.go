package starlsp

import (
	"strings"
	"testing"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// TestNamedChildrenAgreesWithNamedChild keeps the linear scan honest. It must
// return exactly what gotreesitter's own NamedChildCount and NamedChild return,
// at every node of valid, broken, and hostile buffers.
func TestNamedChildrenAgreesWithNamedChild(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inputs := map[string]string{
		"empty":        "",
		"valid":        "# c\nNAMES = [\"a\", \"b\"]\n\ndef f(a, b = 1, *c, **d):\n    total = a + b\n    return [x for x in c if x]\n\nload(\"//x:y.bzl\", \"z\", w = \"v\")\n",
		"strings":      "x = \"a\\n\\tb\" 'c' \"\"\"d\ne\"\"\" r'f'\n",
		"half typed":   "def main():\n    x = something.\n    return x\n",
		"unclosed":     "x = (1,\ndef f():\n    pass\n",
		"stray tokens": ")]}\n= = =\n..\n",
		"many":         strings.Repeat("x = 1\n", 300),
		"crlf":         "def a():\r\n    return 1\r\n",
		"deep":         "x = " + strings.Repeat("[", 60) + strings.Repeat("]", 60) + "\n",
		"unicode":      "名前 = \"日本語🚀\"\n",
	}
	for name, src := range inputs {
		t.Run(name, func(t *testing.T) {
			d := srv.docs.snapshot("file:///tmp/starlsp-test/named.star", 1, []byte(src))
			if d.root == nil {
				return
			}
			var nodes int
			walk(d.root, func(n *gotreesitter.Node) bool {
				nodes++
				var want []*gotreesitter.Node
				for i := 0; i < n.NamedChildCount(); i++ {
					want = append(want, n.NamedChild(i))
				}
				got := namedChildren(n)
				if len(got) != len(want) {
					t.Fatalf("%s at byte %d: %d named children, want %d", n.Type(srv.lang), n.StartByte(), len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("%s at byte %d: named child %d differs", n.Type(srv.lang), n.StartByte(), i)
					}
				}
				return true
			})
			if nodes == 0 {
				t.Fatal("walked no nodes")
			}
		})
	}
}
