package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serverBehavior selects what the fake daemon does once it has decoded the
// request — the only thing that varies between the shapes below.
type serverBehavior struct {
	reply      *Response // nil = never reply, holding the conn until the test ends
	closeAfter bool      // stop listening once the reply is out
}

// fakeServerWith stands up a Unix socket that captures the request and then
// behaves as b says. It lets us drive Client.send in isolation, without
// spinning up a full Server / session Manager.
func fakeServerWith(t *testing.T, b serverBehavior) (sockPath string, received *Request) {
	t.Helper()
	sockPath = filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		_ = ln.Close()
	})

	received = &Request{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = json.NewDecoder(conn).Decode(received)

		if b.reply == nil {
			<-release
			return
		}
		_ = json.NewEncoder(conn).Encode(*b.reply)
		if b.closeAfter {
			_ = ln.Close()
		}
	}()
	return sockPath, received
}

// fakeServer replies once and keeps listening.
func fakeServer(t *testing.T, reply Response) (sockPath string, received *Request) {
	t.Helper()
	return fakeServerWith(t, serverBehavior{reply: &reply})
}

// hangingServer accepts and reads the request but never replies. This is the
// wedged-daemon shape: dial succeeds, so only a read deadline can break the
// client out of it.
func hangingServer(t *testing.T) (sockPath string) {
	t.Helper()
	sockPath, _ = fakeServerWith(t, serverBehavior{})
	return sockPath
}

// closingServer replies once and then stops listening, the way a real daemon
// behaves after "stop". Without the close, Stop's IsRunning poll would keep
// finding the socket and spin for its full 3s.
func closingServer(t *testing.T, reply Response) (sockPath string) {
	t.Helper()
	sockPath, _ = fakeServerWith(t, serverBehavior{reply: &reply, closeAfter: true})
	return sockPath
}

func TestClientSendWithTimeout_ReportsDeadlineOverrun(t *testing.T) {
	// A short bound stands in for defaultRequestTimeout: the code path is the
	// same and the test stays fast.
	c := NewClient(hangingServer(t))

	_, err := c.sendWithTimeout(Request{Action: "list"}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected a deadline error from a daemon that never replies, got nil")
	}
	if !strings.Contains(err.Error(), "did not respond within") {
		t.Errorf("error = %q, want it to say the daemon did not respond within the bound", err.Error())
	}
	if !strings.Contains(err.Error(), "jin daemon restart") {
		t.Errorf("error = %q, want it to suggest 'jin daemon restart'", err.Error())
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("errors.Is(err, os.ErrDeadlineExceeded) = false for %v, want true", err)
	}
}

// deadlineLog records the bounds the client applied: the dial timeouts it
// asked for, and the read/write deadlines it set, as durations measured from
// the moment of the call.
type deadlineLog struct {
	dial  []time.Duration
	read  []time.Duration
	write []time.Duration
}

type recordingConn struct {
	net.Conn
	log *deadlineLog
}

func (c *recordingConn) SetReadDeadline(t time.Time) error {
	c.log.read = append(c.log.read, time.Until(t))
	return c.Conn.SetReadDeadline(t)
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.log.write = append(c.log.write, time.Until(t))
	return c.Conn.SetWriteDeadline(t)
}

// swapDial replaces the package's dial seam with one that logs every dial
// timeout the client asks for and hands each dialed conn to wrap, restoring
// the original when the test ends. The timeout is logged before dialing, so
// the bound is captured even on the paths where the dial itself fails and
// there is no conn to wrap; wrap receives the same log, which is how a wrapper
// reports what it saw.
func swapDial(t *testing.T, wrap func(net.Conn, *deadlineLog) net.Conn) *deadlineLog {
	t.Helper()
	log := &deadlineLog{}
	original := dialDaemon
	dialDaemon = func(network, address string, timeout time.Duration) (net.Conn, error) {
		log.dial = append(log.dial, timeout)
		conn, err := original(network, address, timeout)
		if err != nil {
			return nil, err
		}
		return wrap(conn, log), nil
	}
	t.Cleanup(func() { dialDaemon = original })
	return log
}

// recordDeadlines swaps the package's dial seam for one that wraps the conn and
// logs every deadline set on it. Observing the calls is the only way to assert
// "no read deadline at all" — waiting can never distinguish an absent deadline
// from a long one — and it is also how the call-site tests below stay fast:
// they read the bound rather than sitting through it.
func recordDeadlines(t *testing.T) *deadlineLog {
	t.Helper()
	return swapDial(t, func(conn net.Conn, log *deadlineLog) net.Conn {
		return &recordingConn{Conn: conn, log: log}
	})
}

