package session

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/git"
)

const testReviewBaseOID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func isTestResolveCommit(args []string) bool {
	return len(args) == 5 &&
		args[0] == "rev-parse" &&
		args[1] == "--verify" &&
		args[2] == "--quiet" &&
		args[3] == "--end-of-options" &&
		args[4] == "origin/main^{commit}"
}

func TestReserveCreation_RecordsWhyReviewBaseIsUnavailable(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, info, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	want := ReviewBase{UnavailableReason: ReviewBaseUnavailableNotManagedWorktree}
	if sess.ReviewBase != want {
		t.Errorf("Session.ReviewBase = %+v, want %+v", sess.ReviewBase, want)
	}
	if info.ReviewBase != want {
		t.Errorf("Info.ReviewBase = %+v, want %+v", info.ReviewBase, want)
	}

	loaded, err := mgr.store.Load(sess.ID)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if loaded.ReviewBase != want {
		t.Errorf("persisted ReviewBase = %+v, want %+v", loaded.ReviewBase, want)
	}
}

func TestCreateWithOptions_PersistsResolvedReviewBase(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	runner := &scriptedGitRunner{handler: func(_ string, args []string) ([]byte, error) {
		switch {
		case strings.Join(args, " ") == "symbolic-ref refs/remotes/origin/HEAD":
			return []byte("refs/remotes/origin/main\n"), nil
		case isTestResolveCommit(args):
			return []byte(testReviewBaseOID + "\n"), nil
		case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
			return nil, nil
		case len(args) >= 1 && args[0] == "rev-parse":
			return nil, errors.New("exit status 1") // candidate branch is free
		case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
			return nil, nil
		}
		return nil, errors.New("unexpected git call: " + strings.Join(args, " "))
	}}
	mgr.SetGitClient(git.NewClientWithRunner(runner))

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: repo, Worktree: true})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	want := ReviewBase{RequestedRef: "origin/main", CommitOID: testReviewBaseOID, WorktreePath: sess.WorkDir}
	if sess.ReviewBase != want {
		t.Errorf("Session.ReviewBase = %+v, want %+v", sess.ReviewBase, want)
	}
	info, ok := mgr.GetInfo(sess.ID)
	if !ok {
		t.Fatal("GetInfo missed session")
	}
	if info.ReviewBase != want {
		t.Errorf("Info.ReviewBase = %+v, want %+v", info.ReviewBase, want)
	}
	add := runner.findCall("worktree", "add")
	if len(add) != 6 || add[5] != testReviewBaseOID {
		t.Fatalf("worktree add args = %v, want resolved OID as base", add)
	}
}

func TestCreateWithOptions_InvalidReviewBaseLeavesNoSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	runner := &scriptedGitRunner{handler: func(_ string, args []string) ([]byte, error) {
		switch {
		case strings.Join(args, " ") == "symbolic-ref refs/remotes/origin/HEAD":
			return []byte("refs/remotes/origin/main\n"), nil
		case isTestResolveCommit(args):
			return nil, errors.New("exit status 1")
		}
		return nil, errors.New("unexpected git call: " + strings.Join(args, " "))
	}}
	mgr.SetGitClient(git.NewClientWithRunner(runner))

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: repo, Worktree: true})
	if err == nil || !strings.Contains(err.Error(), "resolving worktree base") {
		t.Fatalf("CreateWithOptions error = %v, want review-base resolution failure", err)
	}
	if runner.hadCall("worktree", "add") {
		t.Fatal("worktree add ran after base resolution failed")
	}
	if got := mgr.List(); len(got) != 0 {
		t.Fatalf("sessions after failed synchronous create = %d, want 0", len(got))
	}
}

