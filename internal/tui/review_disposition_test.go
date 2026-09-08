package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestReviewDispositionActions_SendDecision(t *testing.T) {
	for _, tt := range []struct {
		name     string
		actionID string
		want     session.ReviewDecision
	}{
		{name: "reviewed", actionID: action.IDMarkReviewed, want: session.ReviewDecisionReviewed},
		{name: "changes requested", actionID: action.IDRequestChanges, want: session.ReviewDecisionChangesRequested},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := startFakeDaemon(t)
			m := NewModel(client)
			m.sessions = []session.Info{{ID: "s1", Description: "review task"}}
			m.cursor = 0

			next, cmd := m.dispatchAction(tt.actionID)
			if cmd == nil {
				t.Fatal("review action returned no command")
			}
			msg := cmd()
			completed, refresh := next.(Model).Update(msg)
			if completed.(Model).err != nil {
				t.Fatalf("review action error = %v", completed.(Model).err)
			}
			if refresh == nil {
				t.Fatal("successful review action returned no refresh command")
			}
			reqOnWire := d.first(t)
			if reqOnWire.Action != "review-disposition" {
				t.Fatalf("request = %+v", reqOnWire)
			}
			var req daemon.ReviewDispositionRequest
			if err := json.Unmarshal(reqOnWire.Data, &req); err != nil {
				t.Fatal(err)
			}
			if req.ID != "s1" || req.Decision != tt.want {
				t.Fatalf("request = %+v", req)
			}
		})
	}
}

func TestRenderDetailPane_ShowsReviewDisposition(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stale bool
		want  string
	}{
		{name: "current", want: "reviewed"},
		{name: "stale", stale: true, want: "reviewed (stale)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sess := session.Info{
				ID: "s1", Description: "review task", Status: session.StatusIdle,
				ReviewDisposition: session.ReviewDispositionInfo{Decision: session.ReviewDecisionReviewed, Stale: tt.stale},
			}
			pane := plainModel().renderDetailPane(sess, 48)
			if !strings.Contains(stripANSI(pane), tt.want) {
				t.Fatalf("detail pane = %q, want %q", stripANSI(pane), tt.want)
			}
		})
	}
}
