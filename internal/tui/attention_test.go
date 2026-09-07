package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func unseenInfo(id, fleet string) session.Info {
	return session.Info{
		ID: id, Description: id, Fleet: fleet, Status: session.StatusIdle,
		Attention: session.AttentionInfo{
			State: session.AttentionDone, Generation: 1, Unseen: true,
		},
	}
}

func seenInfo(id, fleet string) session.Info {
	return session.Info{
		ID: id, Description: id, Fleet: fleet, Status: session.StatusIdle,
		Attention: session.AttentionInfo{
			State: session.AttentionDone, Generation: 1, SeenGeneration: 1,
		},
	}
}

func plainInfo(id, fleet string) session.Info {
	return session.Info{ID: id, Description: id, Fleet: fleet, Status: session.StatusIdle}
}

func idsOf(sessions []session.Info) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

// The dot appears whatever the process is doing. The two axes are independent,
// and a session can be thinking again with a completion nobody acknowledged.
func TestRenderSession_UnseenDotOnEveryStatus(t *testing.T) {
	m := plainModel()
	const width = 40

	for _, status := range []session.Status{
		session.StatusCreating, session.StatusStopped, session.StatusRunning,
		session.StatusIdle, session.StatusThinking, session.StatusPermission,
		session.StatusDeleting,
	} {
		t.Run(string(status), func(t *testing.T) {
			sess := unseenInfo("s", session.DefaultFleet)
			sess.Status = status

			line := sessionRowLines(m.renderSession(sess, false, false, width))[0]
			if !strings.Contains(line, attentionGlyph) {
				t.Errorf("row = %q, want the unseen dot %q", stripANSI(line), attentionGlyph)
			}
		})
	}
}

// The cell is blank in exactly the two states that are not a pending
// completion, so the dot means one thing.
func TestRenderSession_NoDotWhenNothingIsUnseen(t *testing.T) {
	m := plainModel()
	const width = 40

	for name, sess := range map[string]session.Info{
		"never completed": plainInfo("s", session.DefaultFleet),
		"acknowledged":    seenInfo("s", session.DefaultFleet),
	} {
		t.Run(name, func(t *testing.T) {
			row := m.renderSession(sess, false, false, width)
			if strings.Contains(row, attentionGlyph) {
				t.Errorf("row = %q, want no unseen dot", stripANSI(row))
			}
		})
	}
}

// The dot may not move the name: the cell holds its two columns whether or not
// it draws anything, or every name shifts the moment a turn finishes.
func TestRenderSession_UnseenDotDoesNotMoveTheName(t *testing.T) {
	m := plainModel()
	const width = 40

	// The column, not the byte offset: the dot is three bytes wide and one
	// column, so a byte index would report the two as differing even when the
	// cell is doing its job.
	nameColumn := func(sess session.Info) int {
		t.Helper()
		sess.Description = "name"
		line := sessionRowLines(m.renderSession(sess, false, false, width))[0]
		idx := strings.Index(line, "name")
		if idx < 0 {
			t.Fatalf("name missing from %q", stripANSI(line))
		}
		return lipgloss.Width(line[:idx])
	}

	withDot := nameColumn(unseenInfo("s", session.DefaultFleet))
	without := nameColumn(plainInfo("s", session.DefaultFleet))
	if withDot != without {
		t.Errorf("name starts at column %d with the dot and %d without; the cell must be fixed width", withDot, without)
	}
	if withDot != sessionRowLead {
		t.Errorf("name starts at column %d, want sessionRowLead (%d)", withDot, sessionRowLead)
	}
}

