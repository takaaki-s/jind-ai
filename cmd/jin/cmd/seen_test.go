package cmd

import (
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// TestSeenCmd_RegisteredOnSession verifies `jin session seen` is wired up as a
// subcommand of `jin session` and declares an argument validator.
func TestSeenCmd_RegisteredOnSession(t *testing.T) {
	var registered bool
	for _, sub := range sessionCmd.Commands() {
		if sub.Name() == "seen" {
			registered = true
			break
		}
	}
	if !registered {
		t.Fatal("sessionCmd is missing the seen subcommand")
	}
	if seenCmd.Args == nil {
		t.Error("seenCmd.Args is nil, want cobra.ExactArgs(1)")
	}
}

// recordingDaemon is a socket answering the real protocol, recording the
// actions it was asked for. It exists because the command's own wiring — which
// action it sends — has no other seam.
type recordingDaemon struct {
	mu      sync.Mutex
	actions []string
}

func (d *recordingDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.actions...)
}

// startRecordingDaemon answers `list` with sessions and every other action
// through reply, and points JIN_SOCKET at itself.
func startRecordingDaemon(t *testing.T, sessions []session.Info, reply func() daemon.Response) *recordingDaemon {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	t.Setenv("JIN_SOCKET", sock)

	d := &recordingDaemon{}
	listed, _ := json.Marshal(sessions)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req daemon.Request
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				_ = conn.Close()
				continue
			}
			d.mu.Lock()
			d.actions = append(d.actions, req.Action)
			d.mu.Unlock()

			resp := reply()
			if req.Action == "list" {
				resp = daemon.Response{Success: true, Data: listed}
			}
			resp.ProtocolVersion = daemon.ProtocolVersion
			_ = json.NewEncoder(conn).Encode(resp)
			_ = conn.Close()
		}
	}()
	return d
}

// okWith is the ordinary success reply carrying one session.Info.
func okWith(info session.Info) func() daemon.Response {
	data, _ := json.Marshal(info)
	return func() daemon.Response { return daemon.Response{Success: true, Data: data} }
}

// The command must send `attention-seen`. Swapping the call for any other
// action that returns a *session.Info — `get`, say — leaves the printed line
// and the JSON output identical while the receipt is never acknowledged.
func TestSeenCmd_SendsTheAttentionSeenAction(t *testing.T) {
	listed := session.Info{ID: "abcd1234-0000", Description: "auth callback"}
	acknowledged := listed
	acknowledged.Attention = session.AttentionInfo{
		State: session.AttentionDone, Generation: 2, SeenGeneration: 2,
	}

	d := startRecordingDaemon(t, []session.Info{listed}, okWith(acknowledged))

	if err := seenCmd.RunE(seenCmd, []string{"auth"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got := d.seen()
	if len(got) != 2 || got[0] != "list" || got[1] != "attention-seen" {
		t.Errorf("actions = %v, want [list attention-seen]", got)
	}
}

// A daemon that refuses must fail the command rather than print a success line
// over an acknowledgement that never happened.
func TestSeenCmd_SurfacesTheDaemonError(t *testing.T) {
	startRecordingDaemon(t,
		[]session.Info{{ID: "abcd1234-0000", Description: "auth callback"}},
		func() daemon.Response { return daemon.Response{Success: false, Error: "session not found"} })

	if err := seenCmd.RunE(seenCmd, []string{"auth"}); err == nil {
		t.Fatal("RunE returned nil while the daemon refused the acknowledgement")
	}
}
