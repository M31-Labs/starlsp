package starlsp

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestOutlineAndCompletionScaleLinearly guards the scan above. Looping over
// NamedChild(i) rescans the children from the start each time, so a module with
// tens of thousands of statements made the outline, and every completion, take
// seconds. The budget is far above the linear cost and far below the quadratic one.
func TestOutlineAndCompletionScaleLinearly(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const statements = 40_000
	var b strings.Builder
	for i := 0; i < statements; i++ {
		fmt.Fprintf(&b, "x%d = %d\n", i, i)
	}
	d := srv.docs.open("file:///tmp/starlsp-test/many.star", 1, b.String())

	start := time.Now()
	symbols := srv.documentSymbols(d)
	outline := time.Since(start)

	start = time.Now()
	srv.complete(d, Position{Line: 1})
	completion := time.Since(start)

	if len(symbols) != statements {
		t.Fatalf("outline has %d symbols, want %d", len(symbols), statements)
	}
	const budget = 5 * time.Second
	if outline > budget || completion > budget {
		t.Errorf("%d statements: outline took %v and completion %v, budget %v: the scan is quadratic again",
			statements, outline, completion, budget)
	}
	t.Logf("%d statements: outline %v, completion %v", statements, outline, completion)
}