// The row invariants the scroll and hit-test arithmetic rest on must survive
// the extra cell, in every style composition and at the widths around the new
// lead.
func TestRenderSession_UnseenKeepsRowGeometry(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{"del": true}}

	for _, width := range []int{30, 40, 80} {
		for _, selected := range []bool{false, true} {
			for _, viewed := range []bool{false, true} {
				for _, id := range []string{"s", "del"} {
					name := fmt.Sprintf("width=%d/selected=%v/viewed=%v/id=%s", width, selected, viewed, id)
					t.Run(name, func(t *testing.T) {
						sess := unseenInfo(id, session.DefaultFleet)
						sess.RepoName = "jind-ai"
						sess.CurrentBranch = "feat/x"

						row := m.renderSession(sess, selected, viewed, width)
						if n := strings.Count(row, "\n"); n != sessionRowHeight {
							t.Errorf("%d newlines, want %d: %q", n, sessionRowHeight, row)
						}
						for i, line := range sessionRowLines(row) {
							if w := lipgloss.Width(line); w != width {
								t.Errorf("line %d is %d columns, want exactly %d: %q", i, w, width, line)
							}
						}
					})
				}
			}
		}
	}

	t.Run("a width too narrow for the lead still costs its rows", func(t *testing.T) {
		sess := unseenInfo("s", session.DefaultFleet)
		for width := range sessionRowLead + 1 {
			row := m.renderSession(sess, true, true, width)
			if n := strings.Count(row, "\n"); n != sessionRowHeight {
				t.Errorf("width %d: %d newlines, want %d: %q", width, n, sessionRowHeight, row)
			}
			for i, line := range sessionRowLines(row) {
				if w := lipgloss.Width(line); w != max(width, 0) {
					t.Errorf("width %d: line %d is %d columns, want exactly %d", width, i, w, width)
				}
			}
		}
	})
}

// The second line's blank lead has to grow with the first line's, or the two
// lines of one row stop hanging under each other.
func TestRenderSession_SecondLineKeepsTheNewLead(t *testing.T) {
	m := plainModel()
	const width = 40
	sess := unseenInfo("s", session.DefaultFleet)
	sess.RepoName = "jind-ai"

	lines := sessionRowLines(m.renderSession(sess, false, false, width))
	idx := strings.Index(lines[1], "jind-ai")
	if idx < 0 {
		t.Fatalf("repo missing from %q", stripANSI(lines[1]))
	}
	if col := lipgloss.Width(lines[1][:idx]); col != sessionRowLead {
		t.Errorf("repo starts at column %d, want sessionRowLead (%d)", col, sessionRowLead)
	}
	// The dot is stated once, like the status icon.
	if strings.Contains(lines[1], attentionGlyph) {
		t.Errorf("line 2 = %q, want no unseen dot", stripANSI(lines[1]))
	}
}