// assertOneDeadline checks that exactly one deadline of about want was set.
func assertOneDeadline(t *testing.T, kind string, got []time.Duration, want time.Duration) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("%s deadlines = %v, want exactly one of about %s", kind, got, want)
	}
	// Recorded a hair after the client computed it, so it lands just under
	// want and never over.
	if got[0] > want || got[0] < want-time.Second {
		t.Errorf("%s deadline = %s, want about %s", kind, got[0], want)
	}
}

// assertReadDeadline checks the bound the client applied to the response wait.
// A want of 0 asserts that no read deadline was set at all.
func assertReadDeadline(t *testing.T, got []time.Duration, want time.Duration) {
	t.Helper()
	if want == 0 {
		if len(got) != 0 {
			t.Errorf("read deadlines = %v, want none to be set", got)
		}
		return
	}
	assertOneDeadline(t, "read", got, want)
}

// assertWriteDeadline checks the request-write bound, which — unlike the read
// bound — is the same constant for every call site and is never waived. It
// takes no want for that reason: a call site is not entitled to its own write
// bound, so there is nothing per-caller to declare.
func assertWriteDeadline(t *testing.T, got []time.Duration) {
	t.Helper()
	assertOneDeadline(t, "write", got, requestWriteTimeout)
}

// assertDialTimeout checks the timeout the client handed to the dialer. The
// dial seam takes the value as an argument and can quietly ignore it, so this
// is the only thing standing between the code and a bare net.Dial.
func assertDialTimeout(t *testing.T, got []time.Duration) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("dial timeouts = %v, want exactly one", got)
	}
	if got[0] != dialTimeout {
		t.Errorf("dial timeout = %s, want %s", got[0], dialTimeout)
	}
}

// TestClientSendWithTimeout_ZeroSkipsOnlyTheReadDeadline pins the PanePopup /
// New contract: a caller that cannot name a bound waives the wait for the
// response, and nothing else. The write stays bounded so that a daemon which
// stopped reading cannot wedge Encode forever.
func TestClientSendWithTimeout_ZeroSkipsOnlyTheReadDeadline(t *testing.T) {
	log := recordDeadlines(t)
	sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true})

	resp, err := NewClient(sock).sendWithTimeout(Request{Action: "pane-popup"}, 0)
	if err != nil {
		t.Fatalf("sendWithTimeout with timeout=0: %v", err)
	}
	if !resp.Success {
		t.Errorf("Success = false, want true")
	}

	assertReadDeadline(t, log.read, 0)
	assertWriteDeadline(t, log.write)
	assertDialTimeout(t, log.dial)
}

// TestClientCallSites_PassTheirOwnBound pins which bound each call site picks.
// A correct sendWithTimeout is worth nothing if a call site quietly reverts to
// send() and inherits the default.
func TestClientCallSites_PassTheirOwnBound(t *testing.T) {
	tests := []struct {
		name     string
		call     func(*Client) error
		wantRead time.Duration // 0 = the call must set no read deadline
	}{
		{"SendHook", func(c *Client) error { return c.SendHook(HookRequest{}) }, hookRequestTimeout},
		{"PanePopup", func(c *Client) error { return c.PanePopup("id", "cmd", "", "", "") }, 0},
		{"NewWithOptions", func(c *Client) error { _, _, err := c.NewWithOptions(NewOptions{}); return err }, 0},
		{"Delete", func(c *Client) error { return c.Delete("id", true, false) }, 0},
		{"List via send", func(c *Client) error { _, err := c.List(); return err }, defaultRequestTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := recordDeadlines(t)
			// Data "null" unmarshals into every response payload shape, so one
			// reply serves every call site.
			sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: json.RawMessage("null")})

			if err := tt.call(NewClient(sock)); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			assertReadDeadline(t, log.read, tt.wantRead)
			// Asserted for every row rather than declared per row: the write
			// and dial bounds are properties of the transport, so a call site
			// that wants its own is already wrong.
			assertWriteDeadline(t, log.write)
			assertDialTimeout(t, log.dial)
		})
	}
}

// TestClientIsRunning_BoundsTheDial pins the third of this package's bounds on
// the one path that has nothing else: IsRunning never reads or writes, so the
// dial timeout is all that keeps it from blocking on a socket nobody accepts.
func TestClientIsRunning_BoundsTheDial(t *testing.T) {
	log := recordDeadlines(t)
	sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true})

	if !NewClient(sock).IsRunning() {
		t.Fatal("IsRunning = false against a listening socket, want true")
	}
	assertDialTimeout(t, log.dial)
}

