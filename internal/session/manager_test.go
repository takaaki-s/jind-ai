package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

// testIdentity is the jin newTestManager tells a Manager to hand its agents.
// Deliberately not temp dirs: a test asserting on what reaches an agent's
// environment must be able to tell these values from sessionsDir, stateDir and
// the config dir. Nothing here is ever dialled or executed.
func testIdentity() jinenv.Identity {
	return jinenv.Identity{
		SocketPath: "/nonexistent/test-daemon.sock",
		BinPath:    "/nonexistent/test-daemon-bin/jin",
	}
}

// newTestManager creates a Manager backed by temporary directories, a mock
// tmux runner, and a mock hook runner. Both mocks are pre-wired via their
// setters; tests that don't care about the hook runner discard it with `_`.
func newTestManager(t *testing.T) (*Manager, *mockTmuxRunner, *mockHookRunner) {
	t.Helper()
	return newTestManagerOn(t, testIdentity())
}

// newTestManagerOn is newTestManager with the callback identity named, for
// tests asserting on what reaches an agent's environment. Going through
// NewManager's argument rather than writing the field afterwards is the point:
// that hop is what the assertions are about.
func newTestManagerOn(t *testing.T, identity jinenv.Identity) (*Manager, *mockTmuxRunner, *mockHookRunner) {
	t.Helper()
	dir := t.TempDir()
	configDir := t.TempDir()
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	// A state dir of its own, not configDir reused. Anything asserting against
	// mgr.stateDir — worktree placement, hook logs, bin/jin — cannot tell a
	// state-dir bug from a config-dir one while the two are the same path. The
	// socket is a literal for the same reason, one no test ever connects to.
	mgr, err := NewManager(dir, t.TempDir(), identity, configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	tmuxMock := newMockTmuxRunner()
	mgr.SetTmuxClient(tmuxMock)
	hookMock := newMockHookRunner()
	mgr.SetHookRunner(hookMock)
	mgr.SetAgentResolver(newFakeAgentResolver())
	return mgr, tmuxMock, hookMock
}

// fakeAgent implements session.Agent with the CC hook vocabulary hard-wired
// so HandleHookEvent-driven tests still see the same status transitions as
// pre-refactor code. The concrete Claude Code adapter lives in
// internal/agent/claude/; importing it here would create a build cycle
// (agent/claude → session), so we hand-roll a duplicate mapping.
//
// enhancer is the Layer C enhancer the adapter returns via Description();
// tests that exercise Layer C swap it via installEnhancer (which reaches
// into the fakeAgentResolver held by newTestManager).
type fakeAgent struct {
	// spawnFn overrides SpawnCommand so a test can hand Manager a plan of a
	// shape a real adapter produces.
	spawnFn func(SpawnOptions) SpawnPlan
	// setupFn observes the SetupContext an adapter is handed. Adapters bake
	// ExecPath into the hook wiring they generate, so what arrives here is what
	// a running session's hooks will exec for the rest of its life.
	setupFn   func(SetupContext) error
	enhancer  DescriptionEnhancer
	clearKeys []string            // returned from ClearInputKeys; nil = opt-out (matches production default)
	pasteFn   func(string) string // non-nil opts into the paste transport
	// dismissFn answers DismissOverlayKeys. It takes the prompt rather than
	// being a fixed slice so that a test can pin the wiring: production
	// claude decides per prompt, and a manager that passed "" instead would
	// silently return every session to the pre-fix behaviour while the
	// adapter's own table tests stayed green.
	dismissFn func(string) []string
	// transcriptSrc is what Transcript() hands back. Nil (the default) means
	// this adapter cannot read a conversation, which is what the list rows
	// must tolerate silently.
	transcriptSrc TranscriptSource
	// detectFn answers DetectBlock. Nil (the default) reports BlockNone, so
	// a fake looks like an agent that is not blocked — which makes
	// RespondToBlock refuse before it sends anything. Every test that does
	// not mention a blocking prompt depends on that default.
	detectFn func(string) BlockKind
	// answerFn answers AnswerBlockKeys. Nil (the default) refuses, matching
	// an adapter that recognises a screen it cannot drive.
	answerFn func(BlockKind, string, BlockAnswer) ([]KeyStep, error)
	// recognizesFn answers RecognizesSessionID. Nil (the default) accepts
	// every id, so tests that predate the hook re-key gate keep describing
	// the same behaviour; tests exercising the gate set it.
	recognizesFn func(string) bool
}

func (a *fakeAgent) Kind() string { return "claude" }
func (a *fakeAgent) Setup(ctx SetupContext) error {
	if a.setupFn != nil {
		return a.setupFn(ctx)
	}
	return nil
}
func (a *fakeAgent) SpawnCommand(opts SpawnOptions) SpawnPlan {
	if a.spawnFn != nil {
		return a.spawnFn(opts)
	}
	return SpawnPlan{Command: "claude"}
}
func (a *fakeAgent) RecognizesSessionID(id string) bool {
	if a.recognizesFn == nil {
		return true
	}
	return a.recognizesFn(id)
}
func (a *fakeAgent) Description() DescriptionEnhancer { return a.enhancer }
func (a *fakeAgent) StatusSource() StatusSource       { return fakeStatusSource{} }
func (a *fakeAgent) ClearInputKeys() []string         { return a.clearKeys }
func (a *fakeAgent) PastePlaceholder(prompt string) string {
	if a.pasteFn == nil {
		return ""
	}
	return a.pasteFn(prompt)
}
func (a *fakeAgent) DismissOverlayKeys(prompt string) []string {
	if a.dismissFn == nil {
		return nil
	}
	return a.dismissFn(prompt)
}

func (a *fakeAgent) Transcript() TranscriptSource { return a.transcriptSrc }

func (a *fakeAgent) DetectBlock(capture string) BlockKind {
	if a.detectFn == nil {
		return BlockNone
	}
	return a.detectFn(capture)
}

func (a *fakeAgent) AnswerBlockKeys(kind BlockKind, capture string, ans BlockAnswer) ([]KeyStep, error) {
	if a.answerFn == nil {
		return nil, fmt.Errorf("fake agent cannot answer %q prompts", kind)
	}
	return a.answerFn(kind, capture, ans)
}

type fakeStatusSource struct{}

func (fakeStatusSource) Interpret(sig StatusSignal) (StatusUpdate, bool) {
	if sig.Kind != "hook" {
		return StatusUpdate{}, false
	}
	switch sig.Payload["event"] {
	case "UserPromptSubmit":
		return StatusUpdate{Status: StatusThinking, ClearError: true, Notify: NotifyNone}, true
	case "PreToolUse", "PostToolUse":
		// Liveness mirrors the real adapter: a tool hook cannot open a turn.
		// Without it the tests below would exercise a Manager rule no shipped
		// adapter asks for.
		return StatusUpdate{Status: StatusThinking, ClearError: true, Notify: NotifyNone, Liveness: true}, true
	case "Stop":
		return StatusUpdate{Status: StatusIdle, ClearError: true, Notify: NotifyTaskComplete}, true
	case "StopFailure":
		return StatusUpdate{
			Status:       StatusIdle,
			ErrorMessage: sig.Payload["stop_reason"],
			Notify:       NotifyError,
		}, true
	case "SessionEnd":
		return StatusUpdate{Status: StatusStopped, Notify: NotifyNone}, true
	case "Notification":
		switch sig.Payload["notification_type"] {
		case "permission_prompt", "elicitation_dialog":
			return StatusUpdate{Status: StatusPermission, Notify: NotifyPermission}, true
		case "idle_prompt":
			return StatusUpdate{Status: StatusIdle, Notify: NotifyNone}, true
		}
	}
	return StatusUpdate{}, false
}

type fakeAgentResolver struct {
	agents map[string]Agent
}

func newFakeAgentResolver() *fakeAgentResolver {
	return &fakeAgentResolver{
		agents: map[string]Agent{"claude": &fakeAgent{}},
	}
}

func (r *fakeAgentResolver) Resolve(kind string) (Agent, error) {
	a, ok := r.agents[kind]
	if !ok {
		return nil, fmt.Errorf("unknown agent kind: %s", kind)
	}
	return a, nil
}

// fakeClaudeAgent returns the stub adapter newTestManager registers under
// kind "claude", so a test can set whatever behaviour SendPrompt (or
// HandleHookEvent) will read off it. One accessor rather than one wrapper per
// capability: each new capability would otherwise copy these two type
// assertions and their diagnostics again.
func fakeClaudeAgent(t *testing.T, mgr *Manager) *fakeAgent {
	t.Helper()
	resolver, ok := mgr.agentResolver.(*fakeAgentResolver)
	if !ok {
		t.Fatalf("expected *fakeAgentResolver, got %T", mgr.agentResolver)
	}
	ag, ok := resolver.agents["claude"].(*fakeAgent)
	if !ok {
		t.Fatalf(`expected "claude" adapter to be *fakeAgent, got %T`, resolver.agents["claude"])
	}
	return ag
}

// installEnhancer swaps the Layer C enhancer the "claude" fake adapter
// returns via Description().
func installEnhancer(t *testing.T, mgr *Manager, enh DescriptionEnhancer) {
	t.Helper()
	fakeClaudeAgent(t, mgr).enhancer = enh
}

// installClearKeys mutates the resolver's "claude" adapter so
// ClearInputKeys returns the given keys. Passing an empty (or nil) slice
// covers the "adapter returns no keys" opt-out variant. The baseline
// fakeAgent's clearKeys defaults to nil, so tests that never call this
// observe the fall-through path — that invariant is what keeps the existing
// verify-by-capture tests intact.
func installClearKeys(t *testing.T, mgr *Manager, keys []string) {
	t.Helper()
	fakeClaudeAgent(t, mgr).clearKeys = keys
}

// installDismissKeys mutates the resolver's "claude" adapter so
// DismissOverlayKeys returns the given keys for every prompt. The baseline
// fakeAgent leaves them nil, which is the production opt-out — so every test
// that does not call this exercises the pre-fix key sequence, and a
// regression there shows up as a failure in the existing send tests rather
// than only in the new ones.
func installDismissKeys(t *testing.T, mgr *Manager, keys []string) {
	t.Helper()
	fakeClaudeAgent(t, mgr).dismissFn = func(string) []string { return keys }
}

// installDismissFn is installDismissKeys for tests that need the answer to
// depend on the prompt — the only way to prove SendPrompt hands the real
// prompt to the adapter rather than something it made up.
func installDismissFn(t *testing.T, mgr *Manager, fn func(string) []string) {
	t.Helper()
	fakeClaudeAgent(t, mgr).dismissFn = fn
}

// ---------------------------------------------------------------------------
// CreateWithOptions tests
// ---------------------------------------------------------------------------

func TestManager_CreateWithOptions_Success(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/project-alpha",
		Description: "alpha",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.ID == "" {
		t.Error("expected non-empty ID")
	}
	if sess.Description != "alpha" {
		t.Errorf("Name = %q, want %q", sess.Description, "alpha")
	}
	if sess.WorkDir != "/tmp/project-alpha" {
		t.Errorf("WorkDir = %q, want %q", sess.WorkDir, "/tmp/project-alpha")
	}
	if sess.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", sess.Status, StatusStopped)
	}
	if sess.AgentSessionID == "" {
		t.Error("expected non-empty AgentSessionID")
	}
}

// TestManager_RepoNameReachesInfo covers the whole path the TUI's detail pane
// reads: a session records the repo it lives in at create time, a worktree
// reports the repo it was cut from rather than its own "jin-xxxxxxxx"
// directory, and both survive the daemon restart that clears the json:"-"
// runtime fields.
func TestManager_RepoNameReachesInfo(t *testing.T) {
	mainRepoDir, worktreeDir := repoFixtures(t)
	wantRepo := filepath.Base(mainRepoDir)

	stateDir := t.TempDir()
	configDir := t.TempDir()
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	mgr, err := NewManager(stateDir, configDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	mgr.SetTmuxClient(newMockTmuxRunner())
	mgr.SetHookRunner(newMockHookRunner())
	mgr.SetAgentResolver(newFakeAgentResolver())

	plain, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: mainRepoDir, Description: "plain"})
	if err != nil {
		t.Fatalf("CreateWithOptions(main repo) failed: %v", err)
	}
	if plain.RepoName != wantRepo {
		t.Errorf("main repo session RepoName = %q, want %q", plain.RepoName, wantRepo)
	}

	wt, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Description: "wt"})
	if err != nil {
		t.Fatalf("CreateWithOptions(worktree) failed: %v", err)
	}
	if wt.RepoName != wantRepo {
		t.Errorf("worktree session RepoName = %q, want the main repo %q (not the worktree dir)", wt.RepoName, wantRepo)
	}

	for _, info := range mgr.List() {
		if info.RepoName != wantRepo {
			t.Errorf("List() Info for %q has RepoName %q, want %q", info.Description, info.RepoName, wantRepo)
		}
	}

	// Restart: RepoName is json:"-", so only the load-time recovery can put it
	// back. Stopped sessions never reach captureOutputTmux, which is why the
	// poll cannot be relied on here.
	restarted, err := NewManager(stateDir, configDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager (restart) failed: %v", err)
	}
	infos := restarted.List()
	if len(infos) != 2 {
		t.Fatalf("after restart List() returned %d sessions, want 2", len(infos))
	}
	for _, info := range infos {
		if info.RepoName != wantRepo {
			t.Errorf("after restart, Info for %q has RepoName %q, want %q", info.Description, info.RepoName, wantRepo)
		}
	}
}

func TestManager_CreateWithOptions_DefaultFleet(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-default", Description: "fd"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != DefaultFleet {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, DefaultFleet)
	}
}

func TestManager_CreateWithOptions_ExplicitFleet(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-named", Description: "fn", Fleet: "backend"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != "backend" {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, "backend")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Description: "second"})
	if err == nil {
		t.Fatal("expected error for duplicate WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_SkipWorktreeSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Simulate the session moving into a worktree (CurrentWorkDir updated by daemon polling)
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo/.claude/worktrees/some-branch"
	mgr.mu.Unlock()

	// Creating a new session for the same WorkDir should succeed
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err != nil {
		t.Fatalf("expected success when existing session is in worktree, got: %v", err)
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_BlockNonWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Set CurrentWorkDir to repo root (not a worktree) — duplicate check should still block
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo"
	mgr.mu.Unlock()

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err == nil {
		t.Fatal("expected error for duplicate WorkDir when session is not in worktree")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_StoppedSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// CurrentWorkDir defaults to "" for a freshly created (stopped) session
	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err == nil {
		t.Fatal("stopped session (CurrentWorkDir empty) should still block duplicate WorkDir")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_ReturnFromWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Session enters a worktree
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo/.claude/worktrees/some-branch"
	mgr.mu.Unlock()

	// Session exits worktree, CurrentWorkDir returns to repo root
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo"
	mgr.mu.Unlock()

	// Duplicate check should block again
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err == nil {
		t.Fatal("expected error after session returned from worktree to original WorkDir")
	}
}

func TestManager_CreateWithOptions_EmptyWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "", Description: "nodir"})
	if err == nil {
		t.Fatal("expected error for empty WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DefaultDescription(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/home/user/my-project"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	want := filepath.Base("/home/user/my-project")
	if sess.Description != want {
		t.Errorf("Description = %q, want %q (filepath.Base of WorkDir)", sess.Description, want)
	}
	if sess.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false when auto-generated")
	}
}

func TestManager_CreateWithOptions_WhitespaceOnlyDescription_UsesBaseline(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/home/user/whitespace-project",
		Description: "   \t  ",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	want := filepath.Base("/home/user/whitespace-project")
	if sess.Description != want {
		t.Errorf("Description = %q, want %q (whitespace-only should fall back to baseline)", sess.Description, want)
	}
	if sess.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false when input trimmed to empty")
	}
}

// ---------------------------------------------------------------------------
// Get tests
// ---------------------------------------------------------------------------

func TestManager_Get_Found(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	created, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/get-test", Description: "getme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	got, ok := mgr.Get(created.ID)
	if !ok {
		t.Fatal("Get returned ok=false for existing session")
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, ok := mgr.Get("nonexistent-id")
	if ok {
		t.Fatal("Get returned ok=true for nonexistent session")
	}
}

// ---------------------------------------------------------------------------
// List tests
// ---------------------------------------------------------------------------

func TestManager_List(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-1", Description: "first"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	// Ensure distinct CreatedAt timestamps.
	time.Sleep(2 * time.Millisecond)
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-2", Description: "second"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 2 {
		t.Fatalf("List returned %d items, want 2", len(infos))
	}
	// Sorted by CreatedAt ascending
	if infos[0].Description != "first" {
		t.Errorf("first item Name = %q, want %q", infos[0].Description, "first")
	}
	if infos[1].Description != "second" {
		t.Errorf("second item Name = %q, want %q", infos[1].Description, "second")
	}
}

func TestManager_List_SortedByFleet(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-1", Description: "s1", Fleet: "backend"})
	if err != nil {
		t.Fatalf("create s1 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-2", Description: "s2"}) // default
	if err != nil {
		t.Fatalf("create s2 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-3", Description: "s3", Fleet: "alpha"})
	if err != nil {
		t.Fatalf("create s3 failed: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 3 {
		t.Fatalf("List returned %d items, want 3", len(infos))
	}
	// Expected order: alpha, backend, default (default always last)
	if infos[0].Fleet != "alpha" {
		t.Errorf("infos[0].Fleet = %q, want %q", infos[0].Fleet, "alpha")
	}
	if infos[1].Fleet != "backend" {
		t.Errorf("infos[1].Fleet = %q, want %q", infos[1].Fleet, "backend")
	}
	if infos[2].Fleet != DefaultFleet {
		t.Errorf("infos[2].Fleet = %q, want %q", infos[2].Fleet, DefaultFleet)
	}
}

// ---------------------------------------------------------------------------
// SetStatus / SetStatusWithError tests
// ---------------------------------------------------------------------------

func TestManager_SetStatus(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/status-test", Description: "s1"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.SetStatus(sess.ID, StatusThinking)

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusThinking {
		t.Errorf("Status = %q, want %q", got.Status, StatusThinking)
	}
}

func TestManager_SetStatusWithError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/err-test", Description: "e1"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.SetStatusWithError(sess.ID, StatusStopped, "something went wrong")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if got.ErrorMessage != "something went wrong" {
		t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, "something went wrong")
	}
}

// ---------------------------------------------------------------------------
// SetWorkDir tests
// ---------------------------------------------------------------------------

func TestManager_SetWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-old", Description: "wd"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := mgr.SetWorkDir(sess.ID, "/tmp/wd-new"); err != nil {
		t.Fatalf("SetWorkDir failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != "/tmp/wd-new" {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, "/tmp/wd-new")
	}
}

func TestManager_SetWorkDir_DuplicateWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-dup", Description: "d1"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	s2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-other", Description: "d2"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	err = mgr.SetWorkDir(s2.ID, "/tmp/wd-dup")
	if err == nil {
		t.Fatal("expected error when setting WorkDir to one already in use, got nil")
	}
}

func TestManager_SetWorkDir_DuplicateWorkDir_SkipWorktreeSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-dup", Description: "d1"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	s2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-other", Description: "d2"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	// s1 is in a worktree — SetWorkDir should succeed
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/wd-dup/.claude/worktrees/some-branch"
	mgr.mu.Unlock()

	err = mgr.SetWorkDir(s2.ID, "/tmp/wd-dup")
	if err != nil {
		t.Fatalf("expected success when conflicting session is in worktree, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CountActive tests
// ---------------------------------------------------------------------------

func TestManager_CountActive(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-1", Description: "ca1"})
	s2, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-2", Description: "ca2"})
	s3, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-3", Description: "ca3"})

	// All start as StatusStopped; set two to active statuses.
	mgr.SetStatus(s2.ID, StatusThinking)
	mgr.SetStatus(s3.ID, StatusRunning)
	// s1 remains StatusStopped

	_ = s1 // keep compiler happy

	count := mgr.CountActive()
	if count != 2 {
		t.Errorf("CountActive() = %d, want 2", count)
	}
}

// ---------------------------------------------------------------------------
// HandleHookEvent tests
// ---------------------------------------------------------------------------

func TestManager_HandleHookEvent_UserPromptSubmit(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ups", Description: "hups"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusThinking {
		t.Errorf("Status = %q, want %q", got.Status, StatusThinking)
	}
}

func TestManager_HandleHookEvent_Stop(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-stop", Description: "hstop"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Set to thinking first
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
}

func TestManager_HandleHookEvent_Notification_Permission(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-perm", Description: "hperm"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Notification", "permission_prompt", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusPermission {
		t.Errorf("Status = %q, want %q", got.Status, StatusPermission)
	}
}

func TestManager_HandleHookEvent_UnknownSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Should not panic when both IDs are unknown.
	mgr.HandleHookEvent("unknown-cc-id", "unknown-jin-id", "Stop", "", "", "")
}

func TestManager_HandleHookEvent_CWDUpdate(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-cwd", Description: "hcwd"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// CWD to a non-git-root path: WorkDir should NOT update, only CurrentWorkDir
	nonGitCwd := "/tmp/hook-cwd-subdir"
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", nonGitCwd, "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != "/tmp/hook-cwd" {
		t.Errorf("WorkDir = %q, want %q (should not update for non-git-root)", got.WorkDir, "/tmp/hook-cwd")
	}
	if got.CurrentWorkDir != nonGitCwd {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, nonGitCwd)
	}

	// CWD to a git root (has .git): WorkDir SHOULD update
	gitRootCwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitRootCwd, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", gitRootCwd, "")

	got, ok = mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != gitRootCwd {
		t.Errorf("WorkDir = %q, want %q (should update for git root)", got.WorkDir, gitRootCwd)
	}
	if got.CurrentWorkDir != gitRootCwd {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, gitRootCwd)
	}
}

func TestManager_HandleHookEvent_CwdChanged(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	origDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(origDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Description: "hcwdch"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// CWD to a non-git-root: only CurrentWorkDir updates
	subDir := filepath.Join(origDir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "CwdChanged", "", subDir, "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != origDir {
		t.Errorf("WorkDir = %q, want %q (should not update for subdirectory)", got.WorkDir, origDir)
	}
	if got.CurrentWorkDir != subDir {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, subDir)
	}
	// Status should remain unchanged (stopped from creation)
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q (unchanged)", got.Status, StatusStopped)
	}

	// CWD to a different git root (worktree): WorkDir SHOULD update
	worktreeDir := t.TempDir()
	// Simulate a git worktree (.git is a file, not a directory)
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: ../main/.git/worktrees/wt"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "CwdChanged", "", worktreeDir, "")

	got, ok = mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != worktreeDir {
		t.Errorf("WorkDir = %q, want %q (should update for worktree root)", got.WorkDir, worktreeDir)
	}
	if got.CurrentWorkDir != worktreeDir {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, worktreeDir)
	}
}