func TestPartitionUnseenFirst(t *testing.T) {
	tests := []struct {
		name string
		in   []session.Info
		want []string
	}{
		{
			name: "empty",
			in:   nil,
			want: []string{},
		},
		{
			name: "nothing unseen keeps the daemon's order",
			in:   []session.Info{plainInfo("a", "f"), seenInfo("b", "f"), plainInfo("c", "f")},
			want: []string{"a", "b", "c"},
		},
		{
			name: "an unseen completion rises within its fleet",
			in:   []session.Info{plainInfo("a", "f"), unseenInfo("b", "f"), plainInfo("c", "f")},
			want: []string{"b", "a", "c"},
		},
		{
			name: "both halves keep their relative order",
			in: []session.Info{
				plainInfo("a", "f"), unseenInfo("b", "f"), plainInfo("c", "f"), unseenInfo("d", "f"),
			},
			want: []string{"b", "d", "a", "c"},
		},
		{
			name: "no session crosses a fleet boundary",
			in: []session.Info{
				plainInfo("a1", "alpha"), plainInfo("a2", "alpha"),
				unseenInfo("b1", "beta"), plainInfo("b2", "beta"),
			},
			want: []string{"a1", "a2", "b1", "b2"},
		},
		{
			name: "an unseen completion in a later fleet does not overtake an earlier fleet",
			in: []session.Info{
				plainInfo("a1", "alpha"),
				plainInfo("b1", "beta"), unseenInfo("b2", "beta"),
			},
			want: []string{"a1", "b2", "b1"},
		},
		{
			name: "the fleet order the daemon chose is preserved, default last included",
			in: []session.Info{
				plainInfo("z1", "zeta"), unseenInfo("z2", "zeta"),
				plainInfo("d1", session.DefaultFleet), unseenInfo("d2", session.DefaultFleet),
			},
			want: []string{"z2", "z1", "d2", "d1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := idsOf(partitionUnseenFirst(tt.in))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// The partition is a projection, so applying it twice changes nothing. That is
// what keeps a two-second poll from shuffling a settled list.
func TestPartitionUnseenFirst_IsStableAcrossPolls(t *testing.T) {
	in := []session.Info{
		plainInfo("a", "f"), unseenInfo("b", "f"), plainInfo("c", "f"), unseenInfo("d", "f"),
	}
	once := partitionUnseenFirst(in)
	twice := partitionUnseenFirst(once)

	for i := range once {
		if once[i].ID != twice[i].ID {
			t.Fatalf("second pass = %v, want the same as the first %v", idsOf(twice), idsOf(once))
		}
	}
}

// The poll installs the partitioned order, so a completion reorders the list
// under the cursor. The cursor follows the session it was on, not the index.
func TestSessionsMsg_CursorFollowsItsSessionAcrossAReorder(t *testing.T) {
	before := []session.Info{plainInfo("a", "f"), plainInfo("b", "f"), plainInfo("c", "f")}
	m := Model{
		sessions:    before,
		cursor:      0, // on "a"
		deletingIDs: map[string]bool{},
		height:      100,
	}

	// "c" finishes a turn and rises to the top of the fleet.
	next, _ := m.updateListMode(sessionsMsg([]session.Info{
		plainInfo("a", "f"), plainInfo("b", "f"), unseenInfo("c", "f"),
	}))
	nm := next.(Model)

	if got := idsOf(nm.sessions); got[0] != "c" {
		t.Fatalf("display order = %v, want the unseen completion first", got)
	}
	if nm.sessions[nm.cursor].ID != "a" {
		t.Errorf("cursor is on %q, want %q — it must follow the session, not the index",
			nm.sessions[nm.cursor].ID, "a")
	}
}

// The same guarantee in the other direction: the session under the cursor is
// the one that rises.
func TestSessionsMsg_CursorFollowsTheSessionThatRose(t *testing.T) {
	m := Model{
		sessions:    []session.Info{plainInfo("a", "f"), plainInfo("b", "f"), plainInfo("c", "f")},
		cursor:      2, // on "c"
		deletingIDs: map[string]bool{},
		height:      100,
	}

	next, _ := m.updateListMode(sessionsMsg([]session.Info{
		plainInfo("a", "f"), plainInfo("b", "f"), unseenInfo("c", "f"),
	}))
	nm := next.(Model)

	if nm.cursor != 0 {
		t.Errorf("cursor = %d, want 0 — %q rose to the top", nm.cursor, "c")
	}
	if nm.sessions[nm.cursor].ID != "c" {
		t.Errorf("cursor is on %q, want %q", nm.sessions[nm.cursor].ID, "c")
	}
}

// A session that disappeared between polls falls through to the pre-existing
// numeric clamp rather than leaving the cursor past the end of the list.
func TestSessionsMsg_CursorClampsWhenItsSessionDisappears(t *testing.T) {
	m := Model{
		sessions:    []session.Info{plainInfo("a", "f"), plainInfo("b", "f"), plainInfo("c", "f")},
		cursor:      2,
		deletingIDs: map[string]bool{},
		height:      100,
	}

	next, _ := m.updateListMode(sessionsMsg([]session.Info{plainInfo("a", "f")}))
	nm := next.(Model)

	if nm.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (clamped to the shortened list)", nm.cursor)
	}
}

// The startup restore keeps its precedence: it aims the cursor at the session
// the right pane was left showing, which is not necessarily the one the cursor
// index happened to name.
func TestSessionsMsg_StartupRestoreStillWins(t *testing.T) {
	m := Model{
		sessions:             []session.Info{plainInfo("a", "f"), plainInfo("b", "f")},
		cursor:               0,
		deletingIDs:          map[string]bool{},
		height:               100,
		currentSessionID:     "b",
		pendingCursorRestore: true,
	}

	next, _ := m.updateListMode(sessionsMsg([]session.Info{plainInfo("a", "f"), plainInfo("b", "f")}))
	nm := next.(Model)

	if nm.sessions[nm.cursor].ID != "b" {
		t.Errorf("cursor is on %q, want %q — the startup restore must win over the captured cursor",
			nm.sessions[nm.cursor].ID, "b")
	}
	if nm.pendingCursorRestore {
		t.Error("pendingCursorRestore is still set; it is a one-shot")
	}
}

// The list geometry is derived from the same slice the reorder installs, so a
// row's top, the mouse hit-test and the rendered rows must still agree after
// one.
func TestSessionsMsg_GeometryAgreesAfterAReorder(t *testing.T) {
	m := Model{
		sessions:    []session.Info{plainInfo("a", "f"), plainInfo("b", "f"), plainInfo("c", "f")},
		cursor:      0,
		deletingIDs: map[string]bool{},
		height:      100,
	}

	next, _ := m.updateListMode(sessionsMsg([]session.Info{
		plainInfo("a", "f"), plainInfo("b", "f"), unseenInfo("c", "f"),
	}))
	nm := next.(Model)

	for idx := range nm.getDisplaySessions() {
		top, height := nm.sessionCardTop(idx)
		if height != sessionRowHeight {
			t.Errorf("session %d has height %d, want %d", idx, height, sessionRowHeight)
		}
		wantTop := 1 + idx*sessionRowHeight // one fleet header, then the rows
		if top != wantTop {
			t.Errorf("sessionCardTop(%d) top = %d, want %d", idx, top, wantTop)
		}
		for line := top; line < top+height; line++ {
			got, ok := nm.sessionIndexAtLine(line)
			if !ok || got != idx {
				t.Errorf("sessionIndexAtLine(%d) = (%d, %v), want (%d, true)", line, got, ok, idx)
			}
		}
	}
}

// The palette is the only way to acknowledge from inside the TUI, so the
// dispatch is the whole of that path: an ID that falls through the switch is
// silently ignored (see dispatchAction), which would leave the action listed
// and inert.
func TestDispatchAction_MarkSeenAcknowledgesTheCursorSession(t *testing.T) {
	d, client := startFakeDaemon(t)
	m := Model{
		client:      client,
		sessions:    []session.Info{unseenInfo("s1", "f"), plainInfo("s2", "f")},
		cursor:      0,
		deletingIDs: map[string]bool{},
		height:      100,
	}

	next, cmd := m.dispatchAction(action.IDMarkSeen)
	nm := next.(Model)
	if nm.err != nil {
		t.Fatalf("err = %v, want nil", nm.err)
	}

	req := d.first(t)
	if req.Action != "attention-seen" {
		t.Errorf("action = %q, want %q", req.Action, "attention-seen")
	}
	var sent daemon.IDRequest
	if err := json.Unmarshal(req.Data, &sent); err != nil {
		t.Fatalf("unmarshal request data: %v", err)
	}
	if sent.ID != "s1" {
		t.Errorf("acknowledged %q, want the cursor session %q", sent.ID, "s1")
	}

	// The dot and the partition are both derived from the list, so the action
	// has to ask for a fresh one or the row stays as it was until the next poll.
	if cmd == nil {
		t.Error("no follow-up Cmd; the list would not refresh until the next poll")
	}
}

// Nothing under the cursor means nothing to acknowledge — no request, no error
// banner. A deleting row reports the same way (see sessionAt).
func TestDispatchAction_MarkSeenWithNoCursorSessionIsANoOp(t *testing.T) {
	d, client := startFakeDaemon(t)
	m := Model{
		client:      client,
		sessions:    nil,
		cursor:      0,
		deletingIDs: map[string]bool{},
		height:      100,
	}

	next, _ := m.dispatchAction(action.IDMarkSeen)
	if nm := next.(Model); nm.err != nil {
		t.Errorf("err = %v, want nil", nm.err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) != 0 {
		t.Errorf("%d requests reached the daemon, want 0", len(d.requests))
	}
}

// A daemon that refuses has to leave a trace: the palette closes on selection,
// so a swallowed error is a row whose dot simply never goes away.
func TestDispatchAction_MarkSeenSurfacesTheDaemonError(t *testing.T) {
	client := daemon.NewClient(filepath.Join(t.TempDir(), "absent.sock"))
	m := Model{
		client:      client,
		sessions:    []session.Info{unseenInfo("s1", "f")},
		cursor:      0,
		deletingIDs: map[string]bool{},
		height:      100,
	}

	next, cmd := m.dispatchAction(action.IDMarkSeen)
	nm := next.(Model)
	if nm.err == nil {
		t.Fatal("err = nil, want the daemon failure")
	}
	if !strings.Contains(nm.err.Error(), "mark seen") {
		t.Errorf("err = %v, want it to name the action", nm.err)
	}
	if cmd != nil {
		t.Error("a failed acknowledgement must not schedule a refresh")
	}
}

// --- explicit attach acknowledges, implicit paths do not ---

// The three env keys funnel into one focusSessionID, so the origin has to be
// carried with it or the TUI cannot tell the user picking a row from a plugin
// asking for a switch.
func TestHandleEnvTick_OnlyThePickerIsAnExplicitAttach(t *testing.T) {
	for _, tt := range []struct {
		key            string
		wantFromPicker bool
	}{
		{"JIN_CREATED_SESSION", false},
		{"JIN_NOTIFY_SESSION", false},
		{"JIN_FOCUS_SESSION", true},
	} {
		t.Run(tt.key, func(t *testing.T) {
			m := Model{sessions: nil, deletingIDs: map[string]bool{}, height: 100}
			next, _ := m.handleEnvTick(map[string]string{tt.key: "ghost"}, func(string) {})
			nm := next.(Model)

			if nm.focusSessionID != "ghost" {
				t.Fatalf("focusSessionID = %q, want %q", nm.focusSessionID, "ghost")
			}
			if nm.focusFromPicker != tt.wantFromPicker {
				t.Errorf("focusFromPicker = %v, want %v", nm.focusFromPicker, tt.wantFromPicker)
			}
		})
	}
}

// The loop lets a later key win the ID. The origin must win with it, or a
// picker selection arriving alongside a created-session id would be judged by
// the wrong half.
func TestConsumeEnvRequests_OriginFollowsTheWinningID(t *testing.T) {
	env := map[string]string{"JIN_CREATED_SESSION": "created", "JIN_FOCUS_SESSION": "picked"}
	req := consumeEnvRequests(func(k string) string { return env[k] })

	if req.focusSessionID != "picked" || !req.focusFromPicker {
		t.Errorf("got (%q, %v), want (\"picked\", true)", req.focusSessionID, req.focusFromPicker)
	}
}

// The slow path gives up on a focus target that never appeared. It has to drop
// the origin too — the next request reuses the same field.
func TestSessionsMsg_GivingUpOnFocusDropsTheOrigin(t *testing.T) {
	m := Model{
		sessions:        []session.Info{plainInfo("a", "f")},
		deletingIDs:     map[string]bool{},
		height:          100,
		focusSessionID:  "ghost",
		focusFromPicker: true,
	}

	next, _ := m.updateListMode(sessionsMsg([]session.Info{plainInfo("a", "f")}))
	nm := next.(Model)

	if nm.focusSessionID != "" || nm.focusFromPicker {
		t.Errorf("after giving up: (%q, %v), want (\"\", false)", nm.focusSessionID, nm.focusFromPicker)
	}
}

// handleSelectSession refuses at three different points, and acknowledging has
// to sit behind all of them: one case per guard, or moving the call above the
// wrong one goes unnoticed. (The attach that does happen is in the e2e file —
// whether one happened is not observable without a real pane.)
func TestHandleSelectSession_NoAttachNoAcknowledge(t *testing.T) {
	creating := unseenInfo("s1", "f")
	creating.Status = session.StatusCreating

	for _, tt := range []struct {
		name string
		sess session.Info
		del  map[string]bool
	}{
		{name: "no display pane to attach to", sess: unseenInfo("s1", "f")},
		{name: "a session still being created", sess: creating},
		{name: "a session on its way out", sess: unseenInfo("s1", "f"), del: map[string]bool{"s1": true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := startFakeDaemon(t)
			del := tt.del
			if del == nil {
				del = map[string]bool{}
			}
			m := Model{
				client: client, sessions: []session.Info{tt.sess},
				cursor: 0, deletingIDs: del, height: 100,
			}

			m.handleSelectSession()

			if got := d.actions(); len(got) != 0 {
				t.Errorf("daemon saw %v, want no request", got)
			}
		})
	}
}

// Same for the picker path: the switch is a no-op without a tmux client, and a
// switch that did not happen acknowledges nothing however explicit it was.
func TestResolveFocusSession_NoSwitchNoAcknowledge(t *testing.T) {
	d, client := startFakeDaemon(t)
	m := &Model{
		client: client, sessions: []session.Info{unseenInfo("s1", "f")},
		deletingIDs: map[string]bool{}, height: 100,
		focusSessionID: "s1", focusFromPicker: true,
	}

	resolved, cmd := m.resolveFocusSession()
	if !resolved {
		t.Fatal("resolveFocusSession() = false, want true (target present)")
	}
	if cmd != nil {
		t.Error("a switch that did not happen returned a refresh Cmd")
	}
	if got := d.actions(); len(got) != 0 {
		t.Errorf("daemon saw %v, want no request", got)
	}
	if m.focusSessionID != "" || m.focusFromPicker {
		t.Errorf("pending focus not cleared: (%q, %v)", m.focusSessionID, m.focusFromPicker)
	}
}
