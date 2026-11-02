package starlsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
)

// This file is the server's resilience layer. Three properties matter for a
// language server that runs all day inside somebody's editor, and none of them
// are about features:
//
//   - It must not die. A panic in one handler, on one strange file, would
//     otherwise take the whole session with it and leave the editor with a
//     dead server and no explanation.
//   - It must not block. A workspace scan that takes two seconds must not
//     freeze completion for those two seconds.
//   - It must honour cancellation. Editors cancel constantly — every keystroke
//     supersedes the completion request before it — and work that nobody is
//     waiting for should stop.

// requestContext tracks one in-flight request so it can be cancelled.
type requestContext struct {
	cancel context.CancelFunc
}

// inflight is the set of requests currently being served.
type inflight struct {
	mu sync.Mutex
	m  map[string]requestContext
}

func newInflight() *inflight { return &inflight{m: make(map[string]requestContext)} }

func (f *inflight) add(id string, cancel context.CancelFunc) {
	f.mu.Lock()
	f.m[id] = requestContext{cancel: cancel}
	f.mu.Unlock()
}

func (f *inflight) done(id string) {
	f.mu.Lock()
	delete(f.m, id)
	f.mu.Unlock()
}

// cancel stops one request. It is a no-op when the request already finished,
// which is the common case: a cancellation usually races the reply.
func (f *inflight) cancel(id string) {
	f.mu.Lock()
	rc, ok := f.m[id]
	f.mu.Unlock()
	if ok {
		rc.cancel()
	}
}

func (f *inflight) cancelAll() {
	f.mu.Lock()
	for _, rc := range f.m {
		rc.cancel()
	}
	f.m = make(map[string]requestContext)
	f.mu.Unlock()
}

// maxConcurrentRequests bounds how many read-only requests run at once.
//
// The cap exists so a client that floods the server cannot start unbounded
// goroutines, each holding a parse tree. Well past what an editor generates.
const maxConcurrentRequests = 16

// Run serves the client until the stream closes or the client exits.
//
// Notifications that mutate a document are handled inline, in arrival order,
// because their order is their meaning: a didChange that overtook its didOpen
// would corrupt the buffer. Everything else is read-only and runs concurrently.
//
// The two can overlap, and that is safe because of how documents are stored. An
// edit never changes a Document; it builds the next one and swaps it in. A
// request takes one Document when it starts and reads only that. See Document.
func (s *Server) Run() error {
	defer s.pending.cancelAll()

	for {
		body, err := s.conn.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.wait()
				return nil
			}
			s.wait()
			return err
		}

		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			_ = s.conn.replyError(nil, errParse, err.Error())
			continue
		}

		if s.handleInline(req) {
			if s.stopped() {
				s.wait()
				return nil
			}
			continue
		}
		s.handleConcurrently(req)
	}
}

// handleInline runs the messages whose order matters, and reports whether it
// took the message.
func (s *Server) handleInline(req request) bool {
	switch req.Method {
	case "initialize", "initialized", "shutdown", "exit",
		"textDocument/didOpen", "textDocument/didChange",
		"textDocument/didSave", "textDocument/didClose",
		"workspace/didChangeConfiguration", "workspace/didChangeWatchedFiles",
		"$/cancelRequest", "$/setTrace":
		s.safely(req, func() error { return s.dispatch(context.Background(), req) })
		return true
	default:
		return false
	}
}

// handleConcurrently runs a read-only request in its own goroutine.
func (s *Server) handleConcurrently(req request) {
	s.slots <- struct{}{}
	s.active.Add(1)

	ctx, cancel := context.WithCancel(context.Background())
	id := string(req.ID)
	if id != "" {
		s.pending.add(id, cancel)
	}

	go func() {
		defer func() {
			cancel()
			if id != "" {
				s.pending.done(id)
			}
			<-s.slots
			s.active.Done()
		}()
		s.safely(req, func() error { return s.dispatch(ctx, req) })
	}()
}

// safely runs a handler and converts a panic into an error reply.
//
// A language server that dies on one bad input is worse than one that answers
// badly: the editor is left with no server and the user with no explanation.
// The stack goes to the log, and the client gets a normal error response and
// carries on.
func (s *Server) safely(req request, fn func() error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		s.logf("panic handling %s: %v\n%s", req.Method, r, debug.Stack())
		if len(req.ID) > 0 {
			_ = s.conn.replyError(req.ID, errInternal,
				fmt.Sprintf("internal error handling %s; the server is still running", req.Method))
		}
	}()

	if err := fn(); err != nil {
		if errors.Is(err, errExit) {
			s.markStopped()
			return
		}
		s.logf("handling %s: %v", req.Method, err)
	}
}

// wait lets in-flight requests finish before the process exits, so a reply is
// not lost to a half-closed stream.
func (s *Server) wait() {
	s.pending.cancelAll()
	s.active.Wait()
}

func (s *Server) markStopped() {
	s.mu.Lock()
	s.exiting = true
	s.mu.Unlock()
}

func (s *Server) stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exiting
}

// cancelParams is the payload of $/cancelRequest.
type cancelParams struct {
	ID json.RawMessage `json:"id"`
}

// handleCancel stops an in-flight request.
func (s *Server) handleCancel(req request) error {
	var params cancelParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return err
	}
	s.pending.cancel(string(params.ID))
	return nil
}

// checkCancelled reports whether the caller should stop.
//
// Handlers consult it between phases rather than mid-loop, so a cancelled
// request stops at a consistent point instead of returning half an answer.
func checkCancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