func TestManager_HandleHookEvent_StopFailure(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf", Description: "hsf"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "rate_limit")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
	if got.ErrorMessage != "rate_limit" {
		t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, "rate_limit")
	}
}

func TestManager_HandleHookEvent_StopFailure_ThenStop_ClearsError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr", Description: "hsfclr"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	// First: StopFailure sets ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "rate_limit")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "rate_limit" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "rate_limit")
	}

	// Then: Stop clears ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage after Stop = %q, want empty", got.ErrorMessage)
	}
}

func TestManager_HandleHookEvent_StopFailure_ThenUserPrompt_ClearsError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr2", Description: "hsfclr2"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	// StopFailure sets ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "auth_error")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "auth_error" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "auth_error")
	}

	// UserPromptSubmit clears ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage after UserPromptSubmit = %q, want empty", got.ErrorMessage)
	}
}

// SessionEnd on a session that still has an error message must preserve
// that message: pre-refactor SessionEnd never touched ErrorMessage, so a
// StopFailure that fired just before the process died should still surface
// after the session is stopped. This guards F002 from regressing back to
// "any adapter verdict clears the error field".
func TestManager_HandleHookEvent_SessionEnd_PreservesError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sessend", Description: "sessend"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "rate_limit")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "rate_limit" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "rate_limit")
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "rate_limit" {
		t.Errorf("ErrorMessage after SessionEnd = %q, want %q (SessionEnd must preserve)", got.ErrorMessage, "rate_limit")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status after SessionEnd = %q, want %q", got.Status, StatusStopped)
	}
}

// Notification hooks (permission_prompt / elicitation_dialog / idle_prompt)
// must not touch ErrorMessage either — F002 regression guard.
func TestManager_HandleHookEvent_Notification_PreservesError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-notif", Description: "notif"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "auth_error")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "auth_error" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "auth_error")
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Notification", "permission_prompt", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "auth_error" {
		t.Errorf("ErrorMessage after Notification = %q, want %q (Notification must preserve)", got.ErrorMessage, "auth_error")
	}
	if got.Status != StatusPermission {
		t.Errorf("Status after Notification permission_prompt = %q, want %q", got.Status, StatusPermission)
	}
}

// F001 regression guard: SessionEnd delivered to an already-stopped session
// must not silently mutate in-memory-only fields (LastOutputTime /
// LastActiveAt). The early-return path handles CWD persistence only.
func TestManager_HandleHookEvent_SessionEnd_AlreadyStopped_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sess-noop", Description: "noop"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Prime the session as already-stopped with a fixed LastActiveAt so we
	// can prove SessionEnd did not shift it.
	fixed := time.Now().Add(-1 * time.Hour)
	mgr.mu.Lock()
	sess.Status = StatusStopped
	sess.LastActiveAt = fixed
	mgr.mu.Unlock()

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if !got.LastActiveAt.Equal(fixed) {
		t.Errorf("LastActiveAt drifted: got %v, want %v", got.LastActiveAt, fixed)
	}
}

func TestManager_HandleHookEvent_StopFailure_EmptyReason(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf2", Description: "hsf2"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty", got.ErrorMessage)
	}
}

func TestManager_HandleHookEvent_SessionStart(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ss", Description: "hss"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Ensure AgentSessionStarted is false initially
	sess.AgentSessionStarted = false

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if !got.AgentSessionStarted {
		t.Error("AgentSessionStarted should be true after SessionStart hook")
	}
}

func TestManager_HandleHookEvent_SessionEnd(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-se", Description: "hse"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

func TestManager_HandleHookEvent_SessionEnd_Idempotent(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sei", Description: "hsei"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Session is already stopped (default from creation)
	if sess.Status != StatusStopped {
		t.Fatalf("precondition: Status = %q, want %q", sess.Status, StatusStopped)
	}

	// Should not panic or change anything
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

// ensureHooksSettingsFile lives under internal/agent/claude/ now; its tests
// moved with it (see hooks_settings_test.go there).

// ---------------------------------------------------------------------------
// Kill tests
// ---------------------------------------------------------------------------

func TestManager_Kill(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-test", Description: "killme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate a running session with tmux integration.
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if !mock.hasCalledWith("TerminatePaneProcess", "%42") {
		t.Error("expected TerminatePaneProcess to be called with %42")
	}
	if mock.hasCalledWith("KillPane", "%42") {
		t.Error("KillPane was called; a pane that stopped on its own must be left standing")
	}
	// The window and pane are what a later start revives in place, and what
	// keeps the session's other panes (plugin splits, user shells) alive in
	// the meantime. Clearing them is the fallback path's job, not this one's.
	if got.TmuxWindowName != "jin_"+sess.ID {
		t.Errorf("TmuxWindowName = %q, want it preserved as %q", got.TmuxWindowName, "jin_"+sess.ID)
	}
	if got.TmuxPaneID != "%42" {
		t.Errorf("TmuxPaneID = %q, want it preserved as %q", got.TmuxPaneID, "%42")
	}
}

// TestManager_Kill_FallsBackWhenProcessSurvives covers the pane whose process
// sits through SIGTERM: kill still has to be a kill, so the pane is destroyed
// outright and the fields that no longer address anything are cleared.
func TestManager_Kill_FallsBackWhenProcessSurvives(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-survivor", Description: "survivor"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	mock.terminateSurvivors["%42"] = true

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if !mock.hasCalledWith("KillPane", "%42") {
		t.Error("expected the kill-pane fallback for a process that ignored SIGTERM")
	}
	if got.TmuxWindowName != "" || got.TmuxPaneID != "" {
		t.Errorf("TmuxWindowName = %q, TmuxPaneID = %q, want both cleared once the pane is gone", got.TmuxWindowName, got.TmuxPaneID)
	}
}

// TestManager_Kill_TerminateErrorFallsBack covers the other failure shape: the
// pid could not be read at all, so the signal never went anywhere.
func TestManager_Kill_TerminateErrorFallsBack(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-termerr", Description: "termerr"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	mock.terminateErr["%42"] = fmt.Errorf("no pane_pid")

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if !mock.hasCalledWith("KillPane", "%42") {
		t.Error("expected the kill-pane fallback when the signal could not be delivered")
	}
	if got.TmuxPaneID != "" {
		t.Errorf("TmuxPaneID = %q, want it cleared", got.TmuxPaneID)
	}
}

// TestManager_Kill_PreservesWindowForRestart is the behaviour the whole change
// exists for: a killed session restarts into the pane it already had, so every
// other pane in that window survives the round trip.
func TestManager_Kill_PreservesWindowForRestart(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	workDir := t.TempDir()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "revive"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	innerName := "sess-" + sess.ID
	mock.paneIDs[innerName] = "%99"

	if err := mgr.StartBackground(sess.ID); err != nil {
		t.Fatalf("StartBackground failed: %v", err)
	}
	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	killed, _ := mgr.Get(sess.ID)
	if killed.TmuxWindowName != innerName {
		t.Fatalf("TmuxWindowName = %q after Kill, want it kept as %q", killed.TmuxWindowName, innerName)
	}

	// The first start clears any stale inner session of the same name, so the
	// question is whether the restart adds another one — that call is what
	// takes the window, and every pane in it, down.
	killSessionsBefore := mock.countCallsWithArgs("KillSession", innerName)

	if err := mgr.StartBackground(sess.ID); err != nil {
		t.Fatalf("restart failed: %v", err)
	}

	if !mock.hasCalledWith("RespawnPane", "%99") {
		t.Error("expected the restart to respawn the existing pane")
	}
	if n := mock.countCallsWithArgs("KillSession", innerName); n != killSessionsBefore {
		t.Errorf("KillSession calls = %d, want it unchanged at %d; the restart must not rebuild the window", n, killSessionsBefore)
	}
}

// TestManager_Kill_DoesNotClobberConcurrentRestart drives a start into the
// window where Kill has released the lock to signal the pane. The newer state
// has to win: stamping "stopped" over it would leave a live agent that nothing
// is monitoring.
func TestManager_Kill_DoesNotClobberConcurrentRestart(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-race", Description: "race"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	sess.StartedAt = time.Now().Add(-time.Hour)
	mgr.mu.Unlock()

	// Stand in for a restart landing mid-kill: same pane, same inner session,
	// fresh StartedAt — exactly what startSessionTmux's revive branch leaves
	// behind, which is why StartedAt is the marker Kill re-validates against.
	mock.onTerminatePaneProcess = func(string) {
		mgr.mu.Lock()
		sess.Status = StatusRunning
		sess.StartedAt = time.Now()
		mgr.mu.Unlock()
	}

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want the restarted session left alone at %q", got.Status, StatusRunning)
	}
	if got.TmuxPaneID != "%42" || got.TmuxWindowName != "jin_"+sess.ID {
		t.Errorf("TmuxPaneID = %q / TmuxWindowName = %q, want both untouched", got.TmuxPaneID, got.TmuxWindowName)
	}
}

// TestManager_Kill_OnAlreadyStoppedPaneKeepsWindow covers killing a session
// whose agent is already gone — a second `x`, or a kill on a session that
// crashed on its own. tmux keeps reporting the pid such a pane started with
// long after the process is gone, so signalling it would fire at a number the
// OS may have reissued to something unrelated; falling back to kill-pane
// afterwards would then drop the window and take the session's other panes
// with it on the next start.
func TestManager_Kill_OnAlreadyStoppedPaneKeepsWindow(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-twice", Description: "killtwice"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	windowName := "jin_" + sess.ID
	mgr.mu.Lock()
	sess.TmuxWindowName = windowName
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("first Kill failed: %v", err)
	}
	terminatesAfterFirst := mock.countCallsWithArgs("TerminatePaneProcess", "%42")

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("second Kill failed: %v", err)
	}

	if n := mock.countCallsWithArgs("TerminatePaneProcess", "%42"); n != terminatesAfterFirst {
		t.Errorf("TerminatePaneProcess calls = %d, want it unchanged at %d; a dead pane's pid is stale", n, terminatesAfterFirst)
	}
	if mock.hasCalledWith("KillPane", "%42") {
		t.Error("KillPane was called on an already-dead pane")
	}
	got, _ := mgr.Get(sess.ID)
	if got.TmuxWindowName != windowName || got.TmuxPaneID != "%42" {
		t.Errorf("TmuxWindowName = %q / TmuxPaneID = %q, want both kept so the restart revives in place", got.TmuxWindowName, got.TmuxPaneID)
	}
}

// TestManager_Kill_DoesNotSwallowConcurrentStart pins the other half of the
// kill window: the session has to read as stopped for the whole time its agent
// is being taken down, or a start arriving in that window finds a session that
// "is already running" and returns without doing anything.
func TestManager_Kill_DoesNotSwallowConcurrentStart(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	workDir := t.TempDir()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "restart-in-window"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	innerName := "sess-" + sess.ID
	mock.paneIDs[innerName] = "%99"
	if err := mgr.StartBackground(sess.ID); err != nil {
		t.Fatalf("StartBackground failed: %v", err)
	}
	respawnsBefore := mock.countCallsWithArgs("RespawnPane", "%99", "")

	// A real start, issued while Kill is between its two lock sections.
	mock.onTerminatePaneProcess = func(string) {
		if err := mgr.StartBackground(sess.ID); err != nil {
			t.Errorf("StartBackground during kill failed: %v", err)
		}
	}

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	// Which side wins the pane is racy by nature (the kill is still tearing
	// down what the start just brought back). What must not happen is the
	// start quietly doing nothing at all.
	respawns := 0
	for _, c := range mock.calls {
		if c.method == "RespawnPane" && len(c.args) > 0 && c.args[0] == "%99" {
			respawns++
		}
	}
	if respawns <= respawnsBefore {
		t.Errorf("RespawnPane calls = %d, want more than %d; the start was swallowed by a session still reading as running", respawns, respawnsBefore)
	}
}

// TestManager_Kill_SurvivesDyingAgentHook covers the last word an agent gets:
// terminating it tends to make it fire one final hook, and that hook lands in
// the window where Kill has released the lock. HandleHookEvent writes whatever
// status the hook maps to without knowing a kill is in flight, so the kill has
// to have the final say or the session sits in the list as idle.
func TestManager_Kill_SurvivesDyingAgentHook(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-dying-hook", Description: "dyinghook"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.TmuxPaneID = "%42"
	sess.Status = StatusThinking
	agentSessionID := sess.AgentSessionID
	mgr.mu.Unlock()

	// The agent's parting Stop hook, delivered mid-kill.
	mock.onTerminatePaneProcess = func(string) {
		mgr.HandleHookEvent(agentSessionID, sess.ID, "Stop", "", "", "")
	}

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q; a hook fired while dying must not outrank the kill", got.Status, StatusStopped)
	}
}

// TestManager_HandlePaneDeath_StoppedSessionDoesNotRespawn is the wiring test
// for the monitor's guard: a kill that lands between the loop's status read
// and its pane probe arrives here looking exactly like a crash, and inside the
// quick-resume window that misreading would hand the user back the agent they
// just stopped.
func TestManager_HandlePaneDeath_StoppedSessionDoesNotRespawn(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/panedeath-stopped", Description: "pd-stopped"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.TmuxPaneID = "%42"
	sess.Status = StatusStopped // Kill got here first
	sess.AgentSessionStarted = true
	sess.spawnResumed = true
	sess.StartedAt = time.Now() // inside quickResumeFailWindow
	mgr.mu.Unlock()

	if stop := mgr.handlePaneDeath(sess, "%42", sess.Description, 1); !stop {
		t.Error("handlePaneDeath = false, want the monitor to exit on an already-recorded stop")
	}
	if mock.hasCalledWith("RespawnPane", "%42") {
		t.Error("the agent was respawned after a stop was already recorded")
	}
	got, _ := mgr.Get(sess.ID)
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want it left at %q", got.Status, StatusStopped)
	}
}

// TestManager_HandlePaneDeath_QuickResumeRetries pins the behaviour the guard
// above must not break: a genuine resume failure still gets its one retry.
func TestManager_HandlePaneDeath_QuickResumeRetries(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "pd-retry"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	sess.AgentSessionStarted = true
	sess.AgentSessionIDConfirmed = true
	sess.spawnResumed = true
	sess.StartedAt = time.Now()
	mgr.mu.Unlock()

	if stop := mgr.handlePaneDeath(sess, "%42", sess.Description, 1); stop {
		t.Error("handlePaneDeath = true, want the monitor to keep watching a retried session")
	}
	if !mock.hasCalledWith("RespawnPane", "%42") {
		t.Error("expected the resume retry to respawn the pane")
	}
	got, _ := mgr.Get(sess.ID)
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q after a successful retry", got.Status, StatusRunning)
	}
	mgr.mu.RLock()
	confirmed := sess.AgentSessionIDConfirmed
	mgr.mu.RUnlock()
	if confirmed {
		t.Error("AgentSessionIDConfirmed = true after the retry minted an id no agent has reported")
	}
}

// TestStartSessionTmux_CarriesResumedFromThePlan pins the wiring
// classifyPaneDeath reads. Whether a spawn resumed is the adapter's answer and
// Manager is the only thing that carries it to the monitor, so a start that
// dropped it would leave every session looking like a fresh spawn.
func TestStartSessionTmux_CarriesResumedFromThePlan(t *testing.T) {
	for _, resumed := range []bool{true, false} {
		t.Run(fmt.Sprintf("resumed=%v", resumed), func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			fakeClaudeAgent(t, mgr).spawnFn = func(SpawnOptions) SpawnPlan {
				return SpawnPlan{Command: "claude", Resumed: resumed}
			}

			sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "spawn-resumed"})
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}
			if err := mgr.StartBackground(sess.ID); err != nil {
				t.Fatalf("StartBackground: %v", err)
			}

			mgr.mu.RLock()
			got := sess.spawnResumed
			mgr.mu.RUnlock()
			if got != resumed {
				t.Errorf("spawnResumed = %v, want %v", got, resumed)
			}
		})
	}
}

// TestManager_HandlePaneDeath_RetryDropsResumed pins what the retry leaves
// behind. It mints an id no agent has reported, so the spawn it builds is a
// fresh start whatever the adapter says: carrying the dead spawn's answer
// forward would tell the monitor a fresh agent was resumed.
func TestManager_HandlePaneDeath_RetryDropsResumed(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	// An adapter that calls every spawn a resume, so the assertion below can
	// only pass on what the retry does to the record.
	fakeClaudeAgent(t, mgr).spawnFn = func(SpawnOptions) SpawnPlan {
		return SpawnPlan{Command: "claude", Resumed: true}
	}

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "pd-retry-resumed"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	sess.AgentSessionStarted = true
	sess.spawnResumed = true // the spawn that just died was a resume
	sess.StartedAt = time.Now()
	mgr.mu.Unlock()

	if stop := mgr.handlePaneDeath(sess, "%42", sess.Description, 1); stop {
		t.Fatal("handlePaneDeath = true, want the retry to run")
	}

	mgr.mu.RLock()
	got := sess.spawnResumed
	mgr.mu.RUnlock()
	if got {
		t.Error("spawnResumed = true after a retry that minted an id no agent has reported")
	}
}

// TestManager_HandlePaneDeath_KillDuringRetryWins drives a kill into the
// window where the retry has released the lock to respawn. The retry must not
// publish the agent it just brought back — the user asked for this session to
// be down after that decision was made.
func TestManager_HandlePaneDeath_KillDuringRetryWins(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "pd-killrace"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	sess.AgentSessionStarted = true
	sess.spawnResumed = true
	sess.StartedAt = time.Now()
	mgr.mu.Unlock()

	mock.onRespawnPane = func(string) {
		if err := mgr.Kill(sess.ID); err != nil {
			t.Errorf("Kill during retry failed: %v", err)
		}
	}

	if stop := mgr.handlePaneDeath(sess, "%42", sess.Description, 1); !stop {
		t.Error("handlePaneDeath = false, want the monitor to exit after the session was killed")
	}
	got, _ := mgr.Get(sess.ID)
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q; the kill landed after the retry decision", got.Status, StatusStopped)
	}
}

