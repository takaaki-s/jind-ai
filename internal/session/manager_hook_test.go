package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/git"
	"github.com/takaaki-s/jindaiko/internal/worktreehook"
)

// hookHappyPathGitRunner returns a scriptedGitRunner wired for the success
// path of CreateWithOptions with Worktree=true (no rollback expected).
func hookHappyPathGitRunner() *scriptedGitRunner {
	return &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				return nil, errors.New("exit status 1")
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
}

// setupHookTest builds a Manager configured for worktree hook tests.
// It pins XDG_STATE_HOME to a temp dir, initializes an empty .git in workDir,
// and wires the given scripted git runner into the manager.
func setupHookTest(t *testing.T, gitRunner *scriptedGitRunner) (mgr *Manager, hookMock *mockHookRunner, workDir string) {
	t.Helper()
	mgr, _, hookMock = newTestManager(t)

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	workDir = t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	mgr.gitClient = git.NewClientWithRunner(gitRunner)
	return mgr, hookMock, workDir
}

// TestCreateWithOptions_HookNoScript: Discover returns exists=false → the
// hook path is skipped and Verify/Run are never called.
func TestCreateWithOptions_HookNoScript(t *testing.T) {
	mgr, hookMock, workDir := setupHookTest(t, hookHappyPathGitRunner())
	// discoverExists left empty → Discover returns exists=false

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "no-script",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}

	if !hookMock.hasCalledWith("Discover", workDir) {
		t.Error("expected Discover to be called with the original repo dir")
	}
	if hookMock.callCount("Verify") != 0 {
		t.Errorf("Verify calls = %d, want 0", hookMock.callCount("Verify"))
	}
	if hookMock.callCount("Run") != 0 {
		t.Errorf("Run calls = %d, want 0", hookMock.callCount("Run"))
	}
}

// TestCreateWithOptions_HookOK: Verdict OK → Run is invoked with the
// discovered script path and hook log path.
func TestCreateWithOptions_HookOK(t *testing.T) {
	mgr, hookMock, workDir := setupHookTest(t, hookHappyPathGitRunner())
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	hookMock.discoverExists[workDir] = true
	hookMock.verdictFor[scriptPath] = worktreehook.VerdictOK

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "hook-ok",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}

	if !hookMock.hasCalledWith("Run", scriptPath) {
		t.Errorf("expected Run to be called with %q; recorded calls: %+v", scriptPath, hookMock.calls)
	}
}

// TestCreateWithOptions_HookNotAllowed: NotAllowed → Run is skipped but the
// session is still created (no rollback).
func TestCreateWithOptions_HookNotAllowed(t *testing.T) {
	mgr, hookMock, workDir := setupHookTest(t, hookHappyPathGitRunner())
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	hookMock.discoverExists[workDir] = true
	hookMock.verdictFor[scriptPath] = worktreehook.VerdictNotAllowed

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "hook-notallowed",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if hookMock.callCount("Run") != 0 {
		t.Errorf("Run calls = %d, want 0", hookMock.callCount("Run"))
	}
}

// TestCreateWithOptions_HookChanged: Changed → Run is skipped, session
// created (same as NotAllowed).
func TestCreateWithOptions_HookChanged(t *testing.T) {
	mgr, hookMock, workDir := setupHookTest(t, hookHappyPathGitRunner())
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	hookMock.discoverExists[workDir] = true
	hookMock.verdictFor[scriptPath] = worktreehook.VerdictChanged

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "hook-changed",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if hookMock.callCount("Run") != 0 {
		t.Errorf("Run calls = %d, want 0", hookMock.callCount("Run"))
	}
}

