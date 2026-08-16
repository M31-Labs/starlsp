package starlsp

import (
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

// session drives a real JSON-RPC conversation against the server, over pipes,
// exactly as an editor would. Testing through the wire rather than calling
// handlers directly is what proves the protocol layer works too.
type session struct {
	t       *testing.T
	toSrv   *io.PipeWriter
	fromSrv *bufferedReader
	nextID  int
	done    chan error
}

func newSession(t *testing.T) *session {
	t.Helper()

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	host := testHostOverride
	if host == nil {
		host = hostFor(t)
	}
	srv, err := New(Options{
		Host: host,
		In:   serverReader,
		Out:  serverWriter,
		Log:  io.Discard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	s := &session{
		t:       t,
		toSrv:   clientWriter,
		fromSrv: newBufferedReader(clientReader),
		nextID:  1,
		done:    make(chan error, 1),
	}
	go func() { s.done <- srv.Run() }()

	t.Cleanup(func() {
		s.notify("exit", nil)
		clientWriter.Close()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			t.Log("server did not stop within 5s")
		}
	})

	s.request("initialize", initializeParams{})
	s.notify("initialized", struct{}{})
	return s
}

// request sends a request and returns its result payload.
func (s *session) request(method string, params any) json.RawMessage {
	s.t.Helper()
	id := s.nextID
	s.nextID++

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		s.t.Fatalf("marshal %s: %v", method, err)
	}
	s.send(body)

	// Skip server-initiated notifications until the matching reply arrives.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := s.receive()
		var envelope struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *responseError  `json:"error"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			s.t.Fatalf("unmarshal reply to %s: %v", method, err)
		}
		if envelope.ID == nil || *envelope.ID != id {
			continue
		}
		if envelope.Error != nil {
			s.t.Fatalf("%s returned error: %s", method, envelope.Error.Message)
		}
		return envelope.Result
	}
	s.t.Fatalf("timed out waiting for a reply to %s", method)
	return nil
}

func (s *session) notify(method string, params any) {
	s.t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		s.t.Fatalf("marshal %s: %v", method, err)
	}
	s.send(body)
}

func (s *session) send(body []byte) {
	s.t.Helper()
	if _, err := fmt.Fprintf(s.toSrv, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

func (s *session) receive() []byte {
	s.t.Helper()
	body, err := s.fromSrv.readMessage()
	if err != nil {
		s.t.Fatalf("read: %v", err)
	}
	return body
}

// waitDiagnostics reads until a publishDiagnostics notification for uri.
func (s *session) waitDiagnostics(uri string) []Diagnostic {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := s.receive()
		var envelope struct {
			Method string                   `json:"method"`
			Params publishDiagnosticsParams `json:"params"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}
		if envelope.Method == "textDocument/publishDiagnostics" && envelope.Params.URI == uri {
			return envelope.Params.Diagnostics
		}
	}
	s.t.Fatalf("timed out waiting for diagnostics on %s", uri)
	return nil
}

// open sends didOpen and returns the diagnostics the server publishes for it.
func (s *session) open(uri, text string) []Diagnostic {
	s.t.Helper()
	s.notify("textDocument/didOpen", didOpenParams{
		TextDocument: TextDocumentItem{URI: uri, LanguageID: "starlark", Version: 1, Text: text},
	})
	return s.waitDiagnostics(uri)
}

// bufferedReader reads Content-Length framed messages from a stream.
type bufferedReader struct{ c *conn }

func newBufferedReader(r io.Reader) *bufferedReader {
	return &bufferedReader{c: newConn(r, io.Discard)}
}

func (b *bufferedReader) readMessage() ([]byte, error) { return b.c.read() }

// requestAllowingError sends a request and returns either its result or its
// error object, instead of failing the test on an error reply.
func (s *session) requestAllowingError(method string, params any) (json.RawMessage, *responseError) {
	s.t.Helper()
	id := s.nextID
	s.nextID++

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		s.t.Fatalf("marshal %s: %v", method, err)
	}
	s.send(body)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := s.receive()
		var envelope struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *responseError  `json:"error"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}
		if envelope.ID == nil || *envelope.ID != id {
			continue
		}
		return envelope.Result, envelope.Error
	}
	s.t.Fatalf("timed out waiting for a reply to %s", method)
	return nil, nil
}