// TestClassifyPaneDeath pins the monitor's reading of a dead agent pane. The
// stopped-inside-the-retry-window row is the regression guard: that is the
// shape a kill leaves behind, and treating it as a failed resume would respawn
// the agent the user just stopped.
func TestClassifyPaneDeath(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		sess       Session
		deadStatus int
		want       paneDeathOutcome
	}{
		{
			name:       "killed inside the quick-resume window",
			sess:       Session{Status: StatusStopped, AgentSessionStarted: true, spawnResumed: true, StartedAt: now.Add(-time.Second)},
			deadStatus: 1,
			want:       paneDeathAlreadyStopped,
		},
		{
			name:       "killed outside the quick-resume window",
			sess:       Session{Status: StatusStopped, AgentSessionStarted: true, spawnResumed: true, StartedAt: now.Add(-time.Hour)},
			deadStatus: 1,
			want:       paneDeathAlreadyStopped,
		},
		{
			name:       "resume failed right after start",
			sess:       Session{Status: StatusRunning, AgentSessionStarted: true, spawnResumed: true, StartedAt: now.Add(-time.Second)},
			deadStatus: 1,
			want:       paneDeathQuickResumeRetry,
		},
		{
			// The monitor never looks sooner than this — see quickResumeFailWindow.
			name:       "resume failed, seen at the first poll one interval later",
			sess:       Session{Status: StatusRunning, AgentSessionStarted: true, spawnResumed: true, StartedAt: now.Add(-paneMonitorInterval - 50*time.Millisecond)},
			deadStatus: 1,
			want:       paneDeathQuickResumeRetry,
		},
		{
			name:       "a fresh spawn died young",
			sess:       Session{Status: StatusRunning, AgentSessionStarted: true, spawnResumed: false, StartedAt: now.Add(-time.Second)},
			deadStatus: 1,
			want:       paneDeathRecordStop,
		},
		{
			name:       "a resumed session the user quit cleanly",
			sess:       Session{Status: StatusRunning, AgentSessionStarted: true, spawnResumed: true, StartedAt: now.Add(-time.Second)},
			deadStatus: 0,
			want:       paneDeathRecordStop,
		},
		{
			name:       "died long after start",
			sess:       Session{Status: StatusIdle, AgentSessionStarted: true, spawnResumed: true, StartedAt: now.Add(-time.Hour)},
			deadStatus: 1,
			want:       paneDeathRecordStop,
		},
		{
			name:       "never spawned an agent",
			sess:       Session{Status: StatusRunning, AgentSessionStarted: false, StartedAt: now.Add(-time.Second)},
			deadStatus: 1,
			want:       paneDeathRecordStop,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPaneDeath(&tt.sess, now, tt.deadStatus); got != tt.want {
				t.Errorf("classifyPaneDeath = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestManager_HandlePaneDeath_SecondDeathStops drives the death of the agent
// the retry just started. The retry mints an id no agent has reported, so what
// it spawned is a fresh start — and a fresh start that dies is not a failed
// resume. Without that reset an agent that cannot come up at all is respawned
// once per poll for as long as the session lives.
func TestManager_HandlePaneDeath_SecondDeathStops(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "pd-second"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	sess.AgentSessionStarted = true
	sess.spawnResumed = true
	sess.StartedAt = time.Now()
	mgr.mu.Unlock()

	if stop := mgr.handlePaneDeath(sess, "%42", sess.Description, 1); stop {
		t.Fatal("handlePaneDeath = true on the first death, want the monitor to keep watching")
	}
	spent := len(mock.respawnedPanes())

	// Same window, same non-zero exit: only what the retry did to the record
	// can tell this death from the one before it.
	mgr.mu.Lock()
	sess.Status = StatusRunning
	sess.StartedAt = time.Now()
	mgr.mu.Unlock()

	if stop := mgr.handlePaneDeath(sess, "%42", sess.Description, 1); !stop {
		t.Error("handlePaneDeath = false, want the monitor to exit on the retry's own death")
	}
	if got := len(mock.respawnedPanes()); got != spent {
		t.Errorf("respawns = %d, want it left at %d", got, spent)
	}
	got, _ := mgr.Get(sess.ID)
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

// ---------------------------------------------------------------------------
// Delete tests
// ---------------------------------------------------------------------------

func TestManager_Delete(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-test", Description: "delme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := mgr.Delete(sess.ID, false, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Session should no longer be accessible via Get.
	_, ok := mgr.Get(sess.ID)
	if ok {
		t.Fatal("Get returned ok=true after Delete")
	}

	// Store should also have removed the file.
	_, err = mgr.store.Load(sess.ID)
	if err == nil {
		t.Fatal("expected store.Load to return error after Delete, got nil")
	}
}

// ---------------------------------------------------------------------------
// RecoverTmuxSessions tests
// ---------------------------------------------------------------------------

func TestManager_RecoverTmuxSessions_Live(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-live", Description: "rlive"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	innerName := "jin_" + sess.ID
	mgr.mu.Lock()
	sess.TmuxWindowName = innerName
	sess.TmuxPaneID = "%10"
	mgr.mu.Unlock()

	// Configure mock: session exists and pane is alive.
	mock.sessions[innerName] = true
	mock.deadPanes["%10"] = false

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
}

func TestManager_RecoverTmuxSessions_DeadPane(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-dead", Description: "rdead"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	innerName := "jin_" + sess.ID
	mgr.mu.Lock()
	sess.TmuxWindowName = innerName
	sess.TmuxPaneID = "%11"
	mgr.mu.Unlock()

	// Configure mock: session exists but pane is dead.
	mock.sessions[innerName] = true
	mock.deadPanes["%11"] = true

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	// TmuxWindowName should be kept (window preserved via remain-on-exit).
	if got.TmuxWindowName == "" {
		t.Error("expected TmuxWindowName to be kept after dead pane recovery")
	}
}

// setupLivePaneSession creates a session in the state a daemon restart leaves
// it in — Status normalized to Stopped with the on-disk value stashed in
// PersistedStatus — and marks its inner tmux session and pane alive on the
// mock. The shared ritual of the daemon-restart recovery tests.
func setupLivePaneSession(t *testing.T, mgr *Manager, mock *mockTmuxRunner, paneID string, persisted Status) *Session {
	t.Helper()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-live-pane", Description: "rlivepane"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	innerName := "jin_" + sess.ID
	mgr.mu.Lock()
	sess.TmuxWindowName = innerName
	sess.TmuxPaneID = paneID
	sess.Status = StatusStopped
	sess.PersistedStatus = persisted
	mgr.mu.Unlock()
	mock.sessions[innerName] = true
	mock.deadPanes[paneID] = false
	return sess
}

// TestManager_RecoverTmuxSessions_PreservesPersistedStatus verifies that the
// hook-derived persisted status survives a daemon restart instead of being
// overwritten with StatusRunning. The fake resolver's status source returns
// false for "recover" signals, so only the preserve path is exercised here.
func TestManager_RecoverTmuxSessions_PreservesPersistedStatus(t *testing.T) {
	for _, status := range []Status{StatusIdle, StatusThinking, StatusPermission} {
		t.Run(string(status), func(t *testing.T) {
			mgr, mock, _ := newTestManager(t)
			sess := setupLivePaneSession(t, mgr, mock, "%12", status)

			mgr.RecoverTmuxSessions()

			got, ok := mgr.Get(sess.ID)
			if !ok {
				t.Fatal("Get returned ok=false")
			}
			if got.Status != status {
				t.Errorf("Status = %q, want persisted %q", got.Status, status)
			}
		})
	}
}

// recoverVerdictSource answers "recover" signals with a canned verdict and
// records the signal so tests can assert the payload Manager built.
type recoverVerdictSource struct {
	verdict StatusUpdate
	ok      bool
	lastSig StatusSignal
}

func (s *recoverVerdictSource) Interpret(sig StatusSignal) (StatusUpdate, bool) {
	if sig.Kind != "recover" {
		// Hooks go to the shared fake: a recovery test that fires one needs
		// the same mapping every other test in this file assumes.
		return fakeStatusSource{}.Interpret(sig)
	}
	s.lastSig = sig
	return s.verdict, s.ok
}

// recoverVerdictAgent is a fakeAgent whose status source is swapped for a
// recoverVerdictSource.
type recoverVerdictAgent struct {
	fakeAgent
	source *recoverVerdictSource
}

func (a *recoverVerdictAgent) StatusSource() StatusSource { return a.source }

func TestManager_RecoverTmuxSessions_RecoverVerdictApplied(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	source := &recoverVerdictSource{
		verdict: StatusUpdate{Status: StatusIdle},
		ok:      true,
	}
	mgr.SetAgentResolver(&fakeAgentResolver{
		agents: map[string]Agent{"claude": &recoverVerdictAgent{source: source}},
	})

	// stale thinking: Stop hook missed while daemon was down
	sess := setupLivePaneSession(t, mgr, mock, "%13", StatusThinking)
	agentSessionID := sess.AgentSessionID

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want adapter verdict %q", got.Status, StatusIdle)
	}
	if s := source.lastSig.Payload["persisted_status"]; s != string(StatusThinking) {
		t.Errorf("persisted_status payload = %q, want %q", s, StatusThinking)
	}
	if s := source.lastSig.Payload["agent_session_id"]; s != agentSessionID {
		t.Errorf("agent_session_id payload = %q, want %q", s, agentSessionID)
	}
}

// TestManager_RecoverTmuxSessions_HookDuringProbeBeatsVerdict pins the guard
// in applyRecovery: a hook that lands while the probes run outranks the
// adapter's verdict, which was derived from the pre-probe snapshot.
//
// mock.onHasSession is the seam that makes the race deterministic — the probe
// phase calls it exactly once per session, between snapshotForRecovery and
// applyRecovery. What runs there is the real HandleHookEvent path, so the test
// fails if that path stops moving Status.
//
// TestManager_RecoverTmuxSessions_RecoverVerdictApplied above is the control:
// with nothing moving in the window, the verdict still wins.
func TestManager_RecoverTmuxSessions_HookDuringProbeBeatsVerdict(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	source := &recoverVerdictSource{
		verdict: StatusUpdate{Status: StatusThinking},
		ok:      true,
	}
	mgr.SetAgentResolver(&fakeAgentResolver{
		agents: map[string]Agent{"claude": &recoverVerdictAgent{source: source}},
	})

	sess := setupLivePaneSession(t, mgr, mock, "%21", StatusPermission)

	// The agent finished its turn while the daemon was still deciding what to
	// recover.
	mock.onHasSession = func(string) {
		mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	}

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q: the Stop that landed during the probe "+
			"is fresher than a verdict derived before it", got.Status, StatusIdle)
	}
	if source.lastSig.Kind != "recover" {
		t.Fatal("the adapter was never asked for a verdict; the test would pass for the wrong reason")
	}
}

// TestManager_RecoverTmuxSessions_LiveStatusWinsOverDisk verifies that a
// status set by hooks after load (the session is already live in memory)
// is not clobbered by the older on-disk value.
func TestManager_RecoverTmuxSessions_LiveStatusWinsOverDisk(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess := setupLivePaneSession(t, mgr, mock, "%15", StatusIdle)
	mgr.mu.Lock()
	sess.Status = StatusThinking // a hook fired after load
	mgr.mu.Unlock()

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusThinking {
		t.Errorf("Status = %q, want live %q", got.Status, StatusThinking)
	}
}

// TestManager_RecoverTmuxSessions_AfterReload verifies the full daemon-restart
// path: the status persisted by a previous Manager instance survives the
// load-time Stopped normalization and is restored when the pane is alive.
func TestManager_RecoverTmuxSessions_AfterReload(t *testing.T) {
	dir := t.TempDir()
	configDir := t.TempDir()
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}

	mgr1, err := NewManager(dir, configDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	sess, _, err := mgr1.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-reload", Description: "rreload"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	innerName := "jin_" + sess.ID
	sess.TmuxWindowName = innerName
	sess.TmuxPaneID = "%16"
	sess.Status = StatusIdle
	if err := mgr1.store.Save(*sess); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	mgr2, err := NewManager(dir, configDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager (reload) failed: %v", err)
	}
	mock := newMockTmuxRunner()
	mgr2.SetTmuxClient(mock)
	mgr2.SetAgentResolver(newFakeAgentResolver())
	mock.sessions[innerName] = true
	mock.deadPanes["%16"] = false

	mgr2.RecoverTmuxSessions()

	got, ok := mgr2.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want persisted %q restored after reload", got.Status, StatusIdle)
	}
}

func TestManager_RecoverTmuxSessions_NoResolver(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	mgr.SetAgentResolver(nil)

	sess := setupLivePaneSession(t, mgr, mock, "%14", StatusIdle)

	// Must not panic; the preserve path alone decides.
	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
}

func TestManager_RecoverTmuxSessions_NoTmux(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Explicitly set tmuxClient to nil to simulate no tmux available.
	mgr.SetTmuxClient(nil)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-notmux", Description: "rnotmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	mgr.mu.Unlock()

	// Should be a no-op and not panic.
	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	// Status should remain unchanged (StatusStopped from creation).
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

// TestManager_RecoverTmuxSessions_SkipsSessionStartedByThisDaemon verifies
// the apply-phase StartedAt guard: a non-zero StartedAt means this daemon
// process started the session itself, so a recovery decision derived from
// pre-restart observations must not be applied. The probe here reports the
// window gone — without the guard, recovery would clear TmuxWindowName and
// stop the freshly started session.
func TestManager_RecoverTmuxSessions_SkipsSessionStartedByThisDaemon(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := setupLivePaneSession(t, mgr, mock, "%20", "")

	mgr.mu.Lock()
	windowName := sess.TmuxWindowName
	sess.Status = StatusRunning
	sess.StartedAt = time.Now()
	mgr.mu.Unlock()
	mock.sessions[windowName] = false // probe would conclude recoverWindowGone

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want untouched %q", got.Status, StatusRunning)
	}
	if got.TmuxWindowName != windowName {
		t.Errorf("TmuxWindowName = %q, want untouched %q", got.TmuxWindowName, windowName)
	}
}

// TestManager_RecoverTmuxSessions_KillDuringProbe verifies the apply-phase
// re-validation: a session killed while the unlocked probes run must not be
// resurrected by the stale "pane alive" observation. The kill lands before the
// pane probe here, so the decision itself already turns into recoverPaneDead —
// which keeps TmuxWindowName, exactly as Kill left it.
func TestManager_RecoverTmuxSessions_KillDuringProbe(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := setupLivePaneSession(t, mgr, mock, "%21", StatusThinking)

	mgr.mu.RLock()
	windowName := sess.TmuxWindowName
	mgr.mu.RUnlock()

	mock.onHasSession = func(string) {
		if err := mgr.Kill(sess.ID); err != nil {
			t.Errorf("Kill failed: %v", err)
		}
	}

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q (killed mid-probe)", got.Status, StatusStopped)
	}
	if got.TmuxWindowName != windowName {
		t.Errorf("TmuxWindowName = %q, want it kept as %q so the session can be revived in place", got.TmuxWindowName, windowName)
	}
}

// TestManager_RecoverTmuxSessions_KillAfterPaneProbe drives the kill into the
// last gap there is: after every probe has answered "pane alive", with the
// resume decision already made. The window-name check cannot catch this one —
// Kill keeps the name — so the status guard is what has to hold, or recovery
// hands the user back a session they just stopped.
func TestManager_RecoverTmuxSessions_KillAfterPaneProbe(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := setupLivePaneSession(t, mgr, mock, "%22", StatusThinking)

	mock.onIsPaneDead = func(string) {
		if err := mgr.Kill(sess.ID); err != nil {
			t.Errorf("Kill failed: %v", err)
		}
	}

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q; the resume decision predates the kill", got.Status, StatusStopped)
	}
}

// TestManager_RecoverTmuxSessions_DeleteDuringProbe verifies the apply-phase
// existence guard: a session deleted while the unlocked probes run is
// silently skipped instead of being resurrected (or dereferenced as nil).
func TestManager_RecoverTmuxSessions_DeleteDuringProbe(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := setupLivePaneSession(t, mgr, mock, "%22", StatusIdle)

	mock.onHasSession = func(string) {
		if err := mgr.Delete(sess.ID, false, false); err != nil {
			t.Errorf("Delete failed: %v", err)
		}
	}

	mgr.RecoverTmuxSessions()

	if _, ok := mgr.Get(sess.ID); ok {
		t.Error("session still present after mid-probe delete")
	}
}

// TestManager_CreateWithOptions_SaveFailure_NotRegistered verifies the
// compensating delete: when the store write fails, the session must not stay
// registered, preserving the invariant that a returned session is both
// registered and persisted.
func TestManager_CreateWithOptions_SaveFailure_NotRegistered(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod cannot make the dir unwritable")
	}
	mgr, _, _ := newTestManager(t)

	// Store.Save creates its temp file inside the data dir; removing the
	// write bit forces the failure.
	if err := os.Chmod(mgr.store.dataDir, 0o500); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(mgr.store.dataDir, 0o700) })

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/save-fail"})
	if err == nil {
		t.Fatal("expected error when store dir is unwritable")
	}
	if n := len(mgr.List()); n != 0 {
		t.Errorf("got %d registered sessions after failed save, want 0", n)
	}
}

// TestManager_ConcurrentRecoveryAndMutators runs recovery against concurrent
// mutators under -race, with a session start fired deterministically inside
// the probe window (via onHasSession) so the apply-phase guards face a real
// interleaving instead of relying on scheduler luck. The started session must
// keep its live Running state: recovery snapshotted it windowless (a stale
// markStopped decision) and the StartedAt guard has to discard that.
func TestManager_ConcurrentRecoveryAndMutators(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := setupLivePaneSession(t, mgr, mock, "%30", StatusIdle)
	startable, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "startable"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Fires during decideRecovery, after the snapshot and before apply.
	// Calling StartBackground here is safe: recovery was entered directly,
	// not via ensureTmuxClient, so tmuxInitMu is not held during the probe.
	mock.onHasSession = func(string) {
		if err := mgr.StartBackground(startable.ID); err != nil {
			t.Errorf("StartBackground failed: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr.RecoverTmuxSessions()
	}()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			switch n % 3 {
			case 0:
				mgr.SetStatusWithError(sess.ID, StatusThinking, "")
			case 1:
				_ = mgr.SetDescription(sess.ID, fmt.Sprintf("desc-%d", n))
			case 2:
				_ = mgr.SetWorkDir(sess.ID, "")
			}
		}(i)
	}
	wg.Wait()

	got, ok := mgr.Get(startable.ID)
	if !ok {
		t.Fatal("startable session missing after recovery")
	}
	if got.Status != StatusRunning {
		t.Errorf("mid-probe-started session Status = %q, want %q (stale markStopped must be discarded)", got.Status, StatusRunning)
	}
}

// TestManager_ConcurrentHookEventsAndKill runs HandleHookEvent and Kill
// against the same session concurrently with -race. No assertions beyond "no
// race, no panic": the interleavings themselves are the test. This is the
// regression coverage for the Store.Save-by-value migration — HandleHookEvent
// and Kill used to hand Store.Save a live *Session after unlocking, which
// raced against each other's field writes.
func TestManager_ConcurrentHookEventsAndKill(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "concurrent-hook-kill"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			switch n % 4 {
			case 0:
				mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
			case 1:
				mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
			case 2:
				_ = mgr.SetDescription(sess.ID, fmt.Sprintf("desc-%d", n))
			case 3:
				_ = mgr.Kill(sess.ID)
			}
		}(i)
	}
	wg.Wait()
}

// TestManager_MarkIdleFallbackLocked exercises the guard captureOutputTmux's
// idle fallback relies on to avoid resurrecting a session that was deleted or
// moved off Running between its RLock snapshot and the Lock this helper runs
// under.
func TestManager_MarkIdleFallbackLocked(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "idle-fallback"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	t.Run("running session transitions to idle", func(t *testing.T) {
		mgr.mu.Lock()
		sess.Status = StatusRunning
		mgr.mu.Unlock()

		mgr.mu.Lock()
		saved, changed := mgr.markIdleFallbackLocked(sess.ID)
		mgr.mu.Unlock()

		if !changed {
			t.Fatal("changed = false, want true for a Running session")
		}
		if saved.Status != StatusIdle {
			t.Errorf("saved.Status = %q, want %q", saved.Status, StatusIdle)
		}
		if got, ok := mgr.Get(sess.ID); !ok || got.Status != StatusIdle {
			t.Errorf("live session Status = %v (ok=%v), want %q", got, ok, StatusIdle)
		}
	})

	t.Run("non-running session is left alone", func(t *testing.T) {
		mgr.mu.Lock()
		sess.Status = StatusThinking
		mgr.mu.Unlock()

		mgr.mu.Lock()
		_, changed := mgr.markIdleFallbackLocked(sess.ID)
		mgr.mu.Unlock()

		if changed {
			t.Fatal("changed = true, want false for a non-Running session")
		}
		if got, ok := mgr.Get(sess.ID); !ok || got.Status != StatusThinking {
			t.Errorf("live session Status = %v (ok=%v), want untouched %q", got, ok, StatusThinking)
		}
	})

	t.Run("deleted session is left alone", func(t *testing.T) {
		if err := mgr.Delete(sess.ID, false, false); err != nil {
			t.Fatalf("delete failed: %v", err)
		}

		mgr.mu.Lock()
		_, changed := mgr.markIdleFallbackLocked(sess.ID)
		mgr.mu.Unlock()

		if changed {
			t.Fatal("changed = true, want false for a deleted session (would resurrect its file)")
		}
	})
}

// ---------------------------------------------------------------------------
// FindByAgentSessionID tests
// ---------------------------------------------------------------------------

func TestManager_FindByAgentSessionID(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/find-cc", Description: "findcc"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Find by the AgentSessionID that was auto-generated during creation.
	got, ok := mgr.FindByAgentSessionID(sess.AgentSessionID)
	if !ok {
		t.Fatal("FindByAgentSessionID returned ok=false for existing session")
	}
	if got.ID != sess.ID {
		t.Errorf("ID = %q, want %q", got.ID, sess.ID)
	}

	// Find with a non-existent AgentSessionID should return nil.
	got2, ok2 := mgr.FindByAgentSessionID("nonexistent-cc-id")
	if ok2 {
		t.Fatal("FindByAgentSessionID returned ok=true for non-existent AgentSessionID")
	}
	if got2 != nil {
		t.Errorf("expected nil session, got %+v", got2)
	}
}

func TestManager_FindByAgentSessionID_NotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Empty manager: should return nil, false.
	got, ok := mgr.FindByAgentSessionID("does-not-exist")
	if ok {
		t.Fatal("FindByAgentSessionID returned ok=true on empty manager")
	}
	if got != nil {
		t.Errorf("expected nil session, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// StartBackground tests
// ---------------------------------------------------------------------------

func TestManager_StartBackground(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	// Use a real temp directory so os.Stat in startSessionTmux passes.
	workDir := t.TempDir()

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "bg"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Configure mock so GetPaneID returns a valid pane ID for the inner session.
	innerName := "sess-" + sess.ID
	mock.paneIDs[innerName] = "%99"

	if err := mgr.StartBackground(sess.ID); err != nil {
		t.Fatalf("StartBackground failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after StartBackground")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
	if got.TmuxWindowName != innerName {
		t.Errorf("TmuxWindowName = %q, want %q", got.TmuxWindowName, innerName)
	}

	// Verify mock tmux calls.
	if !mock.hasCalledWith("NewSessionWithCmdInDir", innerName) {
		t.Error("expected NewSessionWithCmdInDir to be called with inner session name")
	}
}

func TestManager_StartBackground_NotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	err := mgr.StartBackground("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for non-existent session ID, got nil")
	}
}

func TestManager_StartBackground_AlreadyRunning(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	workDir := t.TempDir()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "already"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate a session that's already running (has TmuxWindowName and non-stopped status).
	mgr.mu.Lock()
	sess.TmuxWindowName = "sess-" + sess.ID
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	// StartBackground should succeed without creating a new tmux session.
	if err := mgr.StartBackground(sess.ID); err != nil {
		t.Fatalf("StartBackground failed: %v", err)
	}

	// NewSessionWithCmdInDir should NOT have been called.
	if mock.hasCalledWith("NewSessionWithCmdInDir", "sess-"+sess.ID) {
		t.Error("expected NewSessionWithCmdInDir NOT to be called for already running session")
	}
}

// ---------------------------------------------------------------------------
// SetStatus extended tests
// ---------------------------------------------------------------------------

func TestManager_SetStatus_NonExistent(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Setting status on a non-existent session should not panic.
	mgr.SetStatus("nonexistent-id", StatusThinking)

	// Verify no sessions were created.
	infos := mgr.List()
	if len(infos) != 0 {
		t.Errorf("List returned %d items, want 0", len(infos))
	}
}

func TestManager_SetStatus_Persisted(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/setstatus-persist", Description: "sp"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.SetStatus(sess.ID, StatusThinking)

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusThinking {
		t.Errorf("Status = %q, want %q", got.Status, StatusThinking)
	}
}

// ---------------------------------------------------------------------------
// EnsureTmuxClient not set tests
// ---------------------------------------------------------------------------

func TestManager_EnsureTmuxClient_NotSet(t *testing.T) {
	dir := t.TempDir()
	configDir := t.TempDir()
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	mgr, err := NewManager(dir, configDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	// Deliberately do NOT call SetTmuxClient — tmux client remains nil so
	// ensureTmuxClient exercises the auto-init path.
	//
	// Isolate that auto-init to a unique socket name and kill the resulting
	// server on cleanup. Without this, running on a machine with tmux installed
	// leaves a stray "-L jin" server behind, and the next daemon start reuses it —
	// the server's tmux env (including CLAUDE_CODE_CHILD_SESSION inherited from
	// whatever launched `go test`) then propagates to every CC spawned in that
	// daemon, silently breaking Layer C description enhancement.
	socketName := "jin-test-" + uuid.New().String()[:8]
	mgr.SetTmuxSocketName(socketName)
	mgr.SetAgentResolver(newFakeAgentResolver())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socketName, "kill-server").Run()
		// tmux 3.x does not unlink its socket file on kill-server (or on natural
		// server exit when the last session ends). Remove it ourselves to avoid
		// accumulating stale sockets under $TMUX_TMPDIR/tmux-$UID/ over many
		// test runs.
		tmpdir := os.Getenv("TMUX_TMPDIR")
		if tmpdir == "" {
			tmpdir = "/tmp"
		}
		_ = os.Remove(filepath.Join(tmpdir, fmt.Sprintf("tmux-%d", os.Getuid()), socketName))
	})

	workDir := t.TempDir()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "notmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// StartBackground calls ensureTmuxClient before locking.
	// Without a real tmux binary, startSessionTmux will fail in some way.
	// We mainly want to verify that it does not panic and returns an error.
	err = mgr.StartBackground(sess.ID)
	if err == nil {
		// If no error, the session should at least have a status set.
		// In CI without tmux installed, ensureTmuxClient will fail silently,
		// and startSessionTmux will likely error on tmux commands.
		// Either way, we verified no panic.
		t.Log("StartBackground succeeded (tmux may be installed), verifying no panic")
	}
}

// ---------------------------------------------------------------------------
// Kill edge case tests
// ---------------------------------------------------------------------------

func TestManager_Kill_NotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	err := mgr.Kill("nonexistent-session-id")
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not contain 'not found'", err.Error())
	}
}

func TestManager_Kill_WithTmuxWindowOnly(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-win", Description: "killwin"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate session with TmuxWindowName but no TmuxPaneID (fallback path)
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.TmuxPaneID = "" // no pane ID
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	mock.sessions[sess.TmuxWindowName] = true

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	// Should have called KillSession (fallback when no pane ID)
	if !mock.hasCalledWith("KillSession", "jin_"+sess.ID) {
		t.Error("expected KillSession to be called with inner session name")
	}
}

