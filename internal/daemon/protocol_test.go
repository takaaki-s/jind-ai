package daemon

import (
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// fakeServer stands up a Unix socket that captures the raw request bytes and
// replies with a caller-provided Response. It lets us drive Client.send in
// isolation, without spinning up a full Server / session Manager.
func fakeServer(t *testing.T, reply Response) (sockPath string, received *Request) {
	t.Helper()
	sockPath = filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received = &Request{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = json.NewDecoder(conn).Decode(received)
		_ = json.NewEncoder(conn).Encode(reply)
	}()
	return sockPath, received
}

func TestClientSend_StampsRequestProtocolVersion(t *testing.T) {
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true})
	c := NewClient(sock)

	if _, err := c.send(Request{Action: "list"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if received.ProtocolVersion != ProtocolVersion {
		t.Errorf("outgoing ProtocolVersion = %d, want %d", received.ProtocolVersion, ProtocolVersion)
	}
}

func TestClientSend_RejectsMissingProtocolVersion(t *testing.T) {
	// Old daemon shape: no protocol_version field at all → deserializes to 0.
	sock, _ := fakeServer(t, Response{Success: true})
	c := NewClient(sock)

	_, err := c.send(Request{Action: "list"})
	if err == nil {
		t.Fatal("expected error for daemon without protocol_version, got nil")
	}
	if !strings.Contains(err.Error(), "protocol version") || !strings.Contains(err.Error(), "jin daemon restart") {
		t.Errorf("error = %q, want it to mention protocol version and 'jin daemon restart'", err.Error())
	}
}

func TestClientSend_RejectsMismatchedProtocolVersion(t *testing.T) {
	sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion + 1, Success: true})
	c := NewClient(sock)

	_, err := c.send(Request{Action: "list"})
	if err == nil {
		t.Fatal("expected error for mismatched protocol version, got nil")
	}
	if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("error = %q, want it to mention protocol version", err.Error())
	}
}

func TestClientSend_AcceptsMatchingProtocolVersion(t *testing.T) {
	sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true})
	c := NewClient(sock)

	resp, err := c.send(Request{Action: "list"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !resp.Success {
		t.Errorf("Success = false, want true")
	}
}

// TestHandleConnection_RejectsLegacyRequest guards the reverse case: an old
// CLI (no protocol_version stamped) hits a new daemon. The daemon should
// refuse before dispatching to any handler, so no side effects run.
func TestHandleConnection_RejectsLegacyRequest(t *testing.T) {
	s := newTestServer(t)

	rawReq, _ := json.Marshal(Request{Action: "list"}) // no ProtocolVersion set
	resp := exchange(t, s, rawReq)

	if resp.ProtocolVersion != ProtocolVersion {
		t.Errorf("response ProtocolVersion = %d, want %d", resp.ProtocolVersion, ProtocolVersion)
	}
	if resp.Success {
		t.Fatal("expected Success=false for legacy request")
	}
	if !strings.Contains(resp.Error, "client protocol version") {
		t.Errorf("error = %q, want it to mention 'client protocol version'", resp.Error)
	}
}

// TestHandleConnection_ProcessesMatchedRequest verifies the happy path — a
// matched request dispatches normally and the response gets the daemon's
// protocol_version stamped in.
func TestHandleConnection_ProcessesMatchedRequest(t *testing.T) {
	s := newTestServer(t)

	// "list" is a safe action to exercise: it doesn't need session state,
	// tmux, or any external dependency — the empty-list return is fine.
	rawReq, _ := json.Marshal(Request{ProtocolVersion: ProtocolVersion, Action: "list"})
	resp := exchange(t, s, rawReq)

	if resp.ProtocolVersion != ProtocolVersion {
		t.Errorf("response ProtocolVersion = %d, want %d", resp.ProtocolVersion, ProtocolVersion)
	}
	if !resp.Success {
		t.Fatalf("expected Success=true for matched request, got error: %q", resp.Error)
	}
}

// newTestServer stands up a real *Server against a temp dir tree — enough
// to exercise handleConnection end-to-end without spawning the socket
// listener.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "daemon.sock")
	s, err := NewServer(sockPath, filepath.Join(dir, "sessions"), filepath.Join(dir, "config"), filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// exchange writes rawReq into one end of an in-memory pipe, runs
// handleConnection on the other end, and returns the decoded Response.
func exchange(t *testing.T, s *Server, rawReq []byte) Response {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		s.handleConnection(server)
		close(done)
	}()

	if _, err := client.Write(append(rawReq, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}

	var resp Response
	if err := json.NewDecoder(client).Decode(&resp); err != nil && err != io.EOF {
		t.Fatalf("decode response: %v", err)
	}
	<-done
	return resp
}
