//go:build e2e

package tui

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

// Whether an attach happened is not observable without a real pane:
// switchToSession reports nothing, and with a nil tmux client it returns
// before doing anything. So the positive half of "explicit attach
// acknowledges" lives here, against the same two-pane layout `jin ui` builds.

// attachableSession is a session switchToSession will actually attach to: alive,
// with an inner tmux session name, holding a completion nobody has seen.
func attachableSession(id string) session.Info {
	return session.Info{
		ID: id, Description: id, Fleet: "f",
		Status:         session.StatusIdle,
		TmuxWindowName: "sess-" + id,
		Attention: session.AttentionInfo{
			State: session.AttentionDone, Generation: 1, Unseen: true,
		},
	}
}

func TestE2E_ExplicitAttachAcknowledges(t *testing.T) {
	tc, displayPaneID := outerTmuxFixture(t)
	d, client := startFakeDaemon(t)

	m := Model{
		client: client, sessions: []session.Info{attachableSession("s1")},
		cursor: 0, deletingIDs: map[string]bool{}, height: 100,
		tmuxClient: tc, displayPaneID: displayPaneID,
	}

	next, cmd := m.handleSelectSession()
	nm := next.(Model)

	if nm.currentSessionID != "s1" {
		t.Fatalf("currentSessionID = %q, want %q — the fixture did not attach", nm.currentSessionID, "s1")
	}
	if nm.err != nil {
		t.Errorf("err = %v, want nil", nm.err)
	}
	if cmd == nil {
		t.Error("no refresh Cmd; the dot would linger until the next poll")
	}
	req := d.first(t)
	if req.Action != "attention-seen" {
		t.Fatalf("first action = %q, want %q", req.Action, "attention-seen")
	}
}

func TestE2E_ExplicitAttachOnTheDisplayedSessionStillAcknowledges(t *testing.T) {
	tc, displayPaneID := outerTmuxFixture(t)
	d, client := startFakeDaemon(t)

	m := Model{
		client: client, sessions: []session.Info{attachableSession("s1")},
		cursor: 0, deletingIDs: map[string]bool{}, height: 100,
		tmuxClient: tc, displayPaneID: displayPaneID,
	}

	// Attach for real first — recordDisplayedSession alone would not leave the
	// live-attach state a second Enter has to meet.
	next, _ := m.handleSelectSession()
	m = next.(Model)
	if len(d.actions()) != 1 {
		t.Fatalf("first attach sent %v, want exactly one request", d.actions())
	}

	// The pane is already on this session, so switchToSession takes its
	// "already displaying this" early return.
	next, _ = m.handleSelectSession()
	if nm := next.(Model); nm.err != nil {
		t.Errorf("err = %v, want nil", nm.err)
	}

	if got := d.actions(); len(got) != 2 || got[1] != "attention-seen" {
		t.Errorf("daemon saw %v, want a second attention-seen", got)
	}
}

// The picker can select a session that is not running, and unlike
// handleSelectSession it does not start it first — so the pane gets a
// placeholder rather than an attach.
func TestE2E_PickerOnAStoppedSessionDoesNotAcknowledge(t *testing.T) {
	tc, displayPaneID := outerTmuxFixture(t)
	d, client := startFakeDaemon(t)

	sess := attachableSession("s1")
	sess.Status = session.StatusStopped
	m := &Model{
		client: client, sessions: []session.Info{sess},
		deletingIDs: map[string]bool{}, height: 100,
		tmuxClient: tc, displayPaneID: displayPaneID,
		focusSessionID: "s1", focusFromPicker: true,
	}

	resolved, cmd := m.resolveFocusSession()
	if !resolved {
		t.Fatal("resolveFocusSession() = false, want true")
	}
	// The pane did take the session — as a placeholder, which is the case
	// currentSessionID alone would read as success.
	if m.currentSessionID != "s1" {
		t.Fatalf("currentSessionID = %q, want %q — the fixture did not render the placeholder",
			m.currentSessionID, "s1")
	}
	if m.displayLocalAttach {
		t.Fatal("displayLocalAttach = true after a placeholder; the test proves nothing")
	}

	if got := d.actions(); len(got) != 0 {
		t.Errorf("daemon saw %v, want no request", got)
	}
	if cmd != nil {
		t.Error("a placeholder returned a refresh Cmd")
	}
}