func TestManager_Kill_NoTmux(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-notmux", Description: "killnotmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Session has no tmux window or pane; Kill should still mark it stopped.
	mgr.mu.Lock()
	sess.Status = StatusThinking
	mgr.mu.Unlock()

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

// ---------------------------------------------------------------------------
// Delete edge case tests
// ---------------------------------------------------------------------------

func TestManager_Delete_NotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	err := mgr.Delete("nonexistent-session-id", false, false)
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not contain 'not found'", err.Error())
	}
}

func TestManager_Delete_Success(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Create two sessions
	sess1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s1", Description: "dels1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s2", Description: "dels2"})
	if err != nil {
		t.Fatalf("create sess2 failed: %v", err)
	}

	// Delete the first session
	if err := mgr.Delete(sess1.ID, false, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify it's gone from Get
	_, ok := mgr.Get(sess1.ID)
	if ok {
		t.Fatal("Get returned ok=true after Delete")
	}

	// Verify it's gone from List
	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("List returned %d items, want 1", len(infos))
	}
	if infos[0].Description != "dels2" {
		t.Errorf("remaining session Name = %q, want %q", infos[0].Description, "dels2")
	}
}

func TestManager_Delete_WithTmuxSession(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-tmux", Description: "deltmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate session with active tmux
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	mock.sessions[sess.TmuxWindowName] = true

	if err := mgr.Delete(sess.ID, false, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Should have called KillSession on the inner tmux session
	if !mock.hasCalledWith("KillSession", "jin_"+sess.ID) {
		t.Error("expected KillSession to be called when deleting a session with tmux")
	}

	// Verify it's gone
	_, ok := mgr.Get(sess.ID)
	if ok {
		t.Fatal("Get returned ok=true after Delete")
	}
}

func TestNewManager_LoadAll_MigratesEmptyFleet(t *testing.T) {
	dataDir := t.TempDir()
	configDir := t.TempDir()

	// Write a session JSON without the fleet field (simulates old data).
	// The same fixture also exercises the name → description migration.
	oldJSON := `{"id":"old-id","name":"old","work_dir":"/tmp/old","created_at":"2025-01-01T00:00:00Z","status":"idle","claude_session_id":"cid"}`
	if err := os.WriteFile(filepath.Join(dataDir, "old-id.json"), []byte(oldJSON), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	mgr, err := NewManager(dataDir, configDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("List returned %d items, want 1", len(infos))
	}
	if infos[0].Fleet != DefaultFleet {
		t.Errorf("Fleet = %q, want %q", infos[0].Fleet, DefaultFleet)
	}
	// Legacy "name" value must be preserved as the new Description.
	if infos[0].Description != "old" {
		t.Errorf("Description = %q, want %q (legacy name should migrate into description)", infos[0].Description, "old")
	}
	// Migrated descriptions are conservatively locked so Layer C doesn't
	// overwrite a value the user had already chosen manually.
	if !infos[0].DescriptionLocked {
		t.Error("DescriptionLocked = false, want true (migrated name should be locked)")
	}
}

// TestNewManager_LoadAll_WritesBackMigratedJSON verifies that the on-disk
// fixture is rewritten in place after migration: the legacy "name" key is
// removed and "description" / "description_locked" replace it. This locks in
// the spec-accepted behaviour that the migration is idempotent and observable
// on disk (spec receipt criterion 13).
func TestNewManager_LoadAll_WritesBackMigratedJSON(t *testing.T) {
	dataDir := t.TempDir()
	configDir := t.TempDir()

	fixturePath := filepath.Join(dataDir, "old-id.json")
	oldJSON := `{"id":"old-id","name":"old","work_dir":"/tmp/old","created_at":"2025-01-01T00:00:00Z","status":"idle","claude_session_id":"cid"}`
	if err := os.WriteFile(fixturePath, []byte(oldJSON), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	if _, err := NewManager(dataDir, configDir, testIdentity(), configMgr); err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	if _, hasName := got["name"]; hasName {
		t.Error(`migrated JSON still contains "name" key`)
	}
	if desc, _ := got["description"].(string); desc != "old" {
		t.Errorf(`"description" = %q, want "old"`, desc)
	}
	if locked, _ := got["description_locked"].(bool); !locked {
		t.Error(`"description_locked" = false, want true`)
	}
}

func TestManager_List_Empty(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	infos := mgr.List()
	if len(infos) != 0 {
		t.Fatalf("List on empty manager returned %d items, want 0", len(infos))
	}
}

// ---------------------------------------------------------------------------
// EnsureClaudeTrustState tests
// ---------------------------------------------------------------------------

// ensureClaudeTrustState moved to internal/agent/claude/ as EnsureTrustState;
// see internal/agent/claude/trust_test.go for the tests.

// ---------------------------------------------------------------------------
// Idle fallback tests (hook-timeout detection in captureOutputTmux)
// ---------------------------------------------------------------------------

// TestManager_IdleFallback_FreshStart verifies that a session in StatusRunning
// with a non-zero StartedAt and stale LastOutputTime satisfies the fallback
// condition that captureOutputTmux uses to transition running → idle.
func TestManager_IdleFallback_FreshStart(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-fresh", Description: "ifb-fresh"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate a fresh start: status running, StartedAt and LastOutputTime set 31s ago.
	mgr.mu.Lock()
	sess.Status = StatusRunning
	sess.StartedAt = time.Now().Add(-31 * time.Second)
	sess.LastOutputTime = time.Now().Add(-31 * time.Second)
	mgr.mu.Unlock()

	const hookIdleTimeout = 30 * time.Second

	mgr.mu.RLock()
	fbStatus := sess.Status
	fbLastOutput := sess.LastOutputTime
	fbStartedAt := sess.StartedAt
	mgr.mu.RUnlock()

	// The condition must be true for the fallback to fire.
	if !(fbStatus == StatusRunning && !fbStartedAt.IsZero() && time.Since(fbLastOutput) > hookIdleTimeout) {
		t.Fatal("expected idle fallback condition to be true for a fresh start with stale LastOutputTime")
	}

	// Apply the same transition captureOutputTmux would perform.
	mgr.mu.Lock()
	if _, exists := mgr.sessions[sess.ID]; exists && sess.Status == StatusRunning {
		sess.Status = StatusIdle
		sess.LastOutputTime = time.Now()
	}
	mgr.mu.Unlock()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session not found after fallback transition")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
}

// TestManager_IdleFallback_DaemonRecovery verifies that sessions recovered after
// a daemon restart (StartedAt == zero) do NOT satisfy the fallback condition,
// preventing false idle transitions while a task may still be running.
func TestManager_IdleFallback_DaemonRecovery(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-recover", Description: "ifb-recover"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate daemon recovery: StartedAt is zero (json:"-" field, never set on recovery).
	mgr.mu.Lock()
	sess.Status = StatusRunning
	// sess.StartedAt is zero by default — as it would be after daemon restart.
	sess.LastOutputTime = time.Now().Add(-31 * time.Second)
	mgr.mu.Unlock()

	const hookIdleTimeout = 30 * time.Second

	mgr.mu.RLock()
	fbStatus := sess.Status
	fbLastOutput := sess.LastOutputTime
	fbStartedAt := sess.StartedAt
	mgr.mu.RUnlock()

	// The condition must be false (StartedAt.IsZero() == true), so fallback does not fire.
	shouldFallback := fbStatus == StatusRunning && !fbStartedAt.IsZero() && time.Since(fbLastOutput) > hookIdleTimeout
	if shouldFallback {
		t.Error("idle fallback condition should be false when StartedAt is zero (daemon recovery)")
	}

	// Status must remain running.
	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session not found")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q (should not change without hook)", got.Status, StatusRunning)
	}
}

func TestManager_HandleHookEvent_CWDUpdate_WorktreePathSkipped(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	origDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(origDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Description: "wt-skip"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate Claude Code's EnterWorktree: CWD moves to .claude/worktrees/xxx
	worktreeDir := filepath.Join(origDir, ".claude", "worktrees", "feat-xyz")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// Worktree has a .git file (as real worktrees do)
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: ../../.git/worktrees/feat-xyz"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "CwdChanged", "", worktreeDir, "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	// WorkDir must NOT be updated to the worktree path
	if got.WorkDir != origDir {
		t.Errorf("WorkDir = %q, want %q (should not update for .claude/worktrees path)", got.WorkDir, origDir)
	}
	// CurrentWorkDir should still be updated
	if got.CurrentWorkDir != worktreeDir {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, worktreeDir)
	}
}

// ---------------------------------------------------------------------------
// Worktree removal tests (Delete + removeGitWorktree)
// ---------------------------------------------------------------------------

// setupTestWorktree initializes a fresh git repo at a temp dir and adds a
// worktree under it. Returns (mainRepoDir, worktreeDir). Skips the test if
// `git` is not on PATH.
func setupTestWorktree(t *testing.T) (string, string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
		}
	}

	runGit("init")
	runGit("config", "user.email", "test@test.com")
	runGit("config", "user.name", "test")

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	worktreeDir := filepath.Join(repoDir, "wt")
	runGit("worktree", "add", worktreeDir, "-b", "test-branch")

	return repoDir, worktreeDir
}

// TestManager_RepoName_ProvisionAsync covers the write the whole RepoName
// feature was built for: `jin new --worktree` puts the session in a directory
// named after the session ("jin-b63188fe"), and the detail pane must show the
// repo that worktree was cut from instead.
//
// ReserveCreation cannot cover it — at that point WorkDir is still the repo
// root, so it would resolve correctly by accident. Only ProvisionAsync sees
// the final worktree path.
func TestManager_RepoName_ProvisionAsync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	mgr, _, _ := newTestManager(t)

	// A decoy — see the same line in setupHookTest.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	repoDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	// `worktree add` is mocked, but it has to leave behind the thing
	// ResolveRepoName reads: a directory whose .git is a FILE pointing back
	// into <main-repo>/.git/worktrees/<name>. Without that the test would pass
	// against a broken implementation, since a missing dir resolves to "".
	var createdWorktree string
	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 1 && args[0] == "fetch":
				return nil, nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				return nil, errors.New("exit status 1")
			case len(args) >= 5 && args[0] == "worktree" && args[1] == "add":
				createdWorktree = args[4]
				name := filepath.Base(createdWorktree)
				if err := os.MkdirAll(createdWorktree, 0o755); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(filepath.Join(repoDir, ".git", "worktrees", name), 0o755); err != nil {
					return nil, err
				}
				gitFile := "gitdir: " + filepath.Join(repoDir, ".git", "worktrees", name) + "\n"
				return nil, os.WriteFile(filepath.Join(createdWorktree, ".git"), []byte(gitFile), 0o644)
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
	mgr.gitClient = git.NewClientWithRunner(runner)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     repoDir,
		Description: "wt-provision",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if createdWorktree == "" {
		t.Fatal("the worktree add mock never ran")
	}

	info, ok := mgr.GetInfo(sess.ID)
	if !ok {
		t.Fatal("GetInfo missed the session")
	}
	if info.WorkDir != createdWorktree {
		t.Fatalf("WorkDir = %q, want the provisioned worktree %q", info.WorkDir, createdWorktree)
	}
	if info.RepoName != filepath.Base(repoDir) {
		t.Errorf("after provisioning, Info.RepoName = %q, want the main repo %q", info.RepoName, filepath.Base(repoDir))
	}
	if info.RepoName == filepath.Base(createdWorktree) {
		t.Errorf("Info.RepoName = %q — that is the worktree directory, which is exactly what this feature exists to avoid", info.RepoName)
	}
	// The contract ProvisionAsync's write actually maintains: RepoName always
	// describes the session's current WorkDir. It happens to be satisfied
	// already by the value ReserveCreation seeded (a worktree resolves to the
	// repo it was cut from, which is the same repo root the reservation saw),
	// so this pairing — not the individual assignment — is what is worth
	// pinning. It catches the two ways it could break later: WorkDir moving
	// without RepoName following, and the reserve-time seed diverging.
	if want := ResolveRepoName(info.WorkDir); info.RepoName != want {
		t.Errorf("Info.RepoName = %q but its WorkDir %q resolves to %q — the two have drifted apart",
			info.RepoName, info.WorkDir, want)
	}
}

// TestManager_RepoName_UpdateGitBranch covers the poll-time write: the agent
// can cd anywhere, and the pane must describe where it actually is.
func TestManager_RepoName_UpdateGitBranch(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	repoDir, worktreeDir := setupTestWorktree(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: repoDir, Description: "cd-follow"})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	live, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session vanished after create")
	}

	read := func() (string, string) {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		return live.RepoName, live.CurrentBranch
	}

	t.Run("follows a cd into a different repo", func(t *testing.T) {
		// A different repo, not another worktree of the same one: the value
		// seeded at create time must actually be replaced, otherwise the test
		// would pass against an implementation that never writes here.
		otherRepo, _ := setupTestWorktree(t)
		mgr.updateGitBranch(live, otherRepo, repoDir)
		gotRepo, _ := read()
		if gotRepo != filepath.Base(otherRepo) {
			t.Errorf("RepoName = %q, want the repo the agent moved into, %q", gotRepo, filepath.Base(otherRepo))
		}
		if gotRepo == filepath.Base(repoDir) {
			t.Errorf("RepoName is still the create-time repo %q — the poll never followed the cd", gotRepo)
		}
	})

	t.Run("follows a cd into a worktree, reporting the main repo", func(t *testing.T) {
		mgr.updateGitBranch(live, worktreeDir, repoDir)
		gotRepo, gotBranch := read()
		if gotRepo != filepath.Base(repoDir) {
			t.Errorf("RepoName = %q, want the main repo %q", gotRepo, filepath.Base(repoDir))
		}
		if gotBranch != "test-branch" {
			t.Errorf("CurrentBranch = %q, want %q", gotBranch, "test-branch")
		}
	})

	t.Run("clears on a cd out of any repo", func(t *testing.T) {
		nonGit := t.TempDir()
		mgr.updateGitBranch(live, nonGit, worktreeDir)
		gotRepo, gotBranch := read()
		if gotRepo != "" {
			t.Errorf("RepoName = %q, want empty outside a repo — a stale repo name is worse than none", gotRepo)
		}
		if gotBranch != "" {
			t.Errorf("CurrentBranch = %q, want empty", gotBranch)
		}
	})
}

func TestManager_Delete_RemovesWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, worktreeDir := setupTestWorktree(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Description: "wt"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := mgr.Delete(sess.ID, true, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree directory should be removed, but still exists: stat err=%v", err)
	}
}

func TestManager_Delete_PrefersCurrentWorkDirForWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mainRepo, worktreeDir := setupTestWorktree(t)

	// Reproduce the bug: WorkDir points at the main repo (because the
	// fix-worktree-workdir-overwrite guard prevents WorkDir from being
	// updated to a worktree path), while CurrentWorkDir tracks the actual
	// worktree the session is in.
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: mainRepo, Description: "wt-current"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.CurrentWorkDir = worktreeDir
	mgr.mu.Unlock()

	if err := mgr.Delete(sess.ID, true, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree directory should be removed, but still exists: stat err=%v", err)
	}
	// Main repo must remain intact.
	if _, err := os.Stat(filepath.Join(mainRepo, ".git")); err != nil {
		t.Errorf("main repo .git should still exist: %v", err)
	}
}

func TestManager_Delete_NonWorktreeReturnsError(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mainRepo, _ := setupTestWorktree(t)

	// Both WorkDir and CurrentWorkDir point at the main repo (no worktree).
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: mainRepo, Description: "main-only"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	err = mgr.Delete(sess.ID, true, false)
	if !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("expected ErrNotWorktree, got: %v", err)
	}

	// Session must still exist (Delete aborted before tmux kill / store removal).
	if _, ok := mgr.Get(sess.ID); !ok {
		t.Error("session should still exist after ErrNotWorktree")
	}
	// Main repo must remain intact.
	if _, err := os.Stat(filepath.Join(mainRepo, ".git")); err != nil {
		t.Errorf("main repo .git should still exist: %v", err)
	}
}

func TestManager_Delete_DirtyWorktreeReturnsErrWorktreeDirty(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, worktreeDir := setupTestWorktree(t)

	if err := os.WriteFile(filepath.Join(worktreeDir, "dirty.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Description: "wt-dirty"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	err = mgr.Delete(sess.ID, true, false)
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("expected ErrWorktreeDirty, got: %v", err)
	}

	if _, err := os.Stat(worktreeDir); err != nil {
		t.Errorf("worktree should still exist after dirty rejection: %v", err)
	}

	// Force removal should succeed.
	if err := mgr.Delete(sess.ID, true, true); err != nil {
		t.Fatalf("force Delete failed: %v", err)
	}
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after force delete: stat err=%v", err)
	}
}

// A worktree removal that fails for a reason other than dirty/not-a-worktree
// (permissions, filesystem, git exec) must abort the delete rather than report
// success while the directory is still on disk. Covers force too: with force
// the dirty classification is skipped entirely (internal/git/worktree.go), so
// every git failure lands on this path.
func TestManager_Delete_WorktreeRemovalFailureAbortsDelete(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "force=false"
		if force {
			name = "force=true"
		}
		t.Run(name, func(t *testing.T) {
			mgr, tmuxMock, _ := newTestManager(t)
			_, worktreeDir := setupTestWorktree(t)

			sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Description: "wt-fail"})
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}
			innerName := tmux.InnerSessionName(sess.ID)
			mgr.mu.Lock()
			sess.TmuxWindowName = innerName
			mgr.mu.Unlock()

			// Dirty-looking output under force must still land on the wrapped
			// path, not ErrWorktreeDirty.
			gitOutput := "fatal: could not remove: permission denied"
			if force {
				gitOutput = "fatal: 'wt' contains modified or untracked files, use --force to delete it"
			}

			// Fail only `git worktree remove`; the pre-flight .git checks still
			// run against the real worktree created above. The injected output
			// deliberately omits the path so that finding it in the returned
			// error proves the wrapper added it.
			runner := &scriptedGitRunner{
				handler: func(dir string, args []string) ([]byte, error) {
					if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
						return []byte(gitOutput), errors.New("exit status 128")
					}
					return nil, nil
				},
			}
			mgr.gitClient = git.NewClientWithRunner(runner)

			err = mgr.Delete(sess.ID, true, force)
			if err == nil {
				t.Fatal("Delete should fail when worktree removal fails")
			}
			if got := runner.hadCall("worktree", "remove", "--force"); got != force {
				t.Errorf("--force propagation: got %v, want %v", got, force)
			}
			if !strings.HasPrefix(err.Error(), "removing git worktree at ") {
				t.Errorf("error should carry the removal context, got: %v", err)
			}
			if !strings.Contains(err.Error(), worktreeDir) {
				t.Errorf("error should name the leftover worktree path %q, got: %v", worktreeDir, err)
			}

			// Must not masquerade as a sentinel. The daemon client restores
			// sentinels from the error string across IPC, so a substring
			// collision would tell the user to retry with --force-worktree for
			// an unrelated failure.
			if errors.Is(err, ErrWorktreeDirty) || errors.Is(err, ErrNotWorktree) {
				t.Errorf("unrelated failure must not be classified as a sentinel: %v", err)
			}
			if strings.Contains(err.Error(), ErrWorktreeDirty.Error()) ||
				strings.Contains(err.Error(), ErrNotWorktree.Error()) {
				t.Errorf("error text must not contain a sentinel message: %q", err.Error())
			}

			// Aborting must leave no side effects: session (in memory and on
			// disk), directory and tmux all intact.
			if _, ok := mgr.Get(sess.ID); !ok {
				t.Error("session should still exist after worktree removal failure")
			}
			if _, err := mgr.store.Load(sess.ID); err != nil {
				t.Errorf("session should still be persisted: %v", err)
			}
			if _, err := os.Stat(worktreeDir); err != nil {
				t.Errorf("worktree should still exist: %v", err)
			}
			for _, c := range tmuxMock.calls {
				if c.method == "KillSession" {
					t.Errorf("tmux session should not be killed when the delete aborts, got call: %+v", c)
				}
			}
		})
	}
}

