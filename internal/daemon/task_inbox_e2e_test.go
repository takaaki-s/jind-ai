//go:build e2e

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/agent/agenttest"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/internal/provider"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

type taskInboxE2EDriver struct {
	manager *session.Manager
	mu      sync.Mutex
	prompt  string
}

func (d *taskInboxE2EDriver) Reserve(opts session.CreateOptions) (session.Info, error) {
	_, info, err := d.manager.ReserveCreation(opts)
	return info, err
}

func (d *taskInboxE2EDriver) Provision(id string, opts session.CreateOptions) (string, error) {
	return d.manager.ProvisionReserved(id, opts)
}

func (d *taskInboxE2EDriver) SetInitialWorkDir(id, relative string) error {
	return d.manager.SetInitialWorkDir(id, relative)
}

func (d *taskInboxE2EDriver) Start(id string) error {
	d.manager.SetStatus(id, session.StatusIdle)
	return nil
}

func (d *taskInboxE2EDriver) Get(id string) (session.Info, bool) {
	return d.manager.GetInfo(id)
}

func (d *taskInboxE2EDriver) Submit(id, prompt string) error {
	d.mu.Lock()
	d.prompt = prompt
	d.mu.Unlock()
	d.manager.SetStatus(id, session.StatusThinking)
	return nil
}

func (d *taskInboxE2EDriver) MarkCreationFailed(id string, err error) {
	d.manager.MarkCreationFailed(id, err)
}

func (d *taskInboxE2EDriver) SetCreationWarning(id, warning string) {
	d.manager.SetCreationWarning(id, warning)
}

func (d *taskInboxE2EDriver) submittedPrompt() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.prompt
}

type taskInboxE2EAgentResolver struct{ agent session.Agent }

func (r taskInboxE2EAgentResolver) Resolve(kind string) (session.Agent, error) {
	if r.agent != nil && r.agent.Kind() == kind {
		return r.agent, nil
	}
	return nil, fmt.Errorf("agent %q is unavailable", kind)
}

type taskInboxE2EFixture struct {
	server      *Server
	manager     *session.Manager
	taskManager *task.Manager
	driver      *taskInboxE2EDriver
	config      *config.Manager
	repo        string
	sessionsDir string
	stateDir    string
	tasksDir    string
	pluginsDir  string
	baseCommit  string
}

func newTaskInboxE2EFixture(t *testing.T) *taskInboxE2EFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTaskInboxGit(t, repo, "init", "-b", "main")
	runTaskInboxGit(t, repo, "config", "user.email", "test@example.com")
	runTaskInboxGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTaskInboxGit(t, repo, "add", "README.md")
	runTaskInboxGit(t, repo, "commit", "-m", "base")
	base := taskInboxGitOutput(t, repo, "rev-parse", "HEAD")
	runTaskInboxGit(t, repo, "update-ref", "refs/remotes/origin/main", base)

	configDir := filepath.Join(root, "config")
	sessionsDir := filepath.Join(root, "sessions")
	stateDir := filepath.Join(root, "state")
	tasksDir := filepath.Join(stateDir, "tasks")
	pluginsDir := filepath.Join(root, "plugins")
	configManager, err := config.NewManager(configDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := jinenv.Identity{
		SocketPath: filepath.Join(root, "daemon.sock"),
		BinPath:    filepath.Join(root, "bin", "jin"),
	}
	manager, err := session.NewManager(sessionsDir, stateDir, identity, configManager)
	if err != nil {
		t.Fatal(err)
	}
	agent := &agenttest.StubAgent{
		KindStr: "e2e",
		InterpretFn: func(signal session.StatusSignal) (session.StatusUpdate, bool) {
			if signal.Kind == "hook" && signal.Payload["event"] == "TurnCompleted" {
				return session.StatusUpdate{Status: session.StatusIdle, Notify: session.NotifyTaskComplete}, true
			}
			return session.StatusUpdate{}, false
		},
	}
	manager.SetAgentResolver(taskInboxE2EAgentResolver{agent: agent})
	driver := &taskInboxE2EDriver{manager: manager}
	taskManager, err := task.NewManager(tasksDir, manager)
	if err != nil {
		t.Fatal(err)
	}
	registry := plugin.NewRegistry(pluginsDir, stateDir, config.PluginsConfig{})
	dispatcher := plugin.NewDispatcher(registry, pluginsDir, stateDir, identity, time.Millisecond, nil)
	server := &Server{
		manager: manager, taskManager: taskManager, taskDriver: driver,
		taskAgentValidator: func(kind string) error {
			if kind != agent.Kind() {
				return fmt.Errorf("agent %q is unavailable", kind)
			}
			return nil
		},
		configMgr: configManager, pluginDisp: dispatcher,
	}
	return &taskInboxE2EFixture{
		server: server, manager: manager, taskManager: taskManager, driver: driver,
		config: configManager, repo: repo, sessionsDir: sessionsDir, stateDir: stateDir,
		tasksDir: tasksDir, pluginsDir: pluginsDir, baseCommit: base,
	}
}

