package starlsp

// What an open file costs, and what a scan of files on disk costs.
//
// gotreesitter allocates a node arena for every tree, and the arena is large
// for a small file: a tree of a 2.5 KB script holds about 3 MB with the version
// this module pins. The numbers below are limits that a change must not
// cross by accident, so a library upgrade that raises them is a decision
// somebody makes on purpose.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func heapInUse() uint64 {
	collect()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func manyDefs(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(defText(i))
	}
	return b.String()
}

// TestAnOpenDocumentStaysSmall: gotreesitter v0.50.1 keeps about 3.0 MB per open
// script of a few kilobytes. v0.56.0 keeps about 7.8 MB, which is 2.6 times as
// much, for trees that are identical on Starkite scripts. An editor with thirty
// files open then holds 235 MB instead of 90 MB. If you raise gotreesitter on
// purpose, raise this limit with it and say why in the changelog.
func TestAnOpenDocumentStaysSmall(t *testing.T) {
	const perDocumentLimit = 5 << 20

	srv, err := New(Options{Host: NewVanilla(), Out: io.Discard, Log: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.docs.open("file:///tmp/starlsp-test/warm.star", 1, manyDefs(50)) // loads the grammar's tables

	src := manyDefs(50) // 2.5 KB
	const docs = 8
	base := heapInUse()
	open := make([]*Document, docs)
	for i := range open {
		open[i] = srv.docs.open(fmt.Sprintf("file:///tmp/starlsp-test/cost%d.star", i), 1, src)
	}
	held := heapInUse() - base
	perDocument := held / docs
	t.Logf("a %d byte script holds %.2f MB while open", len(src), float64(perDocument)/(1<<20))
	if perDocument > perDocumentLimit {
		t.Errorf("an open %d byte script holds %.2f MB, over the %.0f MB limit",
			len(src), float64(perDocument)/(1<<20), float64(perDocumentLimit)/(1<<20))
	}
	runtime.KeepAlive(open)
	runtime.KeepAlive(srv)
}

// workspaceOf writes n scripts to a directory and returns it.
func workspaceOf(t *testing.T, n int, script string) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%d.star", i)), []byte(script), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

func workspaceSymbolRequest(id int, query string) request {
	params, _ := json.Marshal(workspaceSymbolParams{Query: query})
	return request{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: "workspace/symbol", Params: params}
}

// replies reads the results of every reply in the server's output.
func replies(t *testing.T, out []byte) map[string][]SymbolInformation {
	t.Helper()
	got := make(map[string][]SymbolInformation)
	r := newConn(strings.NewReader(string(out)), io.Discard)
	for {
		body, err := r.read()
		if err != nil {
			return got
		}
		var msg struct {
			ID     json.RawMessage     `json:"id"`
			Result []SymbolInformation `json:"result"`
		}
		if json.Unmarshal(body, &msg) == nil && len(msg.ID) > 0 {
			got[string(msg.ID)] = msg.Result
		}
	}
}

// TestAScanOfFilesOnDiskReusesTreeMemory: workspace/symbol parses up to 2,000
// files that are not open. Each file's tree is dead as soon as its symbols are
// listed, so its memory goes back to gotreesitter for the next file. Without
// that, every file costs a fresh 3 MB arena and a scan allocates gigabytes.
func TestAScanOfFilesOnDiskReusesTreeMemory(t *testing.T) {
	const files = 60
	var out lockedBuffer
	srv, err := New(Options{Host: NewVanilla(), Out: &out, Log: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.rootPath = workspaceOf(t, files, manyDefs(50))
	srv.docs.open("file:///tmp/starlsp-test/warm.star", 1, manyDefs(3)) // loads the grammar's tables
	collect()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := srv.handleWorkspaceSymbol(context.Background(), workspaceSymbolRequest(1, "f4")); err != nil {
		t.Fatalf("workspace/symbol: %v", err)
	}
	runtime.ReadMemStats(&after)

	perFile := float64(after.TotalAlloc-before.TotalAlloc) / files
	t.Logf("%d files: %.2f MB allocated per file", files, perFile/(1<<20))
	if perFile > 1<<20 {
		t.Errorf("the scan allocated %.2f MB per file; a file's tree memory is not being reused", perFile/(1<<20))
	}

	// The answers are right. f4, f40 .. f49 match; every file defines all of them.
	got := replies(t, out.Bytes())["1"]
	if want := files * 11; len(got) != want {
		t.Errorf("the scan found %d symbols, want %d", len(got), want)
	}
}

// TestAScanOfFilesOnDiskRunsBesideEditsAndRequests is the same scan with edits
// and requests going on around it. It is meant to be run with -race. The scan
// releases the trees it builds, and nothing that another goroutine can reach may
// be among them.
func TestAScanOfFilesOnDiskRunsBesideEditsAndRequests(t *testing.T) {
	const files = 24
	var out lockedBuffer
	srv, err := New(Options{Host: NewVanilla(), Out: &out, Log: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.rootPath = workspaceOf(t, files, manyDefs(10))

	uri := "file:///tmp/starlsp-test/open.star"
	srv.docs.open(uri, 1, manyDefs(20))

	var wg sync.WaitGroup
	const scanners, rounds = 3, 6
	for g := 0; g < scanners; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				id := g*rounds + r + 1
				if err := srv.handleWorkspaceSymbol(context.Background(), workspaceSymbolRequest(id, "f9")); err != nil {
					t.Errorf("workspace/symbol: %v", err)
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() { // edits, as the inline loop does them
		defer wg.Done()
		for v := 2; v < 40; v++ {
			d := srv.docs.change(uri, v, []contentChange{{Text: manyDefs(20 + v%7)}})
			srv.publishDiagnostics(d)
		}
	}()
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() { // readers of whatever version is current
			defer wg.Done()
			for r := 0; r < 40; r++ {
				d, _ := srv.docs.get(uri)
				srv.documentSymbols(d)
				srv.semanticTokensFor(d)
				srv.hover(d, Position{Line: 1, Character: 6})
				srv.Diagnose("/tmp/starlsp-test/scan-diagnose.star", manyDefs(5))
			}
		}()
	}
	wg.Wait()

	// The open document defines f9 too, so each reply has one symbol more than
	// the files give. What matters is that every reply is whole.
	all := replies(t, out.Bytes())
	if len(all) != scanners*rounds {
		t.Fatalf("got %d replies, want %d", len(all), scanners*rounds)
	}
	for id, syms := range all {
		if len(syms) < files {
			t.Errorf("reply %s lists %d symbols, fewer than the %d files that define f9", id, len(syms), files)
		}
	}
}

func TestReleasingADocumentLeavesNothingToRead(t *testing.T) {
	srv, err := New(Options{Host: NewVanilla(), Out: io.Discard, Log: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := srv.docs.snapshot("file:///tmp/starlsp-test/released.star", 1, []byte(manyDefs(3)))
	if got := len(srv.documentSymbols(d)); got != 3 {
		t.Fatalf("before release the outline has %d symbols, want 3", got)
	}

	d.release()
	d.release() // a second release does nothing

	if d.tree != nil || d.root != nil {
		t.Error("a released Document still points at its tree")
	}
	if got := srv.documentSymbols(d); len(got) != 0 {
		t.Errorf("a released Document still answers an outline: %v", symbolNames(got))
	}
	if string(d.text) != manyDefs(3) {
		t.Error("releasing a Document changed its text")
	}
}
