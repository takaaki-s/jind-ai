//go:build e2e

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func taskInboxProviderCalls(t *testing.T, f *taskInboxE2EFixture, name string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.pluginsDir, name, "calls.log"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "call\n")
}

func assertTaskInboxGateRejects(t *testing.T, f *taskInboxE2EFixture, info session.Info, gate, action, key string) {
	t.Helper()
	prCalls, mergeCalls := taskInboxProviderCalls(t, f, "e2e-pr"), taskInboxProviderCalls(t, f, "e2e-merge")
	wantError := "current reviewed disposition"
	switch gate {
	case "failed-checks":
		wantError = "reported checks failed"
		failed := taskInboxCall[session.Info](t, f.server, "check-report", CheckReportRequest{ID: info.ID, Status: session.CheckStatusFailed})
		if !failed.Attention.Unseen || failed.Attention.State != session.AttentionChecksFailed {
			t.Fatal("new failure did not reopen attention")
		}
	case "changes-requested":
		taskInboxRequireSuccess(t, f.server, "review-disposition", ReviewDispositionRequest{ID: info.ID, Decision: session.ReviewDecisionChangesRequested})
	case "stale-head", "dirty-worktree":
		if err := os.WriteFile(filepath.Join(info.WorkDir, "later.txt"), []byte("later change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if gate == "stale-head" {
			runTaskInboxGit(t, info.WorkDir, "add", "later.txt")
			runTaskInboxGit(t, info.WorkDir, "commit", "-m", "later change")
		} else {
			// Current claims reach the PR cleanliness guard. For merge, the
			// previous PR receipt is now stale and must already block dispatch.
			taskInboxRequireSuccess(t, f.server, "check-report", CheckReportRequest{ID: info.ID, Status: session.CheckStatusPassed})
			taskInboxRequireSuccess(t, f.server, "review-disposition", ReviewDispositionRequest{ID: info.ID, Decision: session.ReviewDecisionReviewed})
			wantError = "uncommitted changes"
			if action == "merge-handoff" {
				wantError = "current successful PR handoff"
			}
		}
	default:
		t.Fatalf("unknown gate %q", gate)
	}
	for _, confirm := range []bool{false, true} {
		var request any
		if action == "pr-handoff" {
			request = PRHandoffRequest{ID: info.ID, Plugin: "e2e-pr", Action: "create", DryRun: !confirm, Confirm: confirm, IdempotencyKey: key}
		} else {
			request = MergeHandoffRequest{ID: info.ID, Plugin: "e2e-merge", Action: "merge", DryRun: !confirm, Confirm: confirm, IdempotencyKey: key}
		}
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		response := f.server.handleRequest(&Request{Action: action, Data: data})
		if response.Success || response.Error == "" {
			t.Fatalf("%s accepted %s (confirm=%t): %+v", action, gate, confirm, response)
		}
		if !strings.Contains(response.Error, wantError) {
			t.Fatalf("wrong rejection: %s (want %q)", response.Error, wantError)
		}
	}
	if taskInboxProviderCalls(t, f, "e2e-pr") != prCalls || taskInboxProviderCalls(t, f, "e2e-merge") != mergeCalls {
		t.Fatal("rejected handoff invoked a provider")
	}
	current, ok := f.manager.GetInfo(info.ID)
	if !ok || !current.MergeHandoff.IsZero() {
		t.Fatalf("rejected handoff changed merge state: %+v", current.MergeHandoff)
	}
	if action == "pr-handoff" && !current.PRHandoff.IsZero() {
		t.Fatal("rejected PR handoff recorded a mutation")
	}
}