func TestE2E_TaskInboxPromptCreatesIsolatedExecution(t *testing.T) {
	f := newTaskInboxE2EFixture(t)
	prompt := "Implement the prompt-backed change without retaining this body."
	created := taskInboxTaskNew(t, f.server, TaskNewRequest{
		IdempotencyKey: "prompt-e2e", Title: "Prompt task", Prompt: prompt,
		Repo: f.repo, RequestedBase: "main", AgentKind: "e2e", NoHook: true,
	})
	settled := waitForTaskRunPhase(t, f.taskManager, created.Task.ID, task.ExecutionSubmitted)
	info, ok := f.manager.GetInfo(created.Session.ID)
	if !ok {
		t.Fatal("orchestrated session is missing")
	}
	t.Cleanup(func() { removeTaskInboxWorktree(f.repo, info.WorkDir, info.ReviewFacts.Branch) })
	if info.WorkDir == f.repo || info.ReviewBase.WorktreePath != info.WorkDir || info.ReviewBase.CommitOID != f.baseCommit {
		t.Fatalf("isolation evidence = %+v", info)
	}
	if stat, err := os.Stat(filepath.Join(info.WorkDir, ".git")); err != nil || !stat.Mode().IsRegular() {
		t.Fatalf("managed worktree marker: %v", err)
	}
	if got := f.driver.submittedPrompt(); got != prompt {
		t.Fatalf("submitted prompt = %q", got)
	}
	if settled.Source.Kind != "prompt" || settled.Source.Ref != "sha256:"+task.PromptDigest(prompt) {
		t.Fatalf("prompt source = %+v", settled.Source)
	}
	assertTaskInboxStateOmits(t, f.tasksDir, prompt)
}