// An alive session jind-ai has no inner session name for cannot be attached:
// switchToSession bails without recording it. Nothing was looked at.
func TestE2E_AttachThatCannotHappenDoesNotAcknowledge(t *testing.T) {
	tc, displayPaneID := outerTmuxFixture(t)
	d, client := startFakeDaemon(t)

	sess := attachableSession("s1")
	sess.TmuxWindowName = ""
	m := Model{
		client: client, sessions: []session.Info{sess},
		cursor: 0, deletingIDs: map[string]bool{}, height: 100,
		tmuxClient: tc, displayPaneID: displayPaneID,
	}

	next, _ := m.handleSelectSession()
	if nm := next.(Model); nm.currentSessionID == "s1" {
		t.Fatal("the fixture attached after all; the test proves nothing")
	}
	if got := d.actions(); len(got) != 0 {
		t.Errorf("daemon saw %v, want no request", got)
	}
}

// The same landed attach, acknowledged or not depending only on where the
// pending focus came from (see consumeEnvRequests).
func TestE2E_OnlyPickerFocusAcknowledges(t *testing.T) {
	for _, tt := range []struct {
		name        string
		fromPicker  bool
		wantRequest bool
	}{
		{"switch-session popup", true, true},
		{"external focus", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc, displayPaneID := outerTmuxFixture(t)
			d, client := startFakeDaemon(t)

			m := &Model{
				client: client, sessions: []session.Info{attachableSession("s1")},
				deletingIDs: map[string]bool{}, height: 100,
				tmuxClient: tc, displayPaneID: displayPaneID,
				focusSessionID: "s1", focusFromPicker: tt.fromPicker,
			}

			resolved, cmd := m.resolveFocusSession()
			if !resolved {
				t.Fatal("resolveFocusSession() = false, want true")
			}
			if m.currentSessionID != "s1" {
				t.Fatalf("currentSessionID = %q, want %q — the fixture did not attach", m.currentSessionID, "s1")
			}

			got := d.actions()
			if tt.wantRequest {
				if len(got) == 0 || got[0] != "attention-seen" {
					t.Errorf("daemon saw %v, want it to start with attention-seen", got)
				}
				if cmd == nil {
					t.Error("no refresh Cmd after acknowledging")
				}
				return
			}
			if len(got) != 0 {
				t.Errorf("daemon saw %v, want no request", got)
			}
			if cmd != nil {
				t.Error("an implicit focus returned a refresh Cmd")
			}
		})
	}
}

// The live-attach flag alone says an attach is up, not which session it is on.
// Without a display pane switchToSession returns before touching anything, so
// the pane keeps the previous session — and the cursor row must not be
// acknowledged for a look that went somewhere else.
func TestE2E_AttachThatNeverSwitchedDoesNotAcknowledgeTheCursorRow(t *testing.T) {
	tc, _ := outerTmuxFixture(t)
	d, client := startFakeDaemon(t)

	m := Model{
		client: client,
		sessions: []session.Info{
			attachableSession("s1"), attachableSession("s2"),
		},
		cursor: 1, deletingIDs: map[string]bool{}, height: 100,
		tmuxClient: tc,
		// No display pane: switchToSession's first guard returns.
		displayPaneID: "",
		// The state a live attach to s1 left behind.
		currentSessionID: "s1", displayLocalAttach: true,
	}

	next, _ := m.handleSelectSession()
	if nm := next.(Model); nm.currentSessionID != "s1" {
		t.Fatalf("currentSessionID = %q, want %q — the fixture switched after all",
			nm.currentSessionID, "s1")
	}

	if got := d.actions(); len(got) != 0 {
		t.Errorf("daemon saw %v, want no request — the pane is still on s1", got)
	}
}
