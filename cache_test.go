package starlsp

// The analysis cache must make repeat requests cheap without keeping anything
// alive that the server has finished with. A Document owns a parse tree, which
// is megabytes, so a cache entry that holds a Document holds a tree.

import (
	"fmt"
	"runtime"
	"testing"
	"weak"
)

func collect() {
	runtime.GC()
	runtime.GC()
}

func TestTheAnalysisCacheAnswersTheCurrentVersionOnce(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/cache-hit.star"
	d := srv.docs.open(uri, 1, "x = 1\n")

	first, second := srv.analyze(d), srv.analyze(d)
	if first.File == nil || first.File != second.File {
		t.Error("the second request for the same version did not reuse the first one's analysis")
	}

	// A new version is a new question, even with the same version number: a
	// client may reuse a number after it reopens a file.
	reopened := srv.docs.open(uri, 1, "y = 2\n")
	if third := srv.analyze(reopened); third.File == first.File {
		t.Error("the analysis of the old text was served for the new text")
	}
}

// TestACachedAnalysisDoesNotKeepADocumentAlive covers a request that reads a
// Document just before the buffer closes and analyses it just after. Its result
// is of no use to anyone. Caching it with the Document would hold the tree until
// something pushed the entry out.
func TestACachedAnalysisDoesNotKeepADocumentAlive(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const closed = 16
	var refs []weak.Pointer[Document]
	for i := 0; i < closed; i++ {
		uri := fmt.Sprintf("file:///tmp/starlsp-test/closed%d.star", i)
		srv.docs.open(uri, 1, defText(i))
		d, _ := srv.docs.get(uri) // a request goroutine reads the Document...
		srv.docs.close(uri)       // ...the inline loop handles didClose...
		srv.analyses.invalidate(uri)
		_ = srv.analyze(d) // ...and the request carries on
		refs = append(refs, weak.Make(d))
	}

	collect()
	live := 0
	for _, w := range refs {
		if w.Value() != nil {
			live++
		}
	}
	if live != 0 {
		t.Errorf("%d of %d closed Documents are still reachable; the analysis cache is holding them", live, closed)
	}

	srv.analyses.mu.Lock()
	entries := len(srv.analyses.entries)
	srv.analyses.mu.Unlock()
	if entries != 0 {
		t.Errorf("the cache holds %d entries for buffers that are closed", entries)
	}
	runtime.KeepAlive(srv)
}

// TestACachedAnalysisDoesNotKeepASupersededVersionAlive is the same for a buffer
// that was edited while a request was still working on the version before.
func TestACachedAnalysisDoesNotKeepASupersededVersionAlive(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/superseded.star"

	var refs []weak.Pointer[Document]
	srv.docs.open(uri, 1, defText(0))
	for v := 2; v <= 12; v++ {
		old, _ := srv.docs.get(uri)
		cur := srv.docs.change(uri, v, []contentChange{{Text: defText(v)}})
		_ = srv.analyze(cur) // the inline loop publishes diagnostics for the new version
		_ = srv.analyze(old) // a slow request finishes with the one before
		refs = append(refs, weak.Make(old))
	}

	collect()
	live := 0
	for _, w := range refs {
		if w.Value() != nil {
			live++
		}
	}
	// Every one of these versions had structure, so the current Document keeps no
	// earlier one as its fallback. Nothing may hold an old version.
	if live != 0 {
		t.Errorf("%d of %d superseded versions are still reachable; the analysis cache is holding them", live, len(refs))
	}
	runtime.KeepAlive(srv)
}

// TestASlowRequestDoesNotReplaceANewerAnalysis: a request that holds version 1
// can finish after version 2 has been analysed. Its result is right for version
// 1, and it must not push out the entry that every request for version 2 wants.
func TestASlowRequestDoesNotReplaceANewerAnalysis(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := "file:///tmp/starlsp-test/stale.star"
	old := srv.docs.open(uri, 1, "x = 1\n")
	cur := srv.docs.change(uri, 2, []contentChange{{Text: "y = 2\n"}})

	want := srv.analyze(cur) // publishDiagnostics for version 2
	late := srv.analyze(old) // a request that held version 1 finishes late
	if late.File == nil || late.File == want.File {
		t.Fatal("the late request was not answered for its own version")
	}

	again := srv.analyze(cur)
	if again.File != want.File {
		t.Error("version 2's cached analysis was replaced by version 1's, so version 2 was analysed again")
	}
}