func TestRemoveGitWorktree_AlreadyDeletedIsIdempotent(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, worktreeDir := setupTestWorktree(t)

	// Remove the worktree directory out-of-band.
	if err := os.RemoveAll(worktreeDir); err != nil {
		t.Fatalf("pre-remove worktree: %v", err)
	}

	if err := mgr.removeGitWorktree(worktreeDir, false); err != nil {
		t.Errorf("removeGitWorktree on missing dir should be nil, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateWithOptions worktree branch tests
// ---------------------------------------------------------------------------

// scriptedGitRunner is a git.Runner test double that dispatches on the args
// via a user-supplied handler and records every call for later assertions.
type scriptedGitRunner struct {
	mu      sync.Mutex
	calls   [][]string
	handler func(dir string, args []string) ([]byte, error)
}

func (r *scriptedGitRunner) Run(dir string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	if r.handler != nil {
		return r.handler(dir, args)
	}
	return nil, nil
}

// hadCall reports whether any recorded call starts with the given argv prefix.
func (r *scriptedGitRunner) hadCall(prefix ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if len(call) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if call[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// findCall returns the first recorded argv that starts with prefix, or nil.
func (r *scriptedGitRunner) findCall(prefix ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if len(call) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if call[i] != p {
				match = false
				break
			}
		}
		if match {
			return call
		}
	}
	return nil
}

func TestManager_CreateWithOptions_Worktree_RejectsNonGitRepo(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     t.TempDir(), // no .git present
		Description: "nogit-wt",
		Worktree:    true,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q should mention 'not a git repository'", err.Error())
	}
}

func TestManager_CreateWithOptions_Worktree_HappyPath(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// A decoy: worktree placement comes from the state dir this Manager was
	// built over, so pointing the process-wide one elsewhere makes the
	// mgr.stateDir assertion below fail if placement ever goes back to reading
	// the global.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 1 && args[0] == "fetch":
				return nil, nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				// Branch does not exist — no collision.
				return nil, errors.New("exit status 1")
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
	mgr.gitClient = git.NewClientWithRunner(runner)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     workDir,
		Description: "wt-happy",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	wantPrefix := filepath.Join(mgr.stateDir, "worktrees", "jin-")
	if !strings.HasPrefix(sess.WorkDir, wantPrefix) {
		t.Errorf("WorkDir = %q, want prefix %q", sess.WorkDir, wantPrefix)
	}
	suffix := strings.TrimPrefix(sess.WorkDir, wantPrefix)
	if len(suffix) != 8 {
		t.Errorf("worktree suffix = %q (len %d), want 8 hex chars", suffix, len(suffix))
	}

	// Assert that we resolved the default branch via symbolic-ref.
	if !runner.hadCall("symbolic-ref", "refs/remotes/origin/HEAD") {
		t.Error("expected `git symbolic-ref refs/remotes/origin/HEAD` to be called")
	}

	// Auto-fetch on worktree creation was removed (feat/worktree-offline-creation);
	// the local origin/<base> tip is used as-is, and users refresh manually or via
	// the post-create hook.
	if runner.findCall("fetch") != nil {
		t.Error("expected no `git fetch` call (auto-fetch is disabled)")
	}

	// Assert AddWorktree used the auto-generated branch (jin/<8hex>),
	// the resolved worktree path, and origin/main as the base ref.
	addCall := runner.findCall("worktree", "add")
	if addCall == nil {
		t.Fatal("expected `git worktree add ...` to be called")
	}
	// Layout: worktree add -b <branch> <path> <baseRef>
	if len(addCall) != 6 {
		t.Fatalf("worktree add args len = %d, want 6: %v", len(addCall), addCall)
	}
	if addCall[2] != "-b" {
		t.Errorf("worktree add[2] = %q, want -b", addCall[2])
	}
	gotBranch := addCall[3]
	wantBranchPrefix := "jin/"
	if !strings.HasPrefix(gotBranch, wantBranchPrefix) {
		t.Errorf("worktree add branch = %q, want prefix %q", gotBranch, wantBranchPrefix)
	}
	if len(strings.TrimPrefix(gotBranch, wantBranchPrefix)) != 8 {
		t.Errorf("worktree add branch suffix = %q, want 8 hex chars", strings.TrimPrefix(gotBranch, wantBranchPrefix))
	}
	if addCall[4] != sess.WorkDir {
		t.Errorf("worktree add path = %q, want %q", addCall[4], sess.WorkDir)
	}
	if addCall[5] != "origin/main" {
		t.Errorf("worktree add baseRef = %q, want origin/main", addCall[5])
	}
}

// TestManager_CreateWithOptions_Worktree_RollsBackOnWorkDirCollision verifies
// that the worktree/branch created before the sessions map re-lock are cleaned
// up when the post-lock WorkDir uniqueness check fails. Using --worktree-name
// gives us a predictable worktree path so we can pre-register a session at
// that exact location.
func TestManager_CreateWithOptions_Worktree_RollsBackOnWorkDirCollision(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// A decoy — see the same line in the happy-path test.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	// An unset base_dir resolves to worktrees/{name} under the Manager's own
	// state dir, which is what makes this path predictable.
	predictablePath := filepath.Join(mgr.stateDir, "worktrees", "collide-wt")

	// Pre-create a session whose WorkDir is exactly the worktree path we'll
	// try to create below, so the re-lock WorkDir uniqueness check trips.
	if _, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     predictablePath,
		Description: "pre-existing",
	}); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 1 && args[0] == "fetch":
				return nil, nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				// Branch does not exist — override-path pre-check passes.
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
	mgr.gitClient = git.NewClientWithRunner(runner)

	_, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:      workDir,
		Description:  "wt-collide",
		Worktree:     true,
		WorktreeName: "collide-wt",
	})
	if err == nil {
		t.Fatal("expected error from WorkDir collision, got nil")
	}
	if !strings.Contains(err.Error(), "already exists for directory") {
		t.Errorf("error %q should mention directory conflict", err.Error())
	}

	if !runner.hadCall("worktree", "remove") {
		t.Error("expected RemoveWorktree runner call during rollback")
	}
	if !runner.hadCall("branch", "-D") {
		t.Error("expected DeleteBranch runner call during rollback")
	}
}

// ---------------------------------------------------------------------------
// TryUpgradeDescription tests
// ---------------------------------------------------------------------------

// stubEnhancer is a minimal DescriptionEnhancer whose response can be scripted
// per test case. It records how many times TryGenerate was called so the "no-op"
// cases can assert the enhancer was never consulted, and layer defaults to the
// zero value, so a test expecting promotion must set it strictly greater.
//
// during, when set, runs inside TryGenerate. It stands in for the window where
// the real enhancer scans a transcript with m.mu released, and may mutate the
// manager freely — exactly what a concurrent caller can do. got records the
// session TryGenerate was handed, so a test can check it is a snapshot copy.
type stubEnhancer struct {
	response string
	ok       bool
	layer    DescriptionLayer
	calls    int
	during   func()
	got      *Session
}

func (s *stubEnhancer) TryGenerate(sess *Session) (string, DescriptionLayer, bool) {
	s.calls++
	s.got = sess
	if s.during != nil {
		s.during()
	}
	return s.response, s.layer, s.ok
}

func TestManager_TryUpgradeDescription_Locked_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/upgrade-locked",
		Description: "manual label",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !sess.DescriptionLocked {
		t.Fatal("precondition: session created with Description should be locked")
	}

	enh := &stubEnhancer{response: "candidate", ok: true}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "manual label" {
		t.Errorf("Description = %q, want %q (locked → no upgrade)", got.Description, "manual label")
	}
	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0", enh.calls)
	}
}

// TestManager_TryUpgradeDescription_DescriptionDrift_NoOp exercises Guard 1
// from the "direct drift" angle: Description is off-baseline while
// DescriptionLayer is still zero, without staging any prior enhancer promotion.
// The companion RestartGuard test covers the same guard reached through the
// "layer→restart→zero" path; keep both to document the two ways state can
// drift, even though they hit the same short-circuit.
func TestManager_TryUpgradeDescription_DescriptionDrift_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-drift"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Simulate an out-of-band edit that moved the description off the baseline
	// without staging a corresponding DescriptionLayer promotion.
	mgr.mu.Lock()
	sess.Description = "already upgraded"
	mgr.mu.Unlock()

	enh := &stubEnhancer{response: "should not apply", ok: true}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "already upgraded" {
		t.Errorf("Description = %q, want %q (baseline mismatch → no upgrade)", got.Description, "already upgraded")
	}
	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0", enh.calls)
	}
}

func TestManager_TryUpgradeDescription_Success_ApplyCandidate(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-ok"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if sess.DescriptionLocked {
		t.Fatal("precondition: auto-generated description should be unlocked")
	}
	baseline := GenerateBaselineDescription(sess.WorkDir, "", false, "")
	if sess.Description != baseline {
		t.Fatalf("precondition: Description=%q should match baseline %q", sess.Description, baseline)
	}

	enh := &stubEnhancer{response: "auth refactor", ok: true, layer: DescriptionLayerTranscript}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "auth refactor" {
		t.Errorf("Description = %q, want %q", got.Description, "auth refactor")
	}
	if got.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false (Layer C must not lock)")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}

	// The store must reflect the new description so a daemon restart preserves it.
	reloaded, err := mgr.store.Load(sess.ID)
	if err != nil {
		t.Fatalf("store.Load failed: %v", err)
	}
	if reloaded.Description != "auth refactor" {
		t.Errorf("persisted Description = %q, want %q", reloaded.Description, "auth refactor")
	}
}

func TestManager_TryUpgradeDescription_EnhancerPending_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-pending"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	before := sess.Description

	enh := &stubEnhancer{response: "", ok: false}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != before {
		t.Errorf("Description = %q, want %q (enhancer pending → keep baseline)", got.Description, before)
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

func TestManager_TryUpgradeDescription_NilEnhancer_NoPanic(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-nil"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	before := sess.Description

	// Must not panic even when Manager has no enhancer wired.
	mgr.TryUpgradeDescription(sess.ID, nil)

	got, _ := mgr.Get(sess.ID)
	if got.Description != before {
		t.Errorf("Description = %q, want %q (nil enhancer → no-op)", got.Description, before)
	}
}

func TestManager_TryUpgradeDescription_UnknownSession_NoPanic(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	enh := &stubEnhancer{response: "x", ok: true}
	mgr.TryUpgradeDescription("does-not-exist", enh)

	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0 (unknown session should short-circuit)", enh.calls)
	}
}

// ---------------------------------------------------------------------------
// TryUpgradeDescription: lock-free I/O phase
// ---------------------------------------------------------------------------

// requireUnlocked fails the test instead of deadlocking when m.mu is still held
// while the enhancer runs. The stubEnhancer during callbacks below take m.mu,
// so without this they would hang until the test binary times out rather than
// report a clear failure.
//
// Must be called from the test goroutine: t.Fatal is only valid there.
func requireUnlocked(t *testing.T, mgr *Manager) {
	t.Helper()
	if !mgr.mu.TryLock() {
		t.Fatal("m.mu held while the enhancer ran; the I/O must happen outside the lock")
	}
	mgr.mu.Unlock()
}

// TestManager_TryUpgradeDescription_EnhancerRunsWithoutLock is the core
// regression test for this change: the enhancer performs unbounded file I/O,
// so running it under the Manager's central lock stalls every other session
// for the duration. TryLock succeeding proves the lock is free.
func TestManager_TryUpgradeDescription_EnhancerRunsWithoutLock(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-unlocked"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	enh := &stubEnhancer{
		response: "candidate",
		ok:       true,
		layer:    DescriptionLayerTranscript,
		during:   func() { requireUnlocked(t, mgr) },
	}
	mgr.TryUpgradeDescription(sess.ID, enh)

	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
	got, _ := mgr.Get(sess.ID)
	if got.Description != "candidate" {
		t.Errorf("Description = %q, want %q (upgrade should still apply)", got.Description, "candidate")
	}
}

// TestManager_TryUpgradeDescription_EnhancerGetsSnapshotNotLiveSession pins the
// other half of moving the I/O out of the lock: the enhancer runs unlocked, so
// handing it the live session would let it read fields while another goroutine
// writes them. It must receive an independent copy.
func TestManager_TryUpgradeDescription_EnhancerGetsSnapshotNotLiveSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-snapshot"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	enh := &stubEnhancer{response: "candidate", ok: true, layer: DescriptionLayerTranscript}
	mgr.TryUpgradeDescription(sess.ID, enh)

	mgr.mu.Lock()
	live := mgr.sessions[sess.ID]
	mgr.mu.Unlock()

	if enh.got == nil {
		t.Fatal("enhancer was never called")
	}
	if enh.got == live {
		t.Error("enhancer received the live session; it must get a snapshot copy")
	}
}

// TestManager_TryUpgradeDescription_DeletedDuringIO_NoWriteback covers the
// session disappearing while the lock is released: the write-back must be
// dropped rather than resurrecting a deleted session.
func TestManager_TryUpgradeDescription_DeletedDuringIO_NoWriteback(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-deleted"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	id := sess.ID

	enh := &stubEnhancer{
		response: "candidate",
		ok:       true,
		layer:    DescriptionLayerTranscript,
		during: func() {
			requireUnlocked(t, mgr)
			mgr.mu.Lock()
			delete(mgr.sessions, id)
			mgr.mu.Unlock()
		},
	}
	mgr.TryUpgradeDescription(id, enh)

	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
	if _, ok := mgr.Get(id); ok {
		t.Error("session reappeared in the manager after being deleted mid-upgrade")
	}
	reloaded, err := mgr.store.Load(id)
	if err != nil {
		t.Fatalf("store.Load failed: %v", err)
	}
	if reloaded.Description == "candidate" {
		t.Errorf("persisted Description = %q; the write-back should have been dropped", reloaded.Description)
	}
}

// TestManager_TryUpgradeDescription_ManualLockDuringIO_KeepsManualValue covers
// the user renaming a session while the enhancer is running. SetDescription
// sets DescriptionLocked, so the re-checked guard must discard our candidate.
func TestManager_TryUpgradeDescription_ManualLockDuringIO_KeepsManualValue(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-manual"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	id := sess.ID

	enh := &stubEnhancer{
		response: "candidate",
		ok:       true,
		layer:    DescriptionLayerTranscript,
		during: func() {
			requireUnlocked(t, mgr)
			if err := mgr.SetDescription(id, "manual label"); err != nil {
				t.Errorf("SetDescription failed: %v", err)
			}
		},
	}
	mgr.TryUpgradeDescription(id, enh)

	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
	got, _ := mgr.Get(id)
	if got.Description != "manual label" {
		t.Errorf("Description = %q, want %q (manual rename must win)", got.Description, "manual label")
	}
	if !got.DescriptionLocked {
		t.Error("DescriptionLocked = false, want true")
	}
}

// TestManager_TryUpgradeDescription_ConcurrentUpgradeDuringIO_RejectsLate
// covers two hook events racing: the one that finishes second carries a layer
// that is no longer strictly greater, so Guard 2 must reject it on re-check.
func TestManager_TryUpgradeDescription_ConcurrentUpgradeDuringIO_RejectsLate(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-raced"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	id := sess.ID

	enh := &stubEnhancer{
		response: "late candidate",
		ok:       true,
		layer:    DescriptionLayerTranscript,
		during: func() {
			requireUnlocked(t, mgr)
			// A competing upgrade lands first, at the same layer.
			winner := &stubEnhancer{response: "early candidate", ok: true, layer: DescriptionLayerTranscript}
			mgr.TryUpgradeDescription(id, winner)
		},
	}
	mgr.TryUpgradeDescription(id, enh)

	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
	got, _ := mgr.Get(id)
	if got.Description != "early candidate" {
		t.Errorf("Description = %q, want %q (Guard 2 must reject the late same-layer write)", got.Description, "early candidate")
	}
}

// TestManager_TryUpgradeDescription_WorkDirChangedDuringIO_Drops covers the
// baseline going stale: the baseline was derived from the snapshot's WorkDir,
// so once the session moves we drop the round rather than compare Guard 1
// against a value that no longer describes the session.
func TestManager_TryUpgradeDescription_WorkDirChangedDuringIO_Drops(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-moved"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	id := sess.ID
	original := sess.Description

	enh := &stubEnhancer{
		response: "candidate",
		ok:       true,
		layer:    DescriptionLayerTranscript,
		during: func() {
			requireUnlocked(t, mgr)
			mgr.mu.Lock()
			mgr.sessions[id].WorkDir = "/tmp/upgrade-moved-elsewhere"
			mgr.mu.Unlock()
		},
	}
	mgr.TryUpgradeDescription(id, enh)

	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
	got, _ := mgr.Get(id)
	if got.Description != original {
		t.Errorf("Description = %q, want %q (stale baseline should drop the write-back)", got.Description, original)
	}
	if got.DescriptionLayer != DescriptionLayerBaseline {
		t.Errorf("DescriptionLayer = %d, want %d", got.DescriptionLayer, DescriptionLayerBaseline)
	}
}

// TestManager_HandleHookEvent_UserPromptSubmit_UpgradesDescription verifies that
// the hook path calls the installed enhancer for the UserPromptSubmit event.
func TestManager_HandleHookEvent_UserPromptSubmit_UpgradesDescription(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	enh := &stubEnhancer{response: "hook-derived", ok: true, layer: DescriptionLayerTranscript}
	installEnhancer(t, mgr, enh)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-upgrade"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "hook-derived" {
		t.Errorf("Description = %q, want %q (UserPromptSubmit should trigger Layer C)", got.Description, "hook-derived")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

// TestManager_HandleHookEvent_Stop_UpgradesDescription verifies that the Stop
// hook path also invokes Layer C. Stop is our reliable fallback when
// UserPromptSubmit races the transcript flush and finds an empty file — see
// the comment in HandleHookEvent for the ~10ms skew observed in practice.
func TestManager_HandleHookEvent_Stop_UpgradesDescription(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	enh := &stubEnhancer{response: "stop-derived", ok: true, layer: DescriptionLayerTranscript}
	installEnhancer(t, mgr, enh)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-stop-upgrade"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Stop only fires after a UserPromptSubmit transitioned status away from
	// stopped, so mirror that ordering to keep the setup realistic.
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "stop-derived" {
		t.Errorf("Description = %q, want %q (Stop should trigger Layer C as a flush-safe fallback)", got.Description, "stop-derived")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

// TestManager_HandleHookEvent_OtherEvents_DoNotUpgrade guards against wiring
// Layer C to hook events that don't imply a completed transcript write.
// SessionStart is intentionally omitted: it is now a Layer C trigger (via the
// LayerAgentName path) since Claude Code writes ~/.claude/sessions/<PID>.json
// before that hook fires. See TestManager_HandleHookEvent_SessionStart* for the
// positive assertion.
func TestManager_HandleHookEvent_OtherEvents_DoNotUpgrade(t *testing.T) {
	events := []string{"SessionEnd", "CwdChanged", "PostToolUse"}
	for _, ev := range events {
		t.Run(ev, func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			enh := &stubEnhancer{response: "should-not-apply", ok: true}
			installEnhancer(t, mgr, enh)

			sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-" + ev})
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}
			before := sess.Description

			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, ev, "", "", "")

			got, _ := mgr.Get(sess.ID)
			if got.Description != before {
				t.Errorf("%s: Description = %q, want %q (Layer C should only fire on UserPromptSubmit/Stop)", ev, got.Description, before)
			}
			if enh.calls != 0 {
				t.Errorf("%s: enhancer.TryGenerate calls = %d, want 0", ev, enh.calls)
			}
		})
	}
}

// TestManager_HandleHookEvent_SessionStart_TriggersLayerC is the positive
// counterpart to TestManager_HandleHookEvent_OtherEvents_DoNotUpgrade:
// SessionStart is a Layer C trigger via the LayerAgentName path, matching
// Claude Code writing ~/.claude/sessions/<PID>.json before the hook fires.
func TestManager_HandleHookEvent_SessionStart_TriggersLayerC(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "cc-name-42", ok: true, layer: DescriptionLayerAgentName})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-session-start-layerc"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "cc-name-42" {
		t.Errorf("Description = %q, want %q", got.Description, "cc-name-42")
	}
	if got.DescriptionLayer != DescriptionLayerAgentName {
		t.Errorf("DescriptionLayer = %d, want %d", got.DescriptionLayer, DescriptionLayerAgentName)
	}
}

// TestManager_TryUpgradeDescription_LayerPromotion locks in the two-hop
// promotion path a real Claude Code session takes: SessionStart plants
// LayerAgentName from the session-name file, then a later UserPromptSubmit
// swaps in the higher-quality LayerTranscript candidate once the transcript
// has been flushed.
func TestManager_TryUpgradeDescription_LayerPromotion(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "cc-name", ok: true, layer: DescriptionLayerAgentName})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-layer-promotion"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "cc-name" || got.DescriptionLayer != DescriptionLayerAgentName {
		t.Fatalf("after SessionStart: Description=%q Layer=%d, want %q/%d",
			got.Description, got.DescriptionLayer, "cc-name", DescriptionLayerAgentName)
	}

	installEnhancer(t, mgr, &stubEnhancer{response: "user prompt", ok: true, layer: DescriptionLayerTranscript})

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, _ = mgr.Get(sess.ID)
	if got.Description != "user prompt" {
		t.Errorf("Description = %q, want %q", got.Description, "user prompt")
	}
	if got.DescriptionLayer != DescriptionLayerTranscript {
		t.Errorf("DescriptionLayer = %d, want %d", got.DescriptionLayer, DescriptionLayerTranscript)
	}
}

// TestManager_TryUpgradeDescription_RejectsDowngrade locks in Guard 2: once a
// higher-layer description is in place, a lower-layer candidate must not
// overwrite it, even though the enhancer offers a non-empty candidate. This
// is what keeps a stray SessionStart delivered after Stop from clobbering an
// already-good LayerTranscript description.
func TestManager_TryUpgradeDescription_RejectsDowngrade(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "prompt-desc", ok: true, layer: DescriptionLayerTranscript})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-reject-downgrade"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "prompt-desc" || got.DescriptionLayer != DescriptionLayerTranscript {
		t.Fatalf("after UserPromptSubmit: Description=%q Layer=%d, want %q/%d",
			got.Description, got.DescriptionLayer, "prompt-desc", DescriptionLayerTranscript)
	}

	installEnhancer(t, mgr, &stubEnhancer{response: "name-desc", ok: true, layer: DescriptionLayerAgentName})

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ = mgr.Get(sess.ID)
	if got.Description != "prompt-desc" {
		t.Errorf("Description = %q, want %q (lower-layer candidate must not downgrade)", got.Description, "prompt-desc")
	}
	if got.DescriptionLayer != DescriptionLayerTranscript {
		t.Errorf("DescriptionLayer = %d, want %d (unchanged)", got.DescriptionLayer, DescriptionLayerTranscript)
	}
}

// TestManager_TryUpgradeDescription_RestartGuard locks in Guard 1: a daemon
// restart loses the runtime-only DescriptionLayer (json:"-") back to zero,
// but the persisted Description may already carry a prior Layer C upgrade.
// The guard must treat that drift as "already upgraded, provenance unknown"
// and refuse to consult the enhancer at all, rather than let a fresh
// SessionStart clobber it.
func TestManager_TryUpgradeDescription_RestartGuard(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "prior-upgrade", ok: true, layer: DescriptionLayerAgentName})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-restart-guard"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "prior-upgrade" || got.DescriptionLayer != DescriptionLayerAgentName {
		t.Fatalf("precondition: Description=%q Layer=%d, want %q/%d",
			got.Description, got.DescriptionLayer, "prior-upgrade", DescriptionLayerAgentName)
	}

	// Simulate the daemon restart: reset the in-memory layer to zero while
	// leaving the persisted Description drifted from baseline, exactly what
	// a freshly loaded Manager would observe.
	mgr.mu.Lock()
	got.DescriptionLayer = DescriptionLayerBaseline
	mgr.mu.Unlock()

	enh := &stubEnhancer{response: "should-not-apply", ok: true, layer: DescriptionLayerAgentName}
	installEnhancer(t, mgr, enh)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	final, _ := mgr.Get(sess.ID)
	if final.Description != "prior-upgrade" {
		t.Errorf("Description = %q, want %q (Guard 1 should have blocked the overwrite)", final.Description, "prior-upgrade")
	}
	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0 (Guard 1 short-circuits before consulting the enhancer)", enh.calls)
	}
}

// TestManager_TryUpgradeDescription_BranchPopulated_StillFires locks in the
// F001 fix: once captureOutputTmux populates CurrentBranch / IsWorktree, the
// baseline used for the equality guard must still match the value stored at
// create time (which knew nothing about branch/worktree). If the two baselines
// diverge, Layer C is silently disabled for every real Claude session.
func TestManager_TryUpgradeDescription_BranchPopulated_StillFires(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-branch-populated"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if sess.DescriptionLocked {
		t.Fatal("precondition: auto-generated description should be unlocked")
	}

	// Simulate the poller having filled in git/worktree metadata. Prior to the
	// F001 fix these would flip the guard's baseline to a value the stored
	// Description no longer matched, aborting the upgrade.
	mgr.mu.Lock()
	sess.CurrentBranch = "main"
	sess.IsWorktree = true
	sess.TmuxWindowName = "jin-abc"
	mgr.mu.Unlock()

	enh := &stubEnhancer{response: "post-poll upgrade", ok: true, layer: DescriptionLayerTranscript}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "post-poll upgrade" {
		t.Errorf("Description = %q, want %q (baseline guard must ignore runtime branch/worktree fields)", got.Description, "post-poll upgrade")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

// TestManager_SetDescription_WhitespaceOnly_Unlocks confirms F006: a value
// consisting only of whitespace behaves like the empty-string unlock path
// rather than being persisted verbatim.
func TestManager_SetDescription_WhitespaceOnly_Unlocks(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/set-description-whitespace",
		Description: "manual label",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !sess.DescriptionLocked {
		t.Fatal("precondition: session with explicit Description should be locked")
	}

	if err := mgr.SetDescription(sess.ID, "   \t \n "); err != nil {
		t.Fatalf("SetDescription: %v", err)
	}

	got, _ := mgr.Get(sess.ID)
	baseline := GenerateBaselineDescription(sess.WorkDir, "", false, "")
	if got.Description != baseline {
		t.Errorf("Description = %q, want baseline %q (whitespace-only value should reset to baseline)", got.Description, baseline)
	}
	if got.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false (whitespace-only value should unlock)")
	}
}

