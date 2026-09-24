package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/task"
	"github.com/takaaki-s/jind-ai/internal/worktreehook"
)

func isolateOnboarding(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("JIN_SOCKET", "")

	oldLookPath := onboardingLookPath
	oldStart := onboardingStartDaemon
	oldWait := onboardingWaitDaemon
	oldCreate := onboardingCreateTask
	t.Cleanup(func() {
		onboardingLookPath = oldLookPath
		onboardingStartDaemon = oldStart
		onboardingWaitDaemon = oldWait
		onboardingCreateTask = oldCreate
	})
	onboardingLookPath = func(name string) (string, error) {
		switch name {
		case "git", "tmux", "claude", "gh":
			return "/test/bin/" + name, nil
		default:
			return "", exec.ErrNotFound
		}
	}
	return root
}

func newOnboardingFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	command := &cobra.Command{Use: "onboard"}
	command.Flags().Bool("confirm", false, "")
	command.Flags().Bool("skill", false, "")
	command.Flags().String("skill-dir", "", "")
	command.Flags().Bool("trust-hook", false, "")
	addTaskNewFlags(command)
	if err := command.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	return command
}

func TestOnboardingOptions_AgentAppliesWithoutTask(t *testing.T) {
	opts, err := onboardingOptionsFromCommand(newOnboardingFlagCommand(t, "--agent", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.AgentKind != "codex" || opts.Task != nil {
		t.Fatalf("options = %+v", opts)
	}
}

func TestOnboardingOptions_RejectsTaskOnlyFlagsWithoutSource(t *testing.T) {
	for _, args := range [][]string{{"--repo", "."}, {"--trust-hook"}, {"--no-hook"}} {
		if _, err := onboardingOptionsFromCommand(newOnboardingFlagCommand(t, args...)); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
}

func TestBuildOnboardingPlan_DryRunDoesNotWrite(t *testing.T) {
	root := isolateOnboarding(t)
	skillDir := filepath.Join(root, "chosen-skill")

	plan, _, err := buildOnboardingPlan(onboardingOptions{Skill: true, SkillDir: skillDir})
	if err != nil {
		t.Fatalf("buildOnboardingPlan: %v", err)
	}
	if !plan.Ready || plan.Mode != "plan" {
		t.Fatalf("plan = ready:%t mode:%q blockers:%v", plan.Ready, plan.Mode, plan.Blockers)
	}
	if len(plan.SkillPaths) != 1 || plan.SkillPaths[0] != filepath.Join(skillDir, "SKILL.md") {
		t.Fatalf("skill paths = %v", plan.SkillPaths)
	}
	for _, path := range []string{filepath.Join(root, "config"), filepath.Join(root, "state"), skillDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry run created %s: stat err = %v", path, err)
		}
	}
}

func TestBuildOnboardingPlan_ReportsMissingPrerequisite(t *testing.T) {
	isolateOnboarding(t)
	onboardingLookPath = func(name string) (string, error) {
		if name == "tmux" {
			return "", exec.ErrNotFound
		}
		return "/test/bin/" + name, nil
	}

	plan, _, err := buildOnboardingPlan(onboardingOptions{})
	if err != nil {
		t.Fatalf("buildOnboardingPlan: %v", err)
	}
	if plan.Ready || !strings.Contains(strings.Join(plan.Blockers, "\n"), "tmux executable not found") {
		t.Fatalf("plan ready=%t blockers=%v", plan.Ready, plan.Blockers)
	}
}

func TestApplyOnboardingPlan_CreatesOnlyPlannedSetup(t *testing.T) {
	root := isolateOnboarding(t)
	skillDir := filepath.Join(root, "chosen-skill")
	opts := onboardingOptions{Confirm: true, Skill: true, SkillDir: skillDir}
	plan, journal, err := buildOnboardingPlan(opts)
	if err != nil || !plan.Ready {
		t.Fatalf("build plan err=%v blockers=%v", err, plan.Blockers)
	}
	starts := 0
	onboardingStartDaemon = func() (bool, int, error) {
		starts++
		return true, 123, nil
	}
	onboardingWaitDaemon = func() error { return nil }

	if err := applyOnboardingPlan(&plan, opts, &journal); err != nil {
		t.Fatalf("applyOnboardingPlan: %v", err)
	}
	if starts != 1 {
		t.Fatalf("daemon starts = %d, want 1", starts)
	}
	for _, path := range []string{
		filepath.Join(root, "config", "jind-ai", "config.yaml"),
		filepath.Join(skillDir, "SKILL.md"),
		filepath.Join(root, "state", "jind-ai", onboardingJournalName),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("planned path %s missing: %v", path, err)
		}
	}
}

func TestCreateDefaultConfigRefusesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), path); err != nil {
		t.Fatal(err)
	}
	created, err := createDefaultConfig(path)
	if err == nil || created {
		t.Fatalf("createDefaultConfig created=%t err=%v", created, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was changed: info=%v err=%v", info, err)
	}
}