// deadWriteConn accepts the connection and then fails every write the way a
// blown write deadline does. Reproducing a real write stall would mean filling
// the socket buffer against a daemon that stopped reading, then waiting out
// requestWriteTimeout; the failure mode is what matters here, not how the
// kernel gets there.
type deadWriteConn struct {
	net.Conn
}

func (c *deadWriteConn) Write([]byte) (int, error) {
	return 0, os.ErrDeadlineExceeded
}

// TestClientSend_WriteOverrunSaysTheDaemonStoppedReading guards the split F023
// asked for: a write that runs out of time means the daemon stopped reading,
// and saying "did not respond" there would send the reader looking at the
// wrong half of the exchange. The unknown-outcome clause still applies — see
// wrapDeadline for why a timed-out write may nonetheless have delivered a
// complete request.
func TestClientSend_WriteOverrunSaysTheDaemonStoppedReading(t *testing.T) {
	swapDial(t, func(conn net.Conn, _ *deadlineLog) net.Conn {
		return &deadWriteConn{Conn: conn}
	})

	sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true})
	err := NewClient(sock).Delete("id", true, false)
	if err == nil {
		t.Fatal("expected an error when the request cannot be written, got nil")
	}
	if !strings.Contains(err.Error(), "stopped reading the request within") {
		t.Errorf("error = %q, want it to say the daemon stopped reading", err.Error())
	}
	if strings.Contains(err.Error(), "did not respond") {
		t.Errorf("error = %q, want it not to blame the response", err.Error())
	}
	if !strings.Contains(err.Error(), "outcome is unknown") {
		t.Errorf("error = %q, want the unknown-outcome warning for a mutating action", err.Error())
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("errors.Is(err, os.ErrDeadlineExceeded) = false for %v, want true", err)
	}
}

// TestClientStop_PassesStopBound covers the fourth call site separately: Stop
// polls IsRunning afterwards, so it needs a server that actually goes away.
func TestClientStop_PassesStopBound(t *testing.T) {
	log := recordDeadlines(t)
	c := NewClient(closingServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true}))

	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertReadDeadline(t, log.read, stopRequestTimeout)
}

// deadReadConn accepts the connection and lets the request through, then fails
// every read the way a blown read deadline does. It buys the stop test its
// timeout instantly instead of waiting out stopRequestTimeout.
type deadReadConn struct {
	net.Conn
}

func (c *deadReadConn) Read([]byte) (int, error) {
	return 0, os.ErrDeadlineExceeded
}

// TestClientStop_TimeoutDoesNotSuggestRestart guards F010: a stop that timed
// out must not be answered by naming the command that routes back through it.
// See Client.Stop for why, and docs/ipc-protocol.md for where this sits among
// the other timeout messages.
func TestClientStop_TimeoutDoesNotSuggestRestart(t *testing.T) {
	swapDial(t, func(conn net.Conn, _ *deadlineLog) net.Conn {
		return &deadReadConn{Conn: conn}
	})
	// The wedged shape: the daemon takes the request, never answers, and keeps
	// accepting. Reaching the exhausted poll needs both halves — an answer
	// would end the send, and a closed listener would end the poll.
	sock := hangingServer(t)

	err := NewClient(sock).stop(2, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the daemon never confirms the stop, got nil")
	}
	if strings.Contains(err.Error(), "jin daemon restart") {
		t.Errorf("error = %q, want it not to suggest the command that routes back here", err.Error())
	}
	if !strings.Contains(err.Error(), "pkill") {
		t.Errorf("error = %q, want a remedy that does not go through the socket", err.Error())
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("errors.Is(err, os.ErrDeadlineExceeded) = false for %v, want true", err)
	}
}

// TestClientStop_PassesItsConstantsToThePoll covers what the seam moved out of
// reach. Every other stop test drives stop() directly and so says nothing about
// the one line that supplies its arguments — drop that to zero attempts and the
// package still passes, while a stop that timed out on the send would start
// reporting failure for a daemon that did shut down.
func TestClientStop_PassesItsConstantsToThePoll(t *testing.T) {
	// The documented budget. Asserted rather than described so that changing
	// either constant has to face the comment that quotes their product.
	if got := time.Duration(stopPollAttempts) * stopPollInterval; got != 3*time.Second {
		t.Errorf("stop poll budget = %s, want the documented 3s", got)
	}

	// Nothing is listening, so the send fails and the poll is the only thing
	// that can tell the daemon is nonetheless gone. Hand the poll no attempts
	// and that send error is what Stop returns instead of nil.
	c := NewClient(filepath.Join(t.TempDir(), "daemon.sock"))

	if err := c.Stop(); err != nil {
		t.Fatalf("Stop against a daemon that is already gone: %v", err)
	}
}