// TestBuildAgentShellCmd_SnapshotIsolatesFromConcurrentWrites is the F402
// regression guard: buildAgentShellCmd must operate on a value snapshot so
// callers can invoke it after m.mu.Unlock() while HandleHookEvent (or any
// other write path) mutates the same session under lock.
//
// The test spins a writer goroutine that keeps flipping session.AgentKind /
// AgentSessionID under lock, and a reader loop that snapshots + builds
// commands with the lock released. Under -race, any read of session.*
// inside buildAgentShellCmd would fire.
func TestBuildAgentShellCmd_SnapshotIsolatesFromConcurrentWrites(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/spawn-race",
		Description: "spawn-race",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	// Writer: flip fields under lock as fast as it can. Mirrors the write
	// pattern in HandleHookEvent (AgentSessionID / AgentSessionStarted /
	// WorkDir change under m.mu.Lock).
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			mgr.mu.Lock()
			sess.AgentSessionID = "written-under-lock"
			sess.AgentSessionStarted = !sess.AgentSessionStarted
			sess.WorkDir = "/tmp/spawn-race"
			mgr.mu.Unlock()
		}
	}()

	// Reader: snapshot under lock (mirrors the retry path) then run the
	// builder outside the lock. If snapshotForSpawn / buildAgentShellCmd
	// touched the session pointer instead of the value copy, -race would
	// catch it here.
	for i := 0; i < 500; i++ {
		mgr.mu.Lock()
		snap := snapshotForSpawn(sess, sess.WorkDir, sess.WorkDir)
		mgr.mu.Unlock()

		if _, _, err := mgr.buildAgentShellCmd(snap); err != nil {
			t.Fatalf("build failed at iter %d: %v", i, err)
		}
	}
	close(stop)
	<-writerDone
}

// ---------------------------------------------------------------------------
// SendPrompt / verify-by-capture tests
// ---------------------------------------------------------------------------

// withShortSendVerify shortens the send tuning knobs for the duration of the
// test so timeout / retry cases finish in milliseconds. Restore is registered on
// t.Cleanup.
//
// timeout lands on sendVerifyTimeoutBase and the two per-unit coefficients are
// zeroed, so the effective budget is exactly what the caller asked for whatever
// the prompt implies. sendChunkDelay is zeroed too: at its production 20ms a
// multi-chunk test would really sleep once per chunk.
//
// Not safe under t.Parallel(): rewrites package-level vars.
func withShortSendVerify(t *testing.T, timeout, settle, backoff time.Duration) {
	t.Helper()
	setForTest(t, &sendVerifyTimeoutBase, timeout)
	setForTest(t, &sendVerifySettleDelay, settle)
	setForTest(t, &sendVerifyBackoff, backoff)
	setForTest(t, &sendVerifyPerChunk, 0)
	setForTest(t, &sendVerifyPerClearKey, 0)
	setForTest(t, &sendChunkDelay, 0)
	// One look per attempt reproduces the pre-look structure, so the tests
	// that reason about attempts (retry counts, clear repeats per attempt)
	// keep a one-to-one mapping between an attempt and a capture. Base 1 with
	// the max clamped to 1 defeats the prompt-length scaling too, which would
	// otherwise give a long prompt extra looks and break those counts.
	// TestSendPrompt_LooksBeforeResending raises this to cover the look loop.
	setForTest(t, &sendVerifyLooksBase, 1)
	setForTest(t, &sendVerifyLooksMax, 1)
	setForTest(t, &sendVerifyLookDelay, 0)
}

// setForTest assigns v to *p for the duration of the test and restores the
// previous value on cleanup. The send tuning knobs are package-level vars, so
// this is not safe under t.Parallel() — see withShortSendVerify.
func setForTest[T any](t *testing.T, p *T, v T) {
	t.Helper()
	orig := *p
	*p = v
	t.Cleanup(func() { *p = orig })
}

// withVerifyLooks raises the per-attempt look count for the one test that
// exercises the look loop. Same package-var-rewrite caveat as
// withShortSendVerify; call it after that helper, which resets the value.
func withVerifyLooks(t *testing.T, looks int, delay time.Duration) {
	t.Helper()
	setForTest(t, &sendVerifyLooksBase, looks)
	setForTest(t, &sendVerifyLooksMax, looks)
	setForTest(t, &sendVerifyLookDelay, delay)
}

// withSmallChunks shrinks sendChunkMaxBytes so a chunking test can drive
// the multi-chunk path with a short, readable prompt instead of an 800-byte
// blob. Same package-var-rewrite caveat as withShortSendVerify.
func withSmallChunks(t *testing.T, max int) {
	t.Helper()
	setForTest(t, &sendChunkMaxBytes, max)
}

// withBudgetCoefficients restores non-zero per-unit terms after
// withShortSendVerify has zeroed them, for the one test that needs the
// budget to actually vary with the prompt. Keep the values small: they are
// multiplied by chunk and keypress counts and land on a real deadline.
func withBudgetCoefficients(t *testing.T, perChunk, perClearKey time.Duration) {
	t.Helper()
	setForTest(t, &sendVerifyPerChunk, perChunk)
	setForTest(t, &sendVerifyPerClearKey, perClearKey)
}

// withChunkDelay restores a non-zero inter-chunk delay for the one test that
// asserts on it — withShortSendVerify zeroes it for everyone else. Carries
// its own Cleanup rather than leaning on that helper's, so the value cannot
// leak into later tests if the call order changes.
func withChunkDelay(t *testing.T, d time.Duration) {
	t.Helper()
	setForTest(t, &sendChunkDelay, d)
}

// wantClearPresses is the number of clear keypresses SendPrompt issues for
// a prompt of the given length and clear sequence — repeats x keys. Tests
// assert against this rather than a hardcoded number so a change to
// sendClearWidthAssumed does not require editing every expectation.
func wantClearPresses(promptLen, keysPerRepeat int) int {
	return sendClearRepeats(promptLen) * keysPerRepeat
}

// withShortSendClear shortens sendClearSettleDelay so clear-key tests
// don't pay the 20ms per-attempt production delay repeatedly. Same
// package-var-rewrite caveat as withShortSendVerify — not t.Parallel-safe.
func withShortSendClear(t *testing.T, settle time.Duration) {
	t.Helper()
	orig := sendClearSettleDelay
	sendClearSettleDelay = settle
	t.Cleanup(func() { sendClearSettleDelay = orig })
}

// newIdleSessionWithPane creates a session, marks it idle and pins the given
// tmux pane ID onto it — the pre-conditions SendPrompt requires.
func newIdleSessionWithPane(t *testing.T, mgr *Manager, workDir, description, paneID string) *Session {
	t.Helper()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: description})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.Status = StatusIdle
	sess.TmuxPaneID = paneID
	mgr.mu.Unlock()
	return sess
}

// countCalls reports how often the mock recorded a method whose first arg
// matches target. SendPrompt tests use it to assert retry counts.
// countCallsWithArgs is countCalls narrowed to one further argument — the
// key for SendKeys, the buffer name for PasteBuffer. Tests asserting that a
// specific key was (or was not) sent use this rather than re-walking the log.
func countCallsWithArgs(m *mockTmuxRunner, method, target, arg string) int {
	n := 0
	for _, c := range m.calls {
		if c.method == method && len(c.args) > 1 && c.args[0] == target && c.args[1] == arg {
			n++
		}
	}
	return n
}

func countCalls(m *mockTmuxRunner, method, target string) int {
	n := 0
	for _, c := range m.calls {
		if c.method == method && len(c.args) > 0 && c.args[0] == target {
			n++
		}
	}
	return n
}

func TestSendPrompt_HitOnFirstAttempt(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)

	const pane = "%1"
	const prompt = "hello world"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-hit-first", "sp1", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",              // before: empty prompt line
		"$ hello world\n", // after: TUI echoed the prompt
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 1 {
		t.Errorf("SendKeysLiteral called %d times, want 1", got)
	}
	// Adapter opted out of clearing, so SendKeys is exactly the nudge plus
	// the commit: assert each by name rather than by total, so adding a key
	// elsewhere cannot be papered over by a count that still adds up.
	if got := mock.countCallsWithArgs("SendKeys", pane, sendNudgeKey); got != 1 {
		t.Errorf("nudge %q sent %d times, want 1", sendNudgeKey, got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("Enter sent %d times, want 1", got)
	}
	if got := countCalls(mock, "SendKeys", pane); got != 2 {
		t.Errorf("SendKeys called %d times, want 2 (nudge + Enter)", got)
	}
	if got := countCalls(mock, "CapturePane", pane); got != 2 {
		t.Errorf("CapturePane called %d times, want 2 (before+after)", got)
	}
}

func TestSendPrompt_HitOnRetry(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)

	const pane = "%2"
	const prompt = "please build the report"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-retry", "sp2", pane)

	// Sequence: before1 → after1 (miss) → before2 → after2 (hit).
	mock.capturedSequence[pane] = []string{
		"welcome\n$ ",                          // before1
		"welcome\n$ ",                          // after1 — TUI dropped the keys
		"welcome\n$ ",                          // before2
		"welcome\n$ please build the report\n", // after2 — landed
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 2 {
		t.Errorf("SendKeysLiteral called %d times, want 2 (initial + 1 retry)", got)
	}
	// The nudge fires once per attempt; Enter only after verify passes.
	if got := mock.countCallsWithArgs("SendKeys", pane, sendNudgeKey); got != 2 {
		t.Errorf("nudge %q sent %d times, want 2 (one per attempt)", sendNudgeKey, got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("Enter sent %d times, want 1 (after verify)", got)
	}
}

func TestSendPrompt_TimeoutMiss(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 30*time.Millisecond, time.Millisecond, time.Millisecond)

	const pane = "%3"
	const prompt = "unreachable"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-timeout", "sp3", pane)

	// Every capture returns the same idle screen — the prompt never lands.
	mock.captured[pane] = "$ "

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want timeout error")
	}
	if !strings.Contains(err.Error(), "send verify") {
		t.Errorf("error %q missing 'send verify' prefix", err.Error())
	}
	// The nudge is part of every attempt, so a bare SendKeys count is no
	// longer the right assertion — what must stay at zero is the commit.
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times on failure, want 0 (no commit until verify passes)", got)
	}
	if countCalls(mock, "SendKeysLiteral", pane) < 1 {
		t.Errorf("SendKeysLiteral was not called at all")
	}
}

func TestSendPrompt_SendKeysLiteralError(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)

	const pane = "%4"
	const prompt = "boom"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-lit-err", "sp4", pane)

	mock.captured[pane] = "$ "
	mock.sendKeysLiteralErr[pane] = errors.New("tmux disconnected")

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "failed to send prompt") {
		t.Errorf("error %q missing 'failed to send prompt'", err.Error())
	}
	if got := countCalls(mock, "SendKeys", pane); got != 0 {
		t.Errorf("SendKeys called %d times after send failure, want 0", got)
	}
}

func TestSendPrompt_CapturePaneErrorBefore(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)

	const pane = "%5"
	const prompt = "hello"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-cap-err-before", "sp5", pane)

	mock.captureErr[pane] = errors.New("pane died")

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "capture-pane before failed") {
		t.Errorf("error %q missing 'capture-pane before failed'", err.Error())
	}
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 0 {
		t.Errorf("SendKeysLiteral called %d times, want 0 (capture failed before send)", got)
	}
	if got := countCalls(mock, "SendKeys", pane); got != 0 {
		t.Errorf("SendKeys called %d times, want 0 (Enter must not fire on capture failure)", got)
	}
}

func TestSendPrompt_CapturePaneErrorAfter(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)

	const pane = "%5b"
	const prompt = "hello"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-cap-err-after", "sp5b", pane)

	// "before" capture succeeds, then "after" capture fails on the same
	// SendPrompt attempt. Exercises the second capture-pane error branch
	// so a regression that swaps the two wrapped strings is caught.
	mock.capturedSequence[pane] = []string{"$ "}
	mock.captureErrAfter[pane] = errors.New("pane died mid-send")

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "capture-pane after failed") {
		t.Errorf("error %q missing 'capture-pane after failed'", err.Error())
	}
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 1 {
		t.Errorf("SendKeysLiteral called %d times, want 1 (send fires before the after-capture)", got)
	}
	// The nudge precedes the after-capture, so it has already fired here.
	// Enter is the thing that must not.
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times, want 0 (must not fire when after-capture fails)", got)
	}
}

// TestSendPrompt_Preconditions covers the guard branches that return
// before any tmux call. Both share the same shape: create a session,
// force fields under lock, call SendPrompt, assert the error and that
// no tmux verbs were invoked.
func TestSendPrompt_Preconditions(t *testing.T) {
	cases := []struct {
		name    string
		workDir string
		status  Status
		paneID  string
		wantErr string
	}{
		{"not-idle", "/tmp/send-notidle", StatusThinking, "%6", "not idle"},
		{"no-pane", "/tmp/send-nopane", StatusIdle, "", "no tmux pane"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, mock, _ := newTestManager(t)
			sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: tc.workDir, Description: tc.name})
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}
			mgr.mu.Lock()
			sess.Status = tc.status
			sess.TmuxPaneID = tc.paneID
			mgr.mu.Unlock()

			err = mgr.SendPrompt(sess.ID, "irrelevant")
			if err == nil {
				t.Fatalf("SendPrompt returned nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q missing %q", err.Error(), tc.wantErr)
			}
			for _, c := range mock.calls {
				if c.method == "SendKeys" || c.method == "SendKeysLiteral" || c.method == "CapturePane" {
					t.Errorf("unexpected tmux call %s(%v) on failing precondition", c.method, c.args)
				}
			}
		})
	}
}

// TestSendPrompt_ResidualClearedByAdapter asserts that when the resolved
// adapter's ClearInputKeys returns a non-empty sequence, SendPrompt sends
// those keys BEFORE each attempt's SendKeysLiteral so residual input cannot
// concatenate with the new prompt. Verified at the call-log level: the
// first C-u must precede the first SendKeysLiteral, and Enter must fire
// only after verify passes.
func TestSendPrompt_ResidualClearedByAdapter(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendClear(t, time.Millisecond)
	installClearKeys(t, mgr, []string{"C-u"})

	const pane = "%clr1"
	const prompt = "SENTINEL9 say ok"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-clear-basic", "clr1", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",                 // before — post-clear, empty
		"$ SENTINEL9 say ok", // after  — prompt landed
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}

	// One press only clears one visual row, so the sequence repeats
	// sendClearRepeats(len(prompt)) times per attempt. We only did one
	// attempt.
	wantClear := wantClearPresses(len(prompt), 1)
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-u"); got != wantClear {
		t.Errorf("SendKeys(%q, %q) count = %d, want %d", pane, "C-u", got, wantClear)
	}
	// Enter fires exactly once at the end.
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("SendKeys(%q, %q) count = %d, want 1", pane, "Enter", got)
	}
	// Ordering: C-u before the prompt literal, prompt literal before Enter.
	clearIdx := mock.firstCallIndex("SendKeys", pane, "C-u")
	litIdx := mock.firstCallIndex("SendKeysLiteral", pane, prompt)
	enterIdx := mock.firstCallIndex("SendKeys", pane, "Enter")
	if clearIdx < 0 || litIdx < 0 || enterIdx < 0 {
		t.Fatalf("missing calls: clearIdx=%d litIdx=%d enterIdx=%d", clearIdx, litIdx, enterIdx)
	}
	if !(clearIdx < litIdx && litIdx < enterIdx) {
		t.Errorf("call order violated: C-u@%d < prompt@%d < Enter@%d expected", clearIdx, litIdx, enterIdx)
	}
	// Baseline capture happens AFTER the clear on every attempt, so the
	// clear must precede the first CapturePane too.
	firstCapIdx := mock.firstCallIndex("CapturePane", pane)
	if firstCapIdx < 0 || clearIdx >= firstCapIdx {
		t.Errorf("clear (idx=%d) must precede first CapturePane (idx=%d)", clearIdx, firstCapIdx)
	}
}

// TestSendPrompt_ResidualClearedOnRetry asserts that the clear key sequence
// fires again on each retry, not just the first attempt — so a TUI that
// eats a strict prefix from attempt 1 sees a fresh empty input line at
// attempt 2 as well.
func TestSendPrompt_ResidualClearedOnRetry(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendClear(t, time.Millisecond)
	installClearKeys(t, mgr, []string{"C-u"})

	const pane = "%clr2"
	const prompt = "please build the report"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-clear-retry", "clr2", pane)

	// Attempt 1: miss. Attempt 2: hit.
	mock.capturedSequence[pane] = []string{
		"$ ",                        // before1 (post-clear)
		"$ ",                        // after1  — dropped keys, verify miss
		"$ ",                        // before2 (post-clear again)
		"$ please build the report", // after2  — landed
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	// Two attempts → the clear sequence repeats on each → two
	// SendKeysLiteral, one Enter.
	wantClear := 2 * wantClearPresses(len(prompt), 1)
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-u"); got != wantClear {
		t.Errorf("SendKeys(%q, %q) count = %d, want %d (initial + 1 retry)", pane, "C-u", got, wantClear)
	}
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 2 {
		t.Errorf("SendKeysLiteral called %d times, want 2 (initial + 1 retry)", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("SendKeys(%q, %q) count = %d, want 1", pane, "Enter", got)
	}
}

// TestSendPrompt_MultipleClearKeysAllSentInOrder asserts that when
// ClearInputKeys returns more than one key (the fallback shape reserved
// for a TUI where C-u is bound to something else), SendPrompt sends every
// key and preserves the declared order. Guards against a refactor that
// accidentally collapses the ranging loop into "send first key only".
func TestSendPrompt_MultipleClearKeysAllSentInOrder(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendClear(t, time.Millisecond)
	installClearKeys(t, mgr, []string{"C-a", "C-k"})

	const pane = "%clr-multi"
	const prompt = "hello multi"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-clear-multi", "clr-multi", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",
		"$ hello multi\n",
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}

	// Both keys must fire, once per repeat of the sequence (one attempt
	// here). The repeat count is per sequence, not per key, so each key
	// lands the same number of times.
	wantEach := sendClearRepeats(len(prompt))
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-a"); got != wantEach {
		t.Errorf("SendKeys(%q, %q) count = %d, want %d", pane, "C-a", got, wantEach)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-k"); got != wantEach {
		t.Errorf("SendKeys(%q, %q) count = %d, want %d", pane, "C-k", got, wantEach)
	}
	// Order: C-a before C-k (as declared in the ClearInputKeys slice).
	caIdx := mock.firstCallIndex("SendKeys", pane, "C-a")
	ckIdx := mock.firstCallIndex("SendKeys", pane, "C-k")
	litIdx := mock.firstCallIndex("SendKeysLiteral", pane, prompt)
	if caIdx < 0 || ckIdx < 0 || litIdx < 0 {
		t.Fatalf("missing calls: C-a@%d C-k@%d prompt@%d", caIdx, ckIdx, litIdx)
	}
	if !(caIdx < ckIdx && ckIdx < litIdx) {
		t.Errorf("call order violated: C-a@%d < C-k@%d < prompt@%d expected", caIdx, ckIdx, litIdx)
	}
}

// TestSendPrompt_ClearKeysNilFallsThrough asserts that when the resolved
// adapter's ClearInputKeys returns nil (opt-out), SendPrompt keeps its
// pre-refactor behaviour (no C-u). This is the fall-through contract every
// existing SendPrompt test also relies on — the baseline fakeAgent's
// clearKeys defaults to nil — so this test pins it explicitly.
func TestSendPrompt_ClearKeysNilFallsThrough(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	// Deliberately do NOT call installClearKeys.

	const pane = "%noclr"
	const prompt = "hello world"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-noclr", "noclr", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",
		"$ hello world\n",
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-u"); got != 0 {
		t.Errorf("SendKeys(%q, %q) count = %d, want 0 (adapter opted out via nil ClearInputKeys)", pane, "C-u", got)
	}
	// Opting out of the clear does NOT opt out of the nudge: it is a
	// measured no-op and Codex/OpenCode are unverifiable without it.
	if got := mock.countCallsWithArgs("SendKeys", pane, sendNudgeKey); got != 1 {
		t.Errorf("nudge %q sent %d times, want 1 even when clearing is opted out", sendNudgeKey, got)
	}
	if got := countCalls(mock, "SendKeys", pane); got != 2 {
		t.Errorf("SendKeys count = %d, want 2 (nudge + Enter)", got)
	}
}

// TestSendPrompt_ClearKeysEmptyNoOp asserts that an adapter whose
// ClearInputKeys returns a non-nil empty slice (`[]string{}`) is treated
// identically to nil: no C-u, no settle delay. This differentiates from
// TestSendPrompt_ClearKeysNilFallsThrough (which drives the default nil path)
// and guards against a regression where `len == 0` on a non-nil slice
// would still trigger the settle-delay branch or an errant SendKeys.
func TestSendPrompt_ClearKeysEmptyNoOp(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	installClearKeys(t, mgr, []string{}) // non-nil, empty — must still opt out

	const pane = "%noop"
	const prompt = "hello world"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-noop", "noop", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",
		"$ hello world\n",
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	if got := countCalls(mock, "SendKeys", pane); got != 2 {
		t.Errorf("SendKeys count = %d, want 2 (nudge + Enter — empty clear keys are opt-out)", got)
	}
}

// TestSendPrompt_ClearKeyError asserts fail-fast semantics: when the
// clear-key SendKeys errors, SendPrompt returns immediately without
// running SendKeysLiteral or the final Enter. The pane is in an unusable
// state and retrying downstream is meaningless.
func TestSendPrompt_ClearKeyError(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendClear(t, time.Millisecond)
	installClearKeys(t, mgr, []string{"C-u"})

	const pane = "%clr-err"
	const prompt = "hello"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-clr-err", "clr-err", pane)

	mock.sendKeysErr["C-u"] = errors.New("tmux disconnected")

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "failed to send clear key") {
		t.Errorf("error %q missing 'failed to send clear key'", err.Error())
	}
	// Fail-fast means the FIRST press returns, not that the loop grinds
	// through all sendClearMaxKeys repeats collecting the same error. At the
	// measured per-invocation cost that difference is seconds of spinning
	// against a pane that is already unusable.
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-u"); got != 1 {
		t.Errorf("C-u attempted %d times, want 1 (fail-fast, not up to %d repeats)", got, sendClearMaxKeys)
	}
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 0 {
		t.Errorf("SendKeysLiteral called %d times after clear failure, want 0", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter fired %d times after clear failure, want 0", got)
	}
	if got := countCalls(mock, "CapturePane", pane); got != 0 {
		t.Errorf("CapturePane called %d times after clear failure, want 0 (must not reach baseline)", got)
	}
}