func TestStoreSave_ReviewBaseCannotBeOverwrittenByStaleSnapshot(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	original := Session{
		ID:         "immutable-base",
		ReviewBase: ReviewBase{RequestedRef: "origin/main", CommitOID: testReviewBaseOID},
	}
	if err := store.Save(original); err != nil {
		t.Fatalf("Save original: %v", err)
	}
	stale := original
	stale.ReviewBase = ReviewBase{
		RequestedRef: "origin/main",
		CommitOID:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	if err := store.Save(stale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}
	loaded, err := store.Load(original.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ReviewBase != original.ReviewBase {
		t.Errorf("ReviewBase = %+v, want first persisted value %+v", loaded.ReviewBase, original.ReviewBase)
	}
}

func TestManagedWorktree_ReviewBaseSurvivesRefMovementAndRestart(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runReviewBaseGit(t, repo, "init")
	runReviewBaseGit(t, repo, "config", "user.email", "test@example.com")
	runReviewBaseGit(t, repo, "config", "user.name", "Test User")
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	runReviewBaseGit(t, repo, "add", "tracked.txt")
	runReviewBaseGit(t, repo, "commit", "-m", "base")
	runReviewBaseGit(t, repo, "branch", "-M", "main")
	baseOID := runReviewBaseGit(t, repo, "rev-parse", "HEAD")
	runReviewBaseGit(t, repo, "update-ref", "refs/remotes/origin/main", baseOID)

	configMgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	sessionsDir := t.TempDir()
	stateDir := t.TempDir()
	mgr, err := NewManager(sessionsDir, stateDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:      repo,
		Worktree:     true,
		WorktreeBase: "main",
		NoHook:       true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	t.Cleanup(func() {
		cmd := exec.Command("git", "-C", repo, "worktree", "remove", "--force", sess.WorkDir)
		_ = cmd.Run()
	})
	if sess.ReviewBase.CommitOID != baseOID {
		t.Fatalf("ReviewBase.CommitOID = %q, want initial base %q", sess.ReviewBase.CommitOID, baseOID)
	}
	if sess.ReviewBase.WorktreePath != sess.WorkDir {
		t.Fatalf("ReviewBase.WorktreePath = %q, want %q", sess.ReviewBase.WorktreePath, sess.WorkDir)
	}
	if got := runReviewBaseGit(t, sess.WorkDir, "rev-parse", "HEAD"); got != baseOID {
		t.Fatalf("worktree HEAD = %q, want persisted review base %q", got, baseOID)
	}

	if err := os.WriteFile(tracked, []byte("moved\n"), 0o644); err != nil {
		t.Fatalf("move tracked file: %v", err)
	}
	runReviewBaseGit(t, repo, "add", "tracked.txt")
	runReviewBaseGit(t, repo, "commit", "-m", "move base ref")
	movedOID := runReviewBaseGit(t, repo, "rev-parse", "HEAD")
	runReviewBaseGit(t, repo, "update-ref", "refs/remotes/origin/main", movedOID)
	if movedOID == baseOID {
		t.Fatal("fixture did not move origin/main")
	}

	// A normal lifecycle save after the ref moved must keep the old OID.
	mgr.SetStatus(sess.ID, StatusIdle)
	restarted, err := NewManager(sessionsDir, stateDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	got, ok := restarted.GetInfo(sess.ID)
	if !ok {
		t.Fatal("restarted manager lost session")
	}
	if got.ReviewBase.CommitOID != baseOID {
		t.Errorf("ReviewBase after ref movement/restart = %q, want %q", got.ReviewBase.CommitOID, baseOID)
	}
	if got.ReviewBase.WorktreePath != sess.WorkDir {
		t.Errorf("ReviewBase path after restart = %q, want %q", got.ReviewBase.WorktreePath, sess.WorkDir)
	}
	// Resume and recovery paths save Session snapshots through the same Store
	// invariant. Exercise another post-restart mutation and load once more.
	restarted.SetStatus(sess.ID, StatusRunning)
	again, err := NewManager(sessionsDir, stateDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("second NewManager: %v", err)
	}
	got, ok = again.GetInfo(sess.ID)
	if !ok || got.ReviewBase.CommitOID != baseOID {
		t.Fatalf("ReviewBase after post-restart lifecycle save = %+v, want OID %q", got.ReviewBase, baseOID)
	}
}

func runReviewBaseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestStoreLoad_LegacyJSONKeepsUnknownReviewBaseZero(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const id = "legacy-review-base"
	raw := []byte(`{"id":"legacy-review-base","description":"old","work_dir":"/tmp/old","status":"stopped","agent_kind":"claude","agent_session_id_confirmed":false,"fleet":"default"}`)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.ReviewBase.IsZero() {
		t.Fatalf("legacy ReviewBase = %+v, want zero/unknown", loaded.ReviewBase)
	}
	if err := store.Save(*loaded); err != nil {
		t.Fatalf("Save legacy: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal saved JSON: %v", err)
	}
	if _, exists := doc["review_base"]; exists {
		t.Fatalf("legacy save synthesized review_base: %s", data)
	}
}