// TestReadOnlyActions_NameRealActions guards F021. Membership is opt-in, so a
// typo fails the same silent-and-harmless way an honest omission does: the
// action simply never matches and keeps the cautious wording. That makes the
// map the one place where a misspelling has no symptom, and this is the only
// thing that would notice.
//
// Spelling is the whole scope. Whether a named action only reads is a claim
// about its handler that no test here checks — adding "delete" to the map
// would pass this and silently drop the unknown-outcome warning. That stays a
// question for review.
func TestReadOnlyActions_NameRealActions(t *testing.T) {
	if len(readOnlyActions) == 0 {
		t.Fatal("readOnlyActions is empty; this test would pass vacuously")
	}
	s := newTestServer(t)

	for action := range readOnlyActions {
		t.Run(action, func(t *testing.T) {
			// Data stays nil: every handler here either ignores it or fails
			// its json.Unmarshal, so the dispatch is exercised without any
			// side effect. Success is therefore not asserted — only that the
			// switch recognized the name.
			resp := s.handleRequest(&Request{Action: action})
			if strings.Contains(resp.Error, "unknown action") {
				t.Errorf("handleRequest(%q) = %q; readOnlyActions names an action the server does not dispatch", action, resp.Error)
			}
		})
	}
}

// TestWrapDeadline_WarnsOnlyForMutatingActions guards the credibility of the
// unknown-outcome warning: emitting it for a failed list would train users to
// ignore it on delete.
func TestWrapDeadline_WarnsOnlyForMutatingActions(t *testing.T) {
	const stalled = "daemon did not respond within 1s"

	readOnly := wrapDeadline(os.ErrDeadlineExceeded, "list", stalled)
	if strings.Contains(readOnly.Error(), "outcome is unknown") {
		t.Errorf("read-only action error = %q, want no unknown-outcome warning", readOnly.Error())
	}

	mutating := wrapDeadline(os.ErrDeadlineExceeded, "delete", stalled)
	if !strings.Contains(mutating.Error(), "outcome is unknown") {
		t.Errorf("mutating action error = %q, want the unknown-outcome warning", mutating.Error())
	}

	// An action nobody classified must fall on the cautious side.
	unclassified := wrapDeadline(os.ErrDeadlineExceeded, "some-future-action", stalled)
	if !strings.Contains(unclassified.Error(), "outcome is unknown") {
		t.Errorf("unclassified action error = %q, want the cautious default", unclassified.Error())
	}
}

// TestClientSend_DistinguishesNotRunningFromUnresponsive guards the reason for
// splitting the dial error: "not started" and "started but wedged" need
// different remedies. The not-running wording is load-bearing for users and
// must not drift.
func TestClientSend_DistinguishesNotRunningFromUnresponsive(t *testing.T) {
	notRunning := NewClient(filepath.Join(t.TempDir(), "daemon.sock"))
	_, notRunningErr := notRunning.send(Request{Action: "list"})
	if notRunningErr == nil {
		t.Fatal("expected an error when nothing is listening, got nil")
	}
	if notRunningErr.Error() != "daemon not running. Start with: jin daemon" {
		t.Errorf("error = %q, want the unchanged not-running wording", notRunningErr.Error())
	}

	unresponsive := NewClient(hangingServer(t))
	_, unresponsiveErr := unresponsive.sendWithTimeout(Request{Action: "list"}, 100*time.Millisecond)
	if unresponsiveErr == nil {
		t.Fatal("expected an error from a daemon that never replies, got nil")
	}
	if unresponsiveErr.Error() == notRunningErr.Error() {
		t.Errorf("unresponsive daemon reports the same error as a missing one: %q", unresponsiveErr.Error())
	}
	if strings.Contains(unresponsiveErr.Error(), "daemon not running") {
		t.Errorf("error = %q, want it not to claim the daemon is not running", unresponsiveErr.Error())
	}
	// Without this the test passes on any error at all — including one raised
	// before the bound was ever reached — and would keep passing if the read
	// deadline stopped being set.
	if !errors.Is(unresponsiveErr, os.ErrDeadlineExceeded) {
		t.Errorf("errors.Is(err, os.ErrDeadlineExceeded) = false for %v, want the error to come from the bound", unresponsiveErr)
	}
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