// TestSendPrompt_ChunksLargePrompt asserts that a prompt too big for one
// write goes out as several literals whose concatenation is byte-identical
// to the original, in order, all before the nudge and the commit. A silent
// reordering or a dropped chunk here would commit a mangled prompt.
func TestSendPrompt_ChunksLargePrompt(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withSmallChunks(t, 10)

	const pane = "%chunk"
	prompt := "0123456789abcdefghijKLMNOPQRSTuvwxyz"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-chunk", "chunk", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",
		"$ " + prompt,
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}

	wantChunks := len(chunkPrompt(prompt, 10))
	if wantChunks < 2 {
		t.Fatalf("test setup broken: prompt yields %d chunk(s), need >1", wantChunks)
	}
	var sent []string
	for _, c := range mock.calls {
		if c.method == "SendKeysLiteral" && c.args[0] == pane {
			sent = append(sent, c.args[1])
		}
	}
	if len(sent) != wantChunks {
		t.Errorf("SendKeysLiteral called %d times, want %d", len(sent), wantChunks)
	}
	if joined := strings.Join(sent, ""); joined != prompt {
		t.Errorf("chunks rejoin to %q, want %q", joined, prompt)
	}
	for i, c := range sent {
		if len(c) > 10 {
			t.Errorf("chunk %d is %d bytes, want at most 10", i, len(c))
		}
	}
	// Nudge after every chunk, Enter after the nudge.
	lastLit := -1
	for i, c := range mock.calls {
		if c.method == "SendKeysLiteral" && c.args[0] == pane {
			lastLit = i
		}
	}
	nudgeIdx := mock.firstCallIndex("SendKeys", pane, sendNudgeKey)
	enterIdx := mock.firstCallIndex("SendKeys", pane, "Enter")
	if !(lastLit < nudgeIdx && nudgeIdx < enterIdx) {
		t.Errorf("order violated: last chunk@%d < nudge@%d < Enter@%d expected", lastLit, nudgeIdx, enterIdx)
	}
}

// TestSendPrompt_LooksBeforeResending pins the rule that makes slow TUIs
// verifiable at all: when the first look misses, the attempt nudges and
// looks again instead of clearing and pushing the whole prompt a second
// time. Re-sending discards whatever the TUI had rendered, so an agent that
// needs longer than one settle delay is reset before it can finish and
// never converges — measured on Codex, where a 16KB prompt burned the whole
// budget across 16 re-sends but verified in 1.7s once the attempt looked.
func TestSendPrompt_LooksBeforeResending(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withVerifyLooks(t, 4, time.Millisecond)
	withShortSendClear(t, time.Millisecond)
	installClearKeys(t, mgr, []string{"C-u"})

	const pane = "%looks"
	const prompt = "slow tui prompt"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-looks", "looks", pane)

	// before, then two misses, then the prompt finally renders.
	mock.capturedSequence[pane] = []string{
		"$ ",
		"$ ",
		"$ ",
		"$ slow tui prompt",
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	// The whole point: one send, several looks.
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 1 {
		t.Errorf("SendKeysLiteral called %d times, want 1 (the prompt must not be re-sent while looking)", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, sendNudgeKey); got != 3 {
		t.Errorf("nudge sent %d times, want 3 (one per look until it landed)", got)
	}
	// A second attempt would clear again; there must have been only one.
	wantClear := wantClearPresses(len(prompt), 1)
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-u"); got != wantClear {
		t.Errorf("C-u sent %d times, want %d (a single attempt's worth)", got, wantClear)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("Enter sent %d times, want 1", got)
	}
}

// TestSendPrompt_SendKeysLiteralErrorMidChunk covers the partial-delivery
// failure: the first chunk lands, the second errors. The prompt is now
// half-written into the input area, so the one thing that must not happen
// is a commit — Enter would submit a truncated prompt to the agent.
func TestSendPrompt_SendKeysLiteralErrorMidChunk(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withSmallChunks(t, 10)

	const pane = "%chunk-err"
	prompt := "0123456789abcdefghijKLMNOPQRST"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-chunk-err", "chunk-err", pane)

	mock.captured[pane] = "$ "
	mock.sendKeysLiteralErr[pane] = errors.New("tmux disconnected")
	mock.sendKeysLiteralErrAfterN[pane] = 2 // first chunk lands, second fails

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "failed to send prompt") {
		t.Errorf("error %q missing 'failed to send prompt'", err.Error())
	}
	if got := countCalls(mock, "SendKeysLiteral", pane); got != 2 {
		t.Errorf("SendKeysLiteral called %d times, want 2 (stopped at the failing chunk)", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times, want 0 (a half-written prompt must never commit)", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, sendNudgeKey); got != 0 {
		t.Errorf("nudge sent %d times, want 0 (send aborted before the nudge)", got)
	}
}

// TestSendPrompt_ClearRepeatsCapped pins the best-effort ceiling: past
// roughly 30KB the clear count stops growing rather than spinning the pane
// for minutes. The send still proceeds — the residue risk at that size is
// documented in docs/gotchas.md rather than turned into an error.
func TestSendPrompt_ClearRepeatsCapped(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendClear(t, time.Millisecond)
	installClearKeys(t, mgr, []string{"C-u"})

	const pane = "%clr-cap"
	prompt := strings.Repeat("x", sendClearWidthAssumed*sendClearMaxKeys*2)
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-clr-cap", "clr-cap", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",
		"$ " + prompt,
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil (cap is best-effort, not an error)", err)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "C-u"); got != sendClearMaxKeys {
		t.Errorf("C-u sent %d times, want %d (capped)", got, sendClearMaxKeys)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("Enter sent %d times, want 1", got)
	}
}

// TestSendPrompt_BudgetWiredFromPromptShape checks that SendPrompt actually
// feeds the send's shape into sendVerifyBudget, rather than merely that the
// function computes the right number — the arithmetic is already pinned by
// TestSendVerifyBudget, and passing it (0, 0) would restore the flat timeout
// this scaling exists to replace while leaving that test green.
//
// The timeout error reports the budget it enforced, so the assertion reads it
// back out of the message instead of timing the call.
func TestSendPrompt_BudgetWiredFromPromptShape(t *testing.T) {
	cases := []struct {
		name      string
		prompt    string
		clearKeys []string
	}{
		{"short", "hi", []string{"C-u"}},
		{"multi-chunk", strings.Repeat("x", 90), []string{"C-u"}},
		// Two keys per repeat: pins that the budget counts actual presses
		// (repeats x keys), not repeats alone.
		{"two-clear-keys", strings.Repeat("x", 90), []string{"C-a", "C-k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, mock, _ := newTestManager(t)
			withShortSendVerify(t, time.Millisecond, 0, time.Millisecond)
			withShortSendClear(t, 0)
			withSmallChunks(t, 10)
			withBudgetCoefficients(t, 2*time.Millisecond, time.Millisecond)
			installClearKeys(t, mgr, tc.clearKeys)

			const pane = "%budget"
			sess := newIdleSessionWithPane(t, mgr, "/tmp/send-budget-"+tc.name, "budget-"+tc.name, pane)
			mock.captured[pane] = "$ " // never shows the prompt: always a miss

			want := sendVerifyBudget(
				len(chunkPrompt(tc.prompt, sendChunkMaxBytes)),
				wantClearPresses(len(tc.prompt), len(tc.clearKeys)),
				sendVerifyLookCount(len(tc.prompt)),
			)
			if want == sendVerifyTimeoutBase {
				t.Fatalf("test setup broken: budget %v equals the base, so this "+
					"case cannot distinguish a wired budget from a flat one", want)
			}

			err := mgr.SendPrompt(sess.ID, tc.prompt)
			if err == nil {
				t.Fatalf("SendPrompt returned nil, want a verify timeout")
			}

			// Anchor on the surrounding literals: a bare Contains on the
			// duration alone matches a longer one that ends with it
			// ("7ms" inside "17ms"), which lets a skewed budget through.
			needle := "within " + want.String() + " (attempts="
			if !strings.Contains(err.Error(), needle) {
				t.Errorf("error %q does not report the expected budget %v; "+
					"SendPrompt is not deriving it from the prompt's shape", err.Error(), want)
			}
		})
	}
}

// TestSendPrompt_DelaysBetweenChunks pins D2. Without a gap between writes
// Codex coalesces adjacent chunks into one read and folds them into a paste
// placeholder, which defeats the split entirely — and since Codex ignores an
// unknown `-c` key silently, the spawn-side paste-burst override cannot be
// relied on to cover it. Every other send test zeroes sendChunkDelay, so
// deleting the sleep would otherwise go unnoticed.
func TestSendPrompt_DelaysBetweenChunks(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, 0, time.Millisecond)
	withSmallChunks(t, 10)
	const delay = 5 * time.Millisecond
	withChunkDelay(t, delay)

	const pane = "%chunk-delay"
	prompt := strings.Repeat("y", 25) // 3 chunks at max=10
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-chunk-delay", "chunk-delay", pane)
	mock.capturedSequence[pane] = []string{"$ ", "$ " + prompt}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}

	gaps := mock.sendKeysLiteralGaps(pane)
	if len(gaps) != 2 {
		t.Fatalf("recorded %d inter-chunk gaps, want 2 (3 chunks)", len(gaps))
	}
	for i, gap := range gaps {
		if gap < delay {
			t.Errorf("gap between chunk %d and %d was %v, want at least %v", i, i+1, gap, delay)
		}
	}
}

// ---------------------------------------------------------------------------
// Overlay dismissal before Enter
//
// Verify proves the prompt is rendered in the input area, not that Enter will
// submit it: an agent holding a completion overlay open consumes Enter to accept
// a candidate instead. These tests pin the step that closes the overlay, and the
// re-check that keeps that step from becoming a new way to commit the wrong thing.
// ---------------------------------------------------------------------------

// withShortSendDismiss shortens the post-dismiss settle so these tests don't
// pay the production delay. Same package-var-rewrite caveat as
// withShortSendVerify — not t.Parallel-safe.
func withShortSendDismiss(t *testing.T, settle time.Duration) {
	t.Helper()
	setForTest(t, &sendDismissSettleDelay, settle)
}

func TestSendPrompt_DismissesOverlayBeforeEnter(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm1"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss", "dm1", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",                     // before — empty
		"$ list @internal/agent", // after the literal — prompt landed
		"$ list @internal/agent", // after Escape — prompt survived
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}

	if got := mock.countCallsWithArgs("SendKeys", pane, "Escape"); got != 1 {
		t.Errorf("Escape sent %d times, want 1", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("Enter sent %d times, want 1", got)
	}
	// Ordering is the whole point: an Escape after Enter would dismiss an
	// overlay that had already eaten the commit.
	litIdx := mock.firstCallIndex("SendKeysLiteral", pane, prompt)
	escIdx := mock.firstCallIndex("SendKeys", pane, "Escape")
	enterIdx := mock.firstCallIndex("SendKeys", pane, "Enter")
	if litIdx < 0 || escIdx < 0 || enterIdx < 0 {
		t.Fatalf("missing calls: literal=%d escape=%d enter=%d", litIdx, escIdx, enterIdx)
	}
	if !(litIdx < escIdx && escIdx < enterIdx) {
		t.Errorf("call order violated: literal@%d < Escape@%d < Enter@%d expected", litIdx, escIdx, enterIdx)
	}
	// before + verify + post-dismiss re-check.
	if got := countCalls(mock, "CapturePane", pane); got != 3 {
		t.Errorf("CapturePane called %d times, want 3 (before + verify + post-dismiss)", got)
	}
	// And that third capture has to come AFTER the keys. Reading the pane
	// first would show the pre-dismiss state, so the re-check would pass
	// whatever the keys did to the input — the count above cannot see that,
	// because the mock advances its sequence by call count, not by time.
	recapIdx := mock.lastCallIndex("CapturePane", pane)
	if !(escIdx < recapIdx && recapIdx < enterIdx) {
		t.Errorf("re-check out of order: Escape@%d < recapture@%d < Enter@%d expected",
			escIdx, recapIdx, enterIdx)
	}
}

// TestSendPrompt_DismissRecheckComparesAgainstBaseline pins the rule the
// re-check shares with the verify loop: the prompt counts as present only if it
// appears MORE often than it did at the baseline, not merely somewhere on screen.
//
// The distinction has teeth whenever the pane already carries the prompt — a
// re-send, or a transcript still showing the previous turn. A presence test
// would be satisfied by that old copy, so a dismiss key that emptied the input
// would sail through and Enter would commit nothing.
func TestSendPrompt_DismissRecheckComparesAgainstBaseline(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm8"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-baseline", "dm8", pane)

	mock.capturedSequence[pane] = []string{
		// Baseline already holds the needle once — the previous turn.
		"prev: list @internal/agent",
		// Verify: now twice, so 2 > 1 and the attempt lands.
		"prev: list @internal/agent\n$ list @internal/agent",
		// After the dismiss key the input is empty again: back to once.
		// 1 > 1 is false, so this must abort — while "is it on screen at
		// all?" would say yes and press Enter on an empty input.
		"prev: list @internal/agent\n$ ",
	}

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil; want abort (the dismiss key emptied the input)")
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times, want 0", got)
	}
}

// TestSendPrompt_VerifyComparesAgainstBaseline is the same rule one step
// earlier, in the look loop. Both call sites read it from landedIn, so both
// need a case where presence and delta disagree — otherwise the shared helper
// could be reduced to a presence test and every test would stay green.
func TestSendPrompt_VerifyComparesAgainstBaseline(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 300*time.Millisecond, time.Millisecond, time.Millisecond)

	const pane = "%dm9"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-verify-baseline", "dm9", pane)

	// The needle is on screen the whole time and never gains an occurrence:
	// the prompt never reached the input. Verify must time out rather than
	// vouch for the copy that was already there.
	mock.capturedSequence[pane] = []string{"prev: list @internal/agent"}

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil; want a verify timeout (the needle never gained an occurrence)")
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times, want 0", got)
	}
}

// TestSendPrompt_DismissKeysAskedForThisPrompt pins the wiring between
// SendPrompt and the adapter. Production claude decides per prompt, so a
// manager that passed "" — or any other stand-in — would put every session
// back on the pre-fix path while the adapter's own table tests stayed green.
func TestSendPrompt_DismissKeysAskedForThisPrompt(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)

	const pane = "%dm10"
	const prompt = "list @internal/agent"
	var seen []string
	installDismissFn(t, mgr, func(p string) []string {
		seen = append(seen, p)
		if p == prompt {
			return []string{"Escape"}
		}
		return nil
	})

	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-wiring", "dm10", pane)
	mock.capturedSequence[pane] = []string{"$ ", "$ list @internal/agent", "$ list @internal/agent"}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	if len(seen) != 1 || seen[0] != prompt {
		t.Errorf("adapter was asked with %q, want exactly [%q]", seen, prompt)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Escape"); got != 1 {
		t.Errorf("Escape sent %d times, want 1 (the adapter said yes for this prompt)", got)
	}
}

// TestSendPrompt_DismissEmptySliceOptsOut covers the other way an adapter can
// decline. The interface doc says nil opts out; an empty slice has to mean the
// same thing, matching ClearInputKeys, or `!= nil` and `len() > 0` drift apart
// and one of them silently starts sending keys nobody asked for.
func TestSendPrompt_DismissEmptySliceOptsOut(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	installDismissKeys(t, mgr, []string{})

	const pane = "%dm11"
	const prompt = "say pong only"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-empty", "dm11", pane)
	mock.capturedSequence[pane] = []string{"$ ", "$ say pong only"}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	if got := countCalls(mock, "SendKeys", pane); got != 2 {
		t.Errorf("SendKeys called %d times, want 2 (nudge + Enter)", got)
	}
	if got := countCalls(mock, "CapturePane", pane); got != 2 {
		t.Errorf("CapturePane called %d times, want 2 (no re-check on the opt-out path)", got)
	}
}

// TestSendPrompt_DismissAfterRetry wires the dismissal to the retry loop.
// beforeNorm now outlives one iteration, so the re-check must read the
// baseline of the attempt that actually landed — not the first attempt's.
func TestSendPrompt_DismissAfterRetry(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm12"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-retry", "dm12", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",                         // attempt 1 baseline
		"$ ",                         // attempt 1 verify — nothing landed
		"prev: list @internal/agent", // attempt 2 baseline, needle already once
		"prev: list @internal/agent\n$ list @internal/agent", // attempt 2 lands: 2 > 1
		"prev: list @internal/agent\n$ list @internal/agent", // post-dismiss: still 2 > 1
	}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Escape"); got != 1 {
		t.Errorf("Escape sent %d times, want 1 (once, after the attempt that landed)", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("Enter sent %d times, want 1", got)
	}
}

// TestSendPrompt_RecheckUsesTheLandingAttemptsBaseline pins which baseline the
// re-check reads. beforeNorm now outlives an iteration, so a refactor that
// captured it once — on the first attempt — would leave the comparison running
// against a stale, emptier pane and quietly pass sends it should reject.
//
// The sequence is built so the two baselines disagree: attempt 1's has the
// needle zero times, attempt 2's has it once, and the pane after the dismiss
// key has it once. Against the right baseline that is 1 > 1, false, abort.
// Against attempt 1's it is 1 > 0, true, and Enter goes out.
func TestSendPrompt_RecheckUsesTheLandingAttemptsBaseline(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm13"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-stale-baseline", "dm13", pane)

	mock.capturedSequence[pane] = []string{
		"$ ",                         // attempt 1 baseline — needle absent
		"$ ",                         // attempt 1 verify — nothing landed, retry
		"prev: list @internal/agent", // attempt 2 baseline — needle once
		"prev: list @internal/agent\n$ list @internal/agent", // attempt 2 lands: 2 > 1
		"prev: list @internal/agent",                         // after the dismiss key: back to once
	}

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil; want abort (the input lost the prompt on the landing attempt)")
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times, want 0", got)
	}
}

// TestSendPrompt_WaitsBeforeReadingTheDismissResult pins the settle between
// the dismiss keys and the re-check.
//
// Without it the re-check reads a pane that has not repainted, which still
// shows the prompt — so it passes regardless of what the keys did, and the
// guard becomes decoration. Nothing else can catch that: this mock advances
// its recorded content by call count, so every ordering and content assertion
// stays satisfied. Measured on Claude Code 2.1.224, a destructive key took
// 54-161ms over five runs to show up in capture-pane.
func TestSendPrompt_WaitsBeforeReadingTheDismissResult(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	const settle = 40 * time.Millisecond
	withShortSendDismiss(t, settle)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm14"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-settle", "dm14", pane)

	// Abort on the re-check, so the last SendKeys is the dismiss key rather
	// than Enter and the gap measures the settle we care about.
	mock.capturedSequence[pane] = []string{"$ ", "$ list @internal/agent", "$ "}

	if err := mgr.SendPrompt(sess.ID, prompt); err == nil {
		t.Fatalf("SendPrompt returned nil; want abort")
	}
	if got := mock.dismissSettleGap(pane); got < settle {
		t.Errorf("waited %v between the dismiss key and the re-check, want at least %v", got, settle)
	}
}

func TestSendPrompt_NoDismissWhenAdapterOptsOut(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	// No installDismissKeys: the adapter opts out, which is the production
	// default for every adapter but claude, and for claude on every prompt
	// that cannot open an overlay.

	const pane = "%dm2"
	const prompt = "say pong only"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-optout", "dm2", pane)

	mock.capturedSequence[pane] = []string{"$ ", "$ say pong only"}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}

	if got := mock.countCallsWithArgs("SendKeys", pane, "Escape"); got != 0 {
		t.Errorf("Escape sent %d times on the opt-out path, want 0", got)
	}
	// Exactly the pre-fix key sequence: nudge + Enter, nothing else.
	if got := countCalls(mock, "SendKeys", pane); got != 2 {
		t.Errorf("SendKeys called %d times, want 2 (nudge + Enter)", got)
	}
	// And no extra capture: the re-check is paid only when keys went out.
	if got := countCalls(mock, "CapturePane", pane); got != 2 {
		t.Errorf("CapturePane called %d times, want 2 (before + verify)", got)
	}
}

func TestSendPrompt_DismissKeyErrorDoesNotPressEnter(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm3"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-err", "dm3", pane)

	mock.capturedSequence[pane] = []string{"$ ", "$ list @internal/agent"}
	mock.sendKeysErr["Escape"] = errors.New("tmux disconnected")

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "overlay-dismiss key") {
		t.Errorf("error %q missing 'overlay-dismiss key'", err.Error())
	}
	// The overlay may still be open, so committing would submit whatever the
	// TUI decides — not what we were asked to send.
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times after a dismiss failure, want 0", got)
	}
}

func TestSendPrompt_DismissThatClearsInputAbortsSend(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm4"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-ate-input", "dm4", pane)

	// The failure this guard exists for: the dismiss key turns out to clear
	// the input on some agent or version. Pressing Enter then commits an
	// empty line — or worse, whatever was left behind.
	mock.capturedSequence[pane] = []string{
		"$ ",                     // before
		"$ list @internal/agent", // verify passes
		"$ ",                     // after Escape — the prompt is gone
	}

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "left the input area") {
		t.Errorf("error %q missing 'left the input area'", err.Error())
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times after the prompt vanished, want 0", got)
	}
}

func TestSendPrompt_DismissRecaptureErrorDoesNotPressEnter(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape"})

	const pane = "%dm5"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-recap-err", "dm5", pane)

	mock.capturedSequence[pane] = []string{"$ ", "$ list @internal/agent", "$ list @internal/agent"}
	// Fail the third capture only — the post-dismiss one. captureErrAfter
	// would take out the verify capture first and never reach this branch.
	mock.captureErrAtCall[pane] = captureFailure{nth: 3, err: errors.New("pane died after dismiss")}

	err := mgr.SendPrompt(sess.ID, prompt)
	if err == nil {
		t.Fatalf("SendPrompt returned nil, want error")
	}
	if !strings.Contains(err.Error(), "after overlay dismiss") {
		t.Errorf("error %q missing 'after overlay dismiss'", err.Error())
	}
	// Unknown state is not a reason to commit.
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 0 {
		t.Errorf("Enter sent %d times when the re-check could not be made, want 0", got)
	}
}