func TestOnboardingTaskJournalReusesKeyAndDoesNotStorePrompt(t *testing.T) {
	root := isolateOnboarding(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "init", "-q", repo)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	hookDir := filepath.Join(repo, ".jin")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "worktree-post-create.sh"), []byte("#!/bin/sh\necho setup\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := "Implement the first bounded change"
	newOptions := func() onboardingOptions {
		return onboardingOptions{Confirm: true, TrustHook: true, Task: &daemon.TaskNewRequest{Prompt: prompt, Repo: repo}}
	}

	opts := newOptions()
	plan, journal, err := buildOnboardingPlan(opts)
	if err != nil || !plan.Ready || plan.Task == nil {
		t.Fatalf("build plan err=%v blockers=%v task=%+v", err, plan.Blockers, plan.Task)
	}
	if plan.Hook.Status != "untrusted" || len(plan.Hook.content) == 0 {
		t.Fatalf("hook plan = %+v", plan.Hook)
	}
	var setupDisclosure string
	for _, check := range plan.Checks {
		if check.Name == "agent-setup" {
			setupDisclosure = check.Planned
		}
	}
	if !strings.Contains(setupDisclosure, ".claude.json") {
		t.Fatalf("agent setup did not disclose Claude trust write: %q", setupDisclosure)
	}
	firstKey := plan.Task.IdempotencyKey
	repeatedPlan, _, err := buildOnboardingPlan(newOptions())
	if err != nil || repeatedPlan.Task.IdempotencyKey != firstKey {
		t.Fatalf("repeated dry plan key = %q, want stable %q (err=%v)", repeatedPlan.Task.IdempotencyKey, firstKey, err)
	}
	starts := 0
	onboardingStartDaemon = func() (bool, int, error) {
		starts++
		return true, 123, nil
	}
	onboardingWaitDaemon = func() error { return nil }
	creates := 0
	onboardingCreateTask = func(req daemon.TaskNewRequest) (*daemon.TaskNewResponse, error) {
		creates++
		if req.IdempotencyKey != firstKey {
			t.Fatalf("task key = %q, want %q", req.IdempotencyKey, firstKey)
		}
		return &daemon.TaskNewResponse{Task: task.Info{ID: "task-first"}}, nil
	}
	if err := applyOnboardingPlan(&plan, opts, &journal); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	journalData, err := os.ReadFile(plan.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journalData), prompt) {
		t.Fatalf("journal persisted prompt body: %s", journalData)
	}
	allowlist, err := worktreehook.LoadAllowlist(getStateDir())
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := allowlist.Get(repo); !ok || entry.SHA256 != plan.Hook.SHA256 {
		t.Fatalf("hook trust entry = %+v, ok=%t; plan sha=%s", entry, ok, plan.Hook.SHA256)
	}

	secondOpts := newOptions()
	secondPlan, secondJournal, err := buildOnboardingPlan(secondOpts)
	if err != nil || !secondPlan.Ready {
		t.Fatalf("second plan err=%v blockers=%v", err, secondPlan.Blockers)
	}
	if secondPlan.Task.IdempotencyKey != firstKey || secondPlan.Task.ExistingTaskID != "task-first" {
		t.Fatalf("second task plan = %+v", secondPlan.Task)
	}
	if err := applyOnboardingPlan(&secondPlan, secondOpts, &secondJournal); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if creates != 1 {
		t.Fatalf("task create calls = %d, want 1", creates)
	}
	if starts != 2 {
		// The fake never creates a real socket, so each independent apply sees
		// the daemon as stopped. This count also proves the second task skip was
		// caused by the journal, not an early return before daemon handling.
		t.Fatalf("daemon starts = %d, want 2", starts)
	}
}

func TestBuildOnboardingPlan_DoesNotTrustHookWithoutOptIn(t *testing.T) {
	isolateOnboarding(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	hookDir := filepath.Join(repo, ".jin")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "worktree-post-create.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	plan, _, err := buildOnboardingPlan(onboardingOptions{Task: &daemon.TaskNewRequest{Prompt: "first", Repo: repo}})
	if err != nil || !plan.Ready {
		t.Fatalf("build plan err=%v blockers=%v", err, plan.Blockers)
	}
	if plan.Hook.Status != "untrusted" || plan.Hook.Planned != "" {
		t.Fatalf("hook plan = %+v", plan.Hook)
	}
	if _, err := os.Stat(filepath.Join(getStateDir(), "worktree-allowlist.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run wrote hook allowlist: %v", err)
	}
}

func TestOnboardingTaskJournalRejectsDifferentFirstTask(t *testing.T) {
	isolateOnboarding(t)
	journalPath := filepath.Join(getStateDir(), onboardingJournalName)
	journal := onboardingJournal{
		TaskFingerprint:    "sha256:other",
		TaskIdempotencyKey: "stable-key",
		TaskID:             "task-existing",
	}
	if err := saveOnboardingJournal(journalPath, &journal); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	plan, _, err := buildOnboardingPlan(onboardingOptions{Task: &daemon.TaskNewRequest{Prompt: "different", Repo: repo}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || !strings.Contains(strings.Join(plan.Blockers, "\n"), "different first task") {
		t.Fatalf("plan ready=%t blockers=%v", plan.Ready, plan.Blockers)
	}
}