func TestE2E_TaskInboxIssueToMergeCleanupSurvivesRestart(t *testing.T) {
	f := newTaskInboxE2EFixture(t)
	ref, err := provider.ParseGitHubIssueReference("acme/demo#7")
	if err != nil {
		t.Fatal(err)
	}
	issueBody := "private issue body marker: implement the bounded change"
	f.server.issueReader = &fakeIssueReader{issue: provider.Issue{
		Ref: ref, Title: "Issue-backed task", Body: issueBody, Labels: []string{"bug"},
		State: "open", SyncToken: "2026-09-14T00:00:00Z",
	}}
	comments := &fakeIssueComments{
		inspection: provider.IssueCommentInspection{Actor: "octocat"},
		created: provider.IssueComment{
			Provider: "github", ID: "91", URL: ref.URL + "#issuecomment-91", Actor: "octocat",
		},
	}
	f.server.issueCommentReader = comments
	f.server.issueCommentWriter = comments

	created := taskInboxTaskNew(t, f.server, TaskNewRequest{
		IdempotencyKey: "issue-e2e", Issue: ref.URL, Repo: f.repo,
		RequestedBase: "main", AgentKind: "e2e", NoHook: true,
	})
	settled := waitForTaskRunPhase(t, f.taskManager, created.Task.ID, task.ExecutionSubmitted)
	info, ok := f.manager.GetInfo(created.Session.ID)
	if !ok {
		t.Fatal("orchestrated session is missing")
	}
	t.Cleanup(func() { removeTaskInboxWorktree(f.repo, info.WorkDir, info.ReviewFacts.Branch) })
	if settled.Source.Provider != "github" || settled.Source.Repository != "acme/demo" || settled.Source.ExternalID != "7" {
		t.Fatalf("issue source = %+v", settled.Source)
	}
	if got := f.driver.submittedPrompt(); !strings.Contains(got, issueBody) || !strings.Contains(got, "untrusted problem context") {
		t.Fatalf("submitted Issue prompt = %q", got)
	}
	assertTaskInboxStateOmits(t, f.tasksDir, issueBody)

	if err := os.WriteFile(filepath.Join(info.WorkDir, "change.txt"), []byte("implemented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTaskInboxGit(t, info.WorkDir, "add", "change.txt")
	runTaskInboxGit(t, info.WorkDir, "commit", "-m", "implement issue")
	head := taskInboxGitOutput(t, info.WorkDir, "rev-parse", "HEAD")
	f.manager.HandleHookEvent("", info.ID, "TurnCompleted", "", info.WorkDir, "")
	waitFor(t, 3*time.Second, "completion review evidence", func() bool {
		current, exists := f.manager.GetInfo(info.ID)
		return exists && current.Status == session.StatusIdle &&
			current.Attention.State == session.AttentionReadyForReview &&
			current.ReviewFacts.Status == session.ReviewFactsAvailable && current.ReviewFacts.HeadCommit == head
	})

	taskInboxRequireSuccess(t, f.server, "check-report", CheckReportRequest{ID: info.ID, Status: session.CheckStatusPassed})
	taskInboxRequireSuccess(t, f.server, "review-disposition", ReviewDispositionRequest{
		ID: info.ID, Decision: session.ReviewDecisionReviewed,
	})
	installTaskInboxProviders(t, f, head)

	prDry := taskInboxCall[PRHandoffResponse](t, f.server, "pr-handoff", PRHandoffRequest{
		ID: info.ID, Plugin: "e2e-pr", Action: "create", DryRun: true,
	})
	if prDry.Payload.IdempotencyKey == "" || !prDry.Handoff.IsZero() {
		t.Fatalf("PR dry-run = %+v", prDry)
	}
	prDone := taskInboxCall[PRHandoffResponse](t, f.server, "pr-handoff", PRHandoffRequest{
		ID: info.ID, Plugin: "e2e-pr", Action: "create", Confirm: true,
		IdempotencyKey: prDry.Payload.IdempotencyKey,
	})
	if prDone.Handoff.Status != session.PRHandoffSucceeded {
		t.Fatalf("PR handoff = %+v", prDone.Handoff)
	}

	mergeDry := taskInboxCall[MergeHandoffResponse](t, f.server, "merge-handoff", MergeHandoffRequest{
		ID: info.ID, Plugin: "e2e-merge", Action: "merge", DryRun: true,
	})
	if mergeDry.Payload.IdempotencyKey == "" || mergeDry.Preflight.Status != "ready" || !mergeDry.Handoff.IsZero() {
		t.Fatalf("merge dry-run = %+v", mergeDry)
	}
	mergeDone := taskInboxCall[MergeHandoffResponse](t, f.server, "merge-handoff", MergeHandoffRequest{
		ID: info.ID, Plugin: "e2e-merge", Action: "merge", Confirm: true,
		IdempotencyKey: mergeDry.Payload.IdempotencyKey,
	})
	if mergeDone.Handoff.Status != session.MergeHandoffSucceeded || mergeDone.Handoff.Result.HeadCommit != head {
		t.Fatalf("merge handoff = %+v", mergeDone.Handoff)
	}

	commentBody := "Implemented and merged; bounded comment marker."
	commentDry := taskInboxCall[TaskCommentResponse](t, f.server, "task-comment", TaskCommentRequest{
		TaskID: created.Task.ID, Body: commentBody, DryRun: true,
	})
	if commentDry.Plan.IdempotencyKey == "" || comments.createCalls != 0 {
		t.Fatalf("comment dry-run = %+v calls=%d", commentDry.Plan, comments.createCalls)
	}
	commentDone := taskInboxCall[TaskCommentResponse](t, f.server, "task-comment", TaskCommentRequest{
		TaskID: created.Task.ID, Body: commentBody, Confirm: true,
		IdempotencyKey: commentDry.Plan.IdempotencyKey,
	})
	if comments.createCalls != 1 || commentDone.Mutation.Status != task.MutationSucceeded {
		t.Fatalf("comment mutation = %+v calls=%d", commentDone.Mutation, comments.createCalls)
	}
	assertTaskInboxStateOmits(t, f.tasksDir, issueBody, commentBody)

	cleanupDry := taskInboxCall[ReviewCleanupResponse](t, f.server, "review-cleanup", ReviewCleanupRequest{
		ID: info.ID, DryRun: true,
	})
	if !cleanupDry.Plan.Ready || cleanupDry.Plan.IdempotencyKey == "" || cleanupDry.Plan.HeadCommit != head {
		t.Fatalf("cleanup dry-run = %+v", cleanupDry.Plan)
	}
	cleanupDone := taskInboxCall[ReviewCleanupResponse](t, f.server, "review-cleanup", ReviewCleanupRequest{
		ID: info.ID, Confirm: true, IdempotencyKey: cleanupDry.Plan.IdempotencyKey,
	})
	if cleanupDone.Journal.Status != session.ReviewCleanupSucceeded {
		t.Fatalf("cleanup journal = %+v", cleanupDone.Journal)
	}
	if _, err := os.Stat(info.WorkDir); !os.IsNotExist(err) {
		t.Fatalf("worktree remains after cleanup: %v", err)
	}
	if _, ok := f.manager.GetInfo(info.ID); ok {
		t.Fatal("session remains after cleanup")
	}

	restartedSessions, err := session.NewManager(f.sessionsDir, f.stateDir, f.manager.Identity(), f.config)
	if err != nil {
		t.Fatal(err)
	}
	restartedTasks, err := task.NewManager(f.tasksDir, restartedSessions)
	if err != nil {
		t.Fatal(err)
	}
	restartedTask, ok := restartedTasks.Get(created.Task.ID)
	if !ok || len(restartedTask.Executions) != 1 || restartedTask.Executions[0].ReferenceState != task.ReferenceMissing ||
		len(restartedTask.Mutations) != 1 || restartedTask.Mutations[0].Status != task.MutationSucceeded {
		t.Fatalf("restarted task = %+v found=%t", restartedTask, ok)
	}
	journal, err := restartedSessions.ReviewCleanupJournal(info.ID)
	if err != nil || journal.Status != session.ReviewCleanupSucceeded {
		t.Fatalf("restarted cleanup journal = %+v err=%v", journal, err)
	}
}

func taskInboxTaskNew(t *testing.T, server *Server, request TaskNewRequest) TaskNewResponse {
	t.Helper()
	return taskInboxCall[TaskNewResponse](t, server, "task-new", request)
}

func taskInboxRequireSuccess(t *testing.T, server *Server, action string, request any) Response {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response := server.handleRequest(&Request{Action: action, Data: data})
	if !response.Success {
		t.Fatalf("%s: %s", action, response.Error)
	}
	return response
}

func taskInboxCall[T any](t *testing.T, server *Server, action string, request any) T {
	t.Helper()
	response := taskInboxRequireSuccess(t, server, action, request)
	var value T
	if err := json.Unmarshal(response.Data, &value); err != nil {
		t.Fatalf("decode %s: %v", action, err)
	}
	return value
}

func installTaskInboxProviders(t *testing.T, f *taskInboxE2EFixture, head string) {
	t.Helper()
	pullURL := "https://github.com/acme/demo/pull/42"
	prResult, _ := json.Marshal(session.PRHandoffProviderResult{
		Status: session.PRHandoffSucceeded, Provider: "github", ID: "42", URL: pullURL,
	})
	installTaskInboxPlugin(t, f.pluginsDir, f.stateDir, "e2e-pr", fmt.Sprintf(`schema_version: 2
name: e2e-pr
version: 0.1.0
description: canonical E2E PR provider
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
actions:
  - id: create
    entrypoint: |
      cat > pr-request.json
      printf '%%s' '%s'
    handoff: true
`, prResult))
	preflight, _ := json.Marshal(session.MergeHandoffPreflightResult{
		Status: "ready", Mergeable: true, RequiredChecks: session.MergeChecksPassed,
		Target: session.MergeProviderTarget{
			Provider: "github", ID: "42", URL: pullURL, BaseRef: "main",
			BaseCommit: f.baseCommit, HeadCommit: head,
		},
	})
	mergeResult, _ := json.Marshal(session.MergeHandoffProviderResult{
		Status: session.MergeHandoffSucceeded, Provider: "github", ID: "42", URL: pullURL,
		HeadCommit: head, TargetCommit: strings.Repeat("c", 40), Method: "squash",
	})
	installTaskInboxPlugin(t, f.pluginsDir, f.stateDir, "e2e-merge", fmt.Sprintf(`schema_version: 2
name: e2e-merge
version: 0.1.0
description: canonical E2E merge provider
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
actions:
  - id: merge
    entrypoint: |
      body=$(cat)
      printf '%%s' "$body" > merge-request.json
      case "$body" in
        *'"operation":"preflight"'*) printf '%%s' '%s' ;;
        *) printf '%%s' '%s' ;;
      esac
    merge_handoff: true
`, preflight, mergeResult))
}

func installTaskInboxPlugin(t *testing.T, pluginsDir, stateDir, name, body string) {
	t.Helper()
	dir := filepath.Join(pluginsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.Filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.LoadLock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Set(name, plugin.LockEntry{Source: "e2e", InstalledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func assertTaskInboxStateOmits(t *testing.T, dir string, forbidden ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range forbidden {
			if strings.Contains(string(data), value) {
				t.Fatalf("Task state %s retained forbidden content %q", entry.Name(), value)
			}
		}
	}
}

func runTaskInboxGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func taskInboxGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func removeTaskInboxWorktree(repo, worktree, branch string) {
	if worktree != "" {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", worktree).Run()
	}
	if branch != "" {
		_ = exec.Command("git", "-C", repo, "branch", "-D", branch).Run()
	}
}