func TestSendPrompt_UnresolvableAgentStillSends(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)

	const pane = "%dm7"
	const prompt = "list @internal/agent"
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/send-dismiss-unknown-kind",
		Description: "dm7",
		AgentKind:   "not-registered",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.Status = StatusIdle
	sess.TmuxPaneID = pane
	mgr.mu.Unlock()

	mock.capturedSequence[pane] = []string{"$ ", "$ list @internal/agent"}

	// Every capability SendPrompt reads is best-effort: a resolver that
	// cannot name the kind must degrade the send, never refuse it. The
	// dismissal is one more of those, so it must not become the first
	// capability whose absence turns into an error.
	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil (unresolvable adapter must not block a send)", err)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Escape"); got != 0 {
		t.Errorf("Escape sent %d times with no adapter to ask, want 0", got)
	}
	if got := mock.countCallsWithArgs("SendKeys", pane, "Enter"); got != 1 {
		t.Errorf("Enter sent %d times, want 1", got)
	}
}

func TestSendPrompt_DismissKeysAllSentInOrder(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	withShortSendVerify(t, 2*time.Second, time.Millisecond, time.Millisecond)
	withShortSendDismiss(t, time.Millisecond)
	installDismissKeys(t, mgr, []string{"Escape", "C-g"})

	const pane = "%dm6"
	const prompt = "list @internal/agent"
	sess := newIdleSessionWithPane(t, mgr, "/tmp/send-dismiss-multi", "dm6", pane)

	mock.capturedSequence[pane] = []string{"$ ", "$ list @internal/agent", "$ list @internal/agent"}

	if err := mgr.SendPrompt(sess.ID, prompt); err != nil {
		t.Fatalf("SendPrompt returned err=%v, want nil", err)
	}

	escIdx := mock.firstCallIndex("SendKeys", pane, "Escape")
	cgIdx := mock.firstCallIndex("SendKeys", pane, "C-g")
	enterIdx := mock.firstCallIndex("SendKeys", pane, "Enter")
	if escIdx < 0 || cgIdx < 0 || enterIdx < 0 {
		t.Fatalf("missing calls: Escape=%d C-g=%d Enter=%d", escIdx, cgIdx, enterIdx)
	}
	if !(escIdx < cgIdx && cgIdx < enterIdx) {
		t.Errorf("dismiss keys out of order: Escape@%d < C-g@%d < Enter@%d expected", escIdx, cgIdx, enterIdx)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers used by SendPrompt
// ---------------------------------------------------------------------------

func TestNormalizeForVerify(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single-space", " ", ""},
		{"only-whitespace", "   \t\n\r", ""},
		{"single-word", "hello", "hello"},
		// Whitespace is removed, not collapsed: capture-pane inserts a
		// newline at each wrap position where the prompt has nothing, so
		// collapsing to one space would still leave the two sides
		// disagreeing exactly at the seam.
		{"internal-runs-removed", "hello\t\n  world", "helloworld"},
		{"leading-trailing", "  hi  ", "hi"},
		{"cr-lf", "a\r\nb", "ab"},
		// OpenCode draws a vertical bar at the start of every wrapped row.
		{"box-drawing-vertical", "abc\n┃ def", "abcdef"},
		{"box-drawing-range-ends", "a─b╿c", "abc"},
		// Runes just outside U+2500-U+257F must survive untouched.
		{"below-box-range-kept", "a⓿b", "a⓿b"},
		{"above-box-range-kept", "a▀b", "a▀b"},
		{"japanese-kept", "日本語 の テスト", "日本語のテスト"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeForVerify(tc.in); got != tc.want {
				t.Errorf("NormalizeForVerify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPromptTail(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		n      int
		want   string
	}{
		{"short-prompt-full", "hi", 32, "hi"},
		{"exact-length", "abcdefgh", 8, "abcdefgh"},
		{"longer-than-n", "aaaaaaaaaaaaaaaaabcdef", 6, "abcdef"},
		{"whitespace-removed", "aa  \n bb\tcc", 32, "aabbcc"},
		{"whitespace-removed-then-truncate", "aaaaaaaa  bbb ccc", 5, "bbccc"},
		{"empty", "", 32, ""},
		{"only-whitespace", "  \n\t ", 32, ""},
		// Rune-boundary truncation: 5 bytes back from the end of
		// "あいう" (9 bytes) lands mid-rune, so the leading fragment is
		// dropped and only the last whole rune survives. A byte slice
		// here would produce invalid UTF-8 that can never match the
		// equally-normalized haystack.
		{"multibyte-boundary", "あいう", 5, "う"},
		{"multibyte-exact-fit", "あいう", 6, "いう"},
		{"multibyte-whole", "あいう", 32, "あいう"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := promptTail(tc.prompt, tc.n)
			if got != tc.want {
				t.Errorf("promptTail(%q, %d) = %q, want %q", tc.prompt, tc.n, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("promptTail(%q, %d) = %q, which is not valid UTF-8", tc.prompt, tc.n, got)
			}
			if len(got) > tc.n {
				t.Errorf("promptTail(%q, %d) = %q (%d bytes), want at most %d", tc.prompt, tc.n, got, len(got), tc.n)
			}
		})
	}
}

// TestPromptVerifiable pins the predicate callers use to reject prompts
// SendPrompt could not prove landed. Box-drawing-only input is the case the
// old whitespace-only check missed once NormalizeForVerify started stripping
// those runes: it normalizes to nothing, so verify has no needle and would
// accept it without evidence.
func TestPromptVerifiable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"ordinary", "hello", true},
		{"japanese", "日本語", true},
		{"leading-dash", "-R", true},
		{"empty", "", false},
		{"spaces", "   ", false},
		{"mixed-whitespace", " \t\n\r ", false},
		{"box-drawing-only", "────────", false},
		{"box-drawing-and-space", "┃ ─ ┃", false},
		{"box-drawing-with-text", "┃ ok", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptVerifiable(tc.in); got != tc.want {
				t.Errorf("PromptVerifiable(%q) = %v, want %v", tc.in, got, tc.want)
			}
			// The contract that makes the predicate worth having: whatever it
			// rejects is exactly what sendVerifyOK would wave through blind.
			if trivial := sendVerifyOK("$ ", "$ ", tc.in); trivial == tc.want {
				t.Errorf("PromptVerifiable(%q)=%v but sendVerifyOK accepts-blind=%v; "+
					"the two must be exact opposites or unverified prompts slip past", tc.in, tc.want, trivial)
			}
		})
	}
}

func TestChunkPrompt(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want []string
	}{
		{"empty", "", 800, nil},
		{"zero-max", "abc", 0, nil},
		{"single-byte", "a", 800, []string{"a"}},
		{"exact-fit", "abcd", 4, []string{"abcd"}},
		{"one-over", "abcde", 4, []string{"abcd", "e"}},
		{"even-split", "abcdef", 2, []string{"ab", "cd", "ef"}},
		// The cut would land inside the 3-byte "い", so it backs off to
		// the rune boundary and emits a short chunk instead.
		{"multibyte-backoff", "あいう", 4, []string{"あ", "い", "う"}},
		{"multibyte-two-per-chunk", "あいうえ", 6, []string{"あい", "うえ"}},
		// A rune wider than max is emitted whole rather than split — the
		// chunk exceeds max, but progress is guaranteed. Unreachable at
		// the production 800, reachable in a test that shrinks it.
		{"rune-wider-than-max", "あa", 2, []string{"あ", "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkPrompt(tc.in, tc.max)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("chunkPrompt(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			// Invariants that must hold for every input, not just these.
			if joined := strings.Join(got, ""); joined != tc.in && tc.max > 0 {
				t.Errorf("chunks rejoin to %q, want %q (round-trip must be exact)", joined, tc.in)
			}
			for i, c := range got {
				if !utf8.ValidString(c) {
					t.Errorf("chunk %d = %q is not valid UTF-8", i, c)
				}
			}
		})
	}
}

// TestChunkPrompt_LargeRoundTrip drives the production chunk size with
// inputs past tmux's own 16341-byte send-keys limit, which is the failure
// this split exists to avoid.
func TestChunkPrompt_LargeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"ascii-20k", strings.Repeat("abcdefghij", 2000)},
		{"japanese-20k", strings.Repeat("日本語テスト", 1200)},
		{"no-whitespace-blob", strings.Repeat("x", 20000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkPrompt(tc.in, sendChunkMaxBytes)
			if strings.Join(got, "") != tc.in {
				t.Fatalf("round-trip mismatch for %s", tc.name)
			}
			for i, c := range got {
				if len(c) > sendChunkMaxBytes {
					t.Errorf("chunk %d is %d bytes, want at most %d", i, len(c), sendChunkMaxBytes)
				}
				if !utf8.ValidString(c) {
					t.Errorf("chunk %d is not valid UTF-8", i)
				}
			}
		})
	}
}

func TestSendClearRepeats(t *testing.T) {
	cases := []struct {
		name      string
		promptLen int
		want      int
	}{
		{"empty", 0, 4},
		{"under-one-row", 10, 4},
		{"exactly-one-row", sendClearWidthAssumed, 5},
		{"ten-rows", sendClearWidthAssumed * 10, 14},
		// The cap engages well before the count could run away. At the
		// assumed 60-column width that is roughly 30KB of residue; past
		// this point SendPrompt continues best-effort with a possibly
		// incomplete clear (see docs/gotchas.md).
		{"at-cap", sendClearWidthAssumed * sendClearMaxKeys, sendClearMaxKeys},
		{"past-cap", sendClearWidthAssumed * sendClearMaxKeys * 10, sendClearMaxKeys},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sendClearRepeats(tc.promptLen); got != tc.want {
				t.Errorf("sendClearRepeats(%d) = %d, want %d", tc.promptLen, got, tc.want)
			}
		})
	}
}

// TestSendVerifyLookCount pins the scaling of the look budget. Each look
// sends one nudge, and on an agent whose input window follows the cursor
// those nudges are what walk a tall prompt's tail into view — so a longer
// prompt must get more of them, up to a ceiling that bounds the wall-clock
// cost of a send that is never going to verify.
func TestSendVerifyLookCount(t *testing.T) {
	if got := sendVerifyLookCount(0); got != sendVerifyLooksBase {
		t.Errorf("sendVerifyLookCount(0) = %d, want the base %d", got, sendVerifyLooksBase)
	}
	small := sendVerifyLookCount(100)
	large := sendVerifyLookCount(4000)
	if large <= small {
		t.Errorf("sendVerifyLookCount(4000)=%d <= (100)=%d; a taller prompt needs more nudges "+
			"to walk its tail into view", large, small)
	}
	if got := sendVerifyLookCount(1 << 20); got != sendVerifyLooksMax {
		t.Errorf("sendVerifyLookCount(1MB) = %d, want the cap %d", got, sendVerifyLooksMax)
	}
	// A 2KB prompt was measured to need roughly 20 presses on a 48-column
	// pane; the allowance must comfortably clear that or the case this
	// scaling exists for still fails.
	if got := sendVerifyLookCount(2048); got < 25 {
		t.Errorf("sendVerifyLookCount(2048) = %d, too few: 2KB needed ~20 nudges when measured", got)
	}
}

// TestSendVerifyBudget pins the shape of the scaling rule: a bigger send
// must get a bigger budget, or large prompts silently lose their retries.
func TestSendVerifyBudget(t *testing.T) {
	base := sendVerifyBudget(0, 0, 0)
	if base != sendVerifyTimeoutBase {
		t.Errorf("sendVerifyBudget(0, 0, 0) = %v, want %v", base, sendVerifyTimeoutBase)
	}
	// Each term has to move the budget on its own, or the corresponding cost
	// silently stops being covered.
	if got := sendVerifyBudget(10, 0, 0); got <= base {
		t.Errorf("sendVerifyBudget(10, 0, 0) = %v, want > base %v", got, base)
	}
	if got := sendVerifyBudget(0, 100, 0); got <= base {
		t.Errorf("sendVerifyBudget(0, 100, 0) = %v, want > base %v", got, base)
	}
	if got := sendVerifyBudget(0, 0, 30); got <= base {
		t.Errorf("sendVerifyBudget(0, 0, 30) = %v, want > base %v", got, base)
	}
	want := sendVerifyTimeoutBase + 10*sendVerifyPerChunk +
		100*sendVerifyPerClearKey + 30*sendVerifyLookDelay
	if got := sendVerifyBudget(10, 100, 30); got != want {
		t.Errorf("sendVerifyBudget(10, 100, 30) = %v, want %v", got, want)
	}
}

func TestSendVerifyOK(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
		prompt string
		want   bool
	}{
		{
			name:   "empty-prompt-trivially-ok",
			before: "$ ",
			after:  "$ ",
			prompt: "",
			want:   true,
		},
		{
			name:   "tail-appeared-in-after",
			before: "welcome\n$ ",
			after:  "welcome\n$ hello world",
			prompt: "hello world",
			want:   true,
		},
		{
			name:   "tail-missing-from-after",
			before: "welcome\n$ ",
			after:  "welcome\n$ ",
			prompt: "hello world",
			want:   false,
		},
		{
			name:   "tail-preexisted-in-before-same-count-in-after",
			before: "hello world\n$ ",
			after:  "hello world\n$ ",
			prompt: "hello world",
			want:   false,
		},
		{
			name:   "tail-preexisted-but-additional-occurrence-in-after",
			before: "hello world\n$ ",
			after:  "hello world\n$ hello world",
			prompt: "hello world",
			want:   true,
		},
		{
			name:   "long-prompt-verifies-on-tail-only",
			before: "$ ",
			after:  "$ " + strings.Repeat("x", 500) + "TAIL-ANCHOR-END",
			prompt: strings.Repeat("x", 500) + "TAIL-ANCHOR-END",
			want:   true,
		},
		{
			name:   "multiline-prompt-normalized",
			before: "$ ",
			after:  "$ first line and second bit",
			prompt: "first\nline and second bit",
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sendVerifyOK(tc.before, tc.after, tc.prompt); got != tc.want {
				t.Errorf("sendVerifyOK(before=%q, after=%q, prompt=%q) = %v, want %v",
					tc.before, tc.after, tc.prompt, got, tc.want)
			}
		})
	}
}

// legacyVerifyOK reproduces the pre-fix verify: whitespace runs collapsed
// to a single space instead of removed, and no box-drawing handling. It
// exists only so TestSendVerifyOK_CapturedPanes can prove its regression
// specimens actually exercise the bug — a specimen that passes under this
// too would silently contribute nothing.
func legacyVerifyOK(before, after, prompt string) bool {
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	s := collapse(prompt)
	tail := s
	if len(s) > sendVerifyTailBytes {
		tail = s[len(s)-sendVerifyTailBytes:]
	}
	if tail == "" {
		return true
	}
	nAfter := strings.Count(collapse(after), tail)
	if nAfter == 0 {
		return false
	}
	return nAfter > strings.Count(collapse(before), tail)
}

// TestSendVerifyOK_CapturedPanes replays real capture-pane output taken from
// Claude Code and OpenCode driven through a throwaway tmux server. Every
// specimen is a prompt that genuinely landed, so verify must accept all of them.
//
// The `regression` flag marks specimens where the prompt's 32-byte tail
// straddles a wrapped row — the case that used to fail, and the only ones with
// diagnostic power. The others repeat a phrase often enough that some occurrence
// avoids the seam, so they passed before the fix and would keep passing if it
// were reverted.
func TestSendVerifyOK_CapturedPanes(t *testing.T) {
	cases := []struct {
		name       string
		agent      string
		regression bool
		why        string
	}{
		{"noseam", "claude", false, "seam falls outside the tail — control"},
		{"seam", "claude", false, "seam present but misses the tail — control"},
		{"s1", "claude", true, "seam pierces the tail at +28"},
		{"s2", "claude", true, "seam pierces the tail at +16"},
		{"s3", "claude", true, "seam pierces the tail at +4"},
		{"oc1", "opencode", false, "tail fit on one row"},
		{"oc2", "opencode", true, "tail split with a box-drawing bar between the halves"},
		{"ja", "claude", false, "japanese; tail cut mid-UTF-8 by the old byte slice"},
		{"en", "claude", false, "spaced tail wraps at a word boundary"},
		{"w1", "claude", false, "spaced tail, wrap variant"},
		{"w2", "claude", false, "spaced tail, wrap variant"},
		{"w3", "claude", false, "spaced tail, wrap variant"},
		{"blob", "claude", false, "unspaced long token, seam missed the tail"},
	}

	read := func(t *testing.T, name, ext string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", "sendverify", name+"."+ext))
		if err != nil {
			t.Fatalf("read specimen: %v", err)
		}
		return string(b)
	}

	sawRegression := false
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt := read(t, tc.name, "prompt")
			before := read(t, tc.name, "before")
			after := read(t, tc.name, "after")

			if !sendVerifyOK(before, after, prompt) {
				t.Errorf("sendVerifyOK = false, want true (%s agent, %s)", tc.agent, tc.why)
			}
			// The same pane compared against itself must NOT verify: the
			// occurrence count has to increase, not merely be non-zero.
			// Without this, a normalization aggressive enough to match
			// anything would look correct above.
			if sendVerifyOK(before, before, prompt) {
				t.Errorf("sendVerifyOK(before, before) = true, want false (no new occurrence)")
			}
			if tc.regression {
				sawRegression = true
				if legacyVerifyOK(before, after, prompt) {
					t.Errorf("specimen passes under the pre-fix verify too, so it "+
						"guards nothing; re-check %s or drop the regression flag", tc.name)
				}
			}
		})
	}
	if !sawRegression {
		t.Error("no regression specimen ran; the suite has lost its teeth")
	}
}

// TestManager_KillReadsDescriptionUnderLock guards the "changed hands" exit in
// Kill, which logs the session's description on its way out.
//
// TestManager_ConcurrentHookEventsAndKill can catch a race there, but only by
// luck: that exit is taken when the session's identity changes mid-kill, which
// twelve unsynchronised goroutines almost never arrange — measured at roughly
// one detection per 600 runs. Here the branch is forced every time by mutating
// the session inside the window where Kill has released m.mu.
func TestManager_KillReadsDescriptionUnderLock(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := newIdleSessionWithPane(t, mgr, t.TempDir(), "killdesc", "%killdesc")

	// The window is one statement wide — Kill unlocks, then logs — so a
	// single pass rarely lands in it. Repeat, with several writers competing
	// for the mutex the moment Kill drops it.
	for round := 0; round < 40; round++ {
		// StartedAt is one of the fields Kill re-checks after signalling the
		// pane; moving it makes the session look like it changed hands, which
		// is the exit under test. Re-armed each round: the hook fires once.
		mock.onTerminatePaneProcess = func(string) {
			mgr.mu.Lock()
			mgr.sessions[sess.ID].StartedAt = time.Now().Add(time.Hour)
			mgr.mu.Unlock()
		}

		var wg sync.WaitGroup
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					_ = mgr.SetDescription(sess.ID, fmt.Sprintf("desc-%d-%d", w, i))
				}
			}(w)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mgr.Kill(sess.ID)
		}()
		wg.Wait()

		// Kill left the session stopped; put it back so the next round takes
		// the same path rather than exiting early.
		mgr.mu.Lock()
		mgr.sessions[sess.ID].Status = StatusIdle
		mgr.sessions[sess.ID].TmuxPaneID = "%killdesc"
		mgr.mu.Unlock()
	}
}

// TestBuildAgentShellCmd_ExtraEnvIsNotInterpreted is the property adapters are
// promised by the SpawnPlan contract, checked by running the thing rather than
// reading it.
//
// Command is spliced into `$SHELL -ic '<cmd>'`. The single quotes stop the
// OUTER shell, but the inner shell is handed a command to interpret, so
// anything an adapter concatenates INTO Command is live: an id of the form
// `ses_x$(...)` executes, and that id reaches jind-ai unvalidated from a hook
// payload. ExtraEnv is the way out — Manager quotes those values, and a shell
// does not re-scan the result of a parameter expansion — so this test pins that
// a hostile value survives the round trip as text.
func TestBuildAgentShellCmd_ExtraEnvIsNotInterpreted(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	canary := filepath.Join(t.TempDir(), "EXECUTED")
	hostile := "ses_x$(touch " + canary + ")`touch " + canary + "`;touch " + canary
	mgr.SetAgentResolver(&fakeAgentResolver{agents: map[string]Agent{
		"probe": &fakeAgent{spawnFn: func(SpawnOptions) SpawnPlan {
			return SpawnPlan{
				Command:  `printf %s "$JIN_PROBE_VALUE" > ` + filepath.Join(t.TempDir(), "unused"),
				ExtraEnv: map[string]string{"JIN_PROBE_VALUE": hostile},
			}
		}},
	}})

	shellCmd, _, err := mgr.buildAgentShellCmd(spawnSnapshot{
		JinSessionID: "probe-session",
		AgentKind:    "probe",
		StartDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("buildAgentShellCmd: %v", err)
	}
	// The value belongs in the quoted env assignment and nowhere else. It must
	// not appear inside `-ic '...'`, which is the part a shell interprets.
	// Checking this before running tells two failures apart: the canary staying
	// absent because the value never reached the command, versus because the
	// quoting happened to hold.
	inner := shellCmd
	if i := strings.Index(shellCmd, "-ic '"); i >= 0 {
		inner = shellCmd[i:]
	} else {
		t.Fatalf("template no longer wraps the command in -ic '...': %s", shellCmd)
	}
	if strings.Contains(inner, canary) {
		t.Errorf("the ExtraEnv value was spliced into the interpreted command: %s", inner)
	}

	// Running it is the assertion. -ic needs an interactive-capable shell;
	// /bin/sh accepts the flag and the substitutions under test are POSIX.
	out, err := exec.Command("/bin/sh", "-c", shellCmd).CombinedOutput()
	if err != nil {
		t.Logf("shell reported %v (output: %s)", err, out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("a substitution inside an ExtraEnv value was executed by the inner shell")
	}
}

// TestManager_PollPaneDeath_CarriesTheExitStatus pins the wiring between the
// tmux probe and the decision it feeds. A poll that handed classifyPaneDeath a
// constant would either retry every quit or never retry at all, and waiting out
// paneMonitorInterval is the only other way to reach that argument.
func TestManager_PollPaneDeath_CarriesTheExitStatus(t *testing.T) {
	tests := []struct {
		name        string
		deadStatus  int
		wantRespawn bool
	}{
		{"a resume that failed", 1, true},
		{"a process that exited clean", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, mock, _ := newTestManager(t)

			sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: "poll-status"})
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}
			mgr.mu.Lock()
			sess.TmuxPaneID = "%42"
			sess.Status = StatusRunning
			sess.AgentSessionStarted = true
			sess.spawnResumed = true
			sess.StartedAt = time.Now()
			mgr.mu.Unlock()

			mock.mu.Lock()
			mock.deadPanes["%42"] = true
			mock.deadStatus["%42"] = tt.deadStatus
			mock.mu.Unlock()

			dead, _ := mgr.pollPaneDeath(sess, "%42", sess.Description)
			if !dead {
				t.Fatal("pollPaneDeath reported a live pane, want it to read the mock's dead one")
			}
			if got := len(mock.respawnedPanes()) > 0; got != tt.wantRespawn {
				t.Errorf("respawned = %v, want %v", got, tt.wantRespawn)
			}
		})
	}
}