// TestCreateWithOptions_HookFail: Run returns error → CreateWithOptions
// fails and the rollback defer removes the worktree/branch via git mock.
// The runner is inlined here (not extracted like hookHappyPathGitRunner)
// because only this test needs the AddWorktree side effect + rollback
// handlers; the extra layout is what makes rollback observable.
func TestCreateWithOptions_HookFail(t *testing.T) {
	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				return nil, errors.New("exit status 1")
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
				worktreePath := args[4]
				mainGitDir := filepath.Join(dir, ".git", "worktrees", filepath.Base(worktreePath))
				if err := os.MkdirAll(mainGitDir, 0o755); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(worktreePath, 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(
					filepath.Join(worktreePath, ".git"),
					[]byte("gitdir: "+mainGitDir+"\n"),
					0o644,
				); err != nil {
					return nil, err
				}
				return nil, nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
				_ = os.RemoveAll(args[len(args)-1])
				return nil, nil
			case len(args) >= 2 && args[0] == "branch" && args[1] == "-D":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
	mgr, hookMock, workDir := setupHookTest(t, runner)
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	hookMock.discoverExists[workDir] = true
	hookMock.verdictFor[scriptPath] = worktreehook.VerdictOK
	hookMock.runErr = fmt.Errorf("exit status 1")

	_, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "hook-fail",
		Worktree:    true,
	})
	if err == nil {
		t.Fatal("expected error from hook failure, got nil")
	}
	if !strings.Contains(err.Error(), "worktree post-create hook failed") {
		t.Errorf("error %q should mention hook failure", err.Error())
	}

	if !runner.hadCall("worktree", "add") {
		t.Error("expected AddWorktree runner call")
	}
	if !runner.hadCall("worktree", "remove") {
		t.Error("expected RemoveWorktree runner call during rollback")
	}
	if !runner.hadCall("branch", "-D") {
		t.Error("expected DeleteBranch runner call during rollback")
	}
}

// TestCreateWithOptions_NoHookFlag: CreateOptions.NoHook=true → hook path is
// skipped entirely (Discover never called).
func TestCreateWithOptions_NoHookFlag(t *testing.T) {
	mgr, hookMock, workDir := setupHookTest(t, hookHappyPathGitRunner())
	hookMock.discoverExists[workDir] = true
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	hookMock.verdictFor[scriptPath] = worktreehook.VerdictOK

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "no-hook-flag",
		Worktree:    true,
		NoHook:      true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if hookMock.callCount("Discover") != 0 {
		t.Errorf("Discover calls = %d, want 0", hookMock.callCount("Discover"))
	}
	if hookMock.callCount("Run") != 0 {
		t.Errorf("Run calls = %d, want 0", hookMock.callCount("Run"))
	}
}

// TestCreateWithOptions_HookDisabledConfig: config.HookEnabled=&false →
// hook path is skipped, session created successfully.
func TestCreateWithOptions_HookDisabledConfig(t *testing.T) {
	mgr, hookMock, workDir := setupHookTest(t, hookHappyPathGitRunner())
	hookMock.discoverExists[workDir] = true
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	hookMock.verdictFor[scriptPath] = worktreehook.VerdictOK

	// Swap in a fresh config.Manager that reads a config.yaml with
	// hook_enabled: false so GetWorktreeConfig() reports the feature as
	// disabled. Writing the file BEFORE NewManager ensures the load path
	// (not just Reload) picks up the value.
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("worktree:\n  hook_enabled: false\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	newCfg, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	mgr.configMgr = newCfg

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "hook-disabled-cfg",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if hookMock.callCount("Discover") != 0 {
		t.Errorf("Discover calls = %d, want 0", hookMock.callCount("Discover"))
	}
	if hookMock.callCount("Run") != 0 {
		t.Errorf("Run calls = %d, want 0", hookMock.callCount("Run"))
	}
}

// TestCreateWithOptions_HookNilRunner: no hook runner installed → hook path
// is skipped, worktree session created.
func TestCreateWithOptions_HookNilRunner(t *testing.T) {
	mgr, _, workDir := setupHookTest(t, hookHappyPathGitRunner())
	mgr.SetHookRunner(nil)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "hook-nil",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
}
