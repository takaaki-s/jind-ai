package tui

import (
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestDeleteFailureClearsGreyoutForRestoredStatus(t *testing.T) {
	for _, status := range []session.Status{
		session.StatusIdle, session.StatusRunning, session.StatusThinking,
		session.StatusPermission, session.StatusStopped, session.StatusCreating,
	} {
		t.Run(string(status), func(t *testing.T) {
			m := plainModel()
			m.deletingIDs["s1"] = true
			live := session.Info{ID: "s1", Description: "large repo", Status: status, ErrorMessage: "git worktree remove: permission denied"}
			updated, _ := m.Update(sessionsMsg{live})
			got := updated.(Model)
			if got.isDeleting(live) {
				t.Fatal("failed deletion remains greyed out")
			}
			if got.sessions[0].Status != status {
				t.Fatal("TUI changed restored daemon status")
			}
			if got.err == nil || !strings.Contains(got.err.Error(), live.ErrorMessage) {
				t.Fatalf("missing deletion error: %v", got.err)
			}
		})
	}
}

func TestDeletePollingKeepsInFlightGreyout(t *testing.T) {
	for _, live := range []session.Info{
		{ID: "s1", Status: session.StatusDeleting},
		{ID: "s1", Status: session.StatusDeleting, ErrorMessage: "previous error"},
		{ID: "s1", Status: session.StatusIdle}, // A poll can arrive before acceptance.
	} {
		m := plainModel()
		m.deletingIDs["s1"] = true
		updated, _ := m.Update(sessionsMsg{live})
		if !updated.(Model).isDeleting(live) {
			t.Fatalf("cleared in-flight deletion: %+v", live)
		}
	}
}
