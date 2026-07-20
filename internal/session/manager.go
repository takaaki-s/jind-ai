package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/debug"
	"github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/internal/tmux"
	"github.com/takaaki-s/jind-ai/internal/transcript"
	"github.com/takaaki-s/jind-ai/internal/worktreehook"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

var debugLog = debug.NewLogger("daemon-debug.log")

// ErrWorktreeDirty is returned when a git worktree has uncommitted changes
// and force removal was not requested.
var ErrWorktreeDirty = errors.New("worktree has uncommitted changes")

// ErrNotWorktree is returned when worktree removal was requested but the
// resolved target directory is not a git worktree (e.g., the main repository
// or a non-git directory). Returned instead of silently succeeding so the
// caller can surface the discrepancy to the user.
var ErrNotWorktree = errors.New("path is not a git worktree")

// Manager owns the jind-ai-side session lifecycle. Every agent-specific
// concern is fetched via agentResolver so no CC-specific literal survives
// in this file after the abstraction refactor.
type Manager struct {
	sessions       map[string]*Session
	store          *Store
	configMgr      *config.Manager
	tmuxClient     tmux.Runner // tmux client for session management
	hookRunner     worktreehook.Runner
	pluginDisp     plugin.Dispatcher
	gitClient      *git.Client
	agentResolver  AgentResolver // resolves AgentKind → Agent adapter (owns Layer C enhancer via Description())
	mu             sync.RWMutex
	paneSlotMu     sync.Mutex // serializes named-slot pane operations (find-then-split is check-then-act; see PaneSplit/PaneClose)
	stateDir       string
	tmuxSocketName string // "" ⇒ tmux.SocketName; tests set an isolated name so ensureTmuxClient does not touch the shared "jin" server
}

// SetTmuxClient sets the tmux client for tmux-based session management.
func (m *Manager) SetTmuxClient(tc tmux.Runner) {
	m.tmuxClient = tc
}

// SetHookRunner installs the worktree post-create hook runner. A nil runner
// disables hook execution (equivalent to worktree.hook_enabled: false).
func (m *Manager) SetHookRunner(r worktreehook.Runner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hookRunner = r
}

// SetPluginDispatcher installs the plugin event dispatcher. A nil dispatcher
// disables plugin event publishing.
func (m *Manager) SetPluginDispatcher(d plugin.Dispatcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pluginDisp = d
}

// SetTmuxSocketName overrides the tmux socket name used by ensureTmuxClient's
// lazy fallback (production leaves this empty and gets tmux.SocketName).
// Tests set an isolated per-run name so a test that exercises the auto-init
// path — where the caller deliberately skips SetTmuxClient — cannot leak a
// real "-L jin" server that would then pollute a subsequent daemon start's
// environment inheritance.
//
// Set exactly once before the first session start; no lock is taken.
func (m *Manager) SetTmuxSocketName(name string) {
	m.tmuxSocketName = name
}

// SetAgentResolver installs the AgentResolver used by startSessionTmux and
// HandleHookEvent to fetch adapter behaviour. Left nil, session start returns
// an error rather than defaulting silently.
//
// Must be called exactly once at startup, before any goroutine reads the
// resolver (daemon.NewServer wires this before returning; tests inject a
// stub before touching the Manager). No lock is taken here to match the
// other one-shot setters (SetTmuxClient / SetHookRunner) — installing at
// runtime while other goroutines are already reading would race regardless.
func (m *Manager) SetAgentResolver(ar AgentResolver) {
	m.agentResolver = ar
}

// RecoverTmuxSessions checks for sessions with existing tmux windows after daemon restart
// and resumes monitoring for live ones, or clears stale TmuxWindowName for dead ones.
func (m *Manager) RecoverTmuxSessions() {
	if m.tmuxClient == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.recoverTmuxSessionsLocked()
}

// recoverTmuxSessionsLocked is the lock-held version of RecoverTmuxSessions.
// Caller must hold m.mu.
func (m *Manager) recoverTmuxSessionsLocked() {
	if m.tmuxClient == nil {
		return
	}

	for _, session := range m.sessions {
		// Consume the status read from disk at load time so a later
		// recovery pass cannot resurrect a stale value.
		fromDisk := session.PersistedStatus
		session.PersistedStatus = ""

		if session.TmuxWindowName == "" {
			// Fix stale sessions: active status but no tmux session (from prior recovery bug)
			if session.Status != StatusStopped && session.Status != StatusCreating {
				session.Status = StatusStopped
				_ = m.store.Save(session)
				debugLog("[RECOVER] Session %s has active status but no tmux session, marked stopped", session.Description)
			}
			continue
		}

		// Check if the inner tmux session still exists
		if !m.tmuxClient.HasSession(session.TmuxWindowName) {
			session.TmuxWindowName = ""
			session.Status = StatusStopped
			_ = m.store.Save(session)
			debugLog("[RECOVER] Session %s inner tmux session gone, marked stopped", session.Description)
			continue
		}

		target := session.TmuxPaneID

		// Check if pane is dead — keep TmuxWindowName (session alive via remain-on-exit)
		if m.tmuxClient.IsPaneDead(target) {
			session.Status = StatusStopped
			_ = m.store.Save(session)
			debugLog("[RECOVER] Session %s tmux pane dead, kept TmuxWindowName (session preserved)", session.Description)
			continue
		}

		// Session exists and pane is alive - resume monitoring.
		// The hook-driven status persisted before the restart
		// (idle/thinking/permission) is the best estimate of the session's
		// real state; only detail-less states fall back to Running. A live
		// in-memory status (hooks may have fired since load) wins over the
		// on-disk value.
		persisted := session.Status
		if (persisted == StatusStopped || persisted == StatusCreating) && fromDisk != "" {
			persisted = fromDisk
		}
		switch persisted {
		case StatusIdle, StatusThinking, StatusPermission:
			session.Status = persisted
		default:
			session.Status = StatusRunning
		}

		// Hooks fired while the daemon was down are lost, so the persisted
		// value itself can be stale (e.g. a missed Stop hook leaves the
		// session "thinking" forever). Let the adapter re-derive the status
		// from its own persistent data (Claude Code: the transcript); a false
		// verdict keeps the decision above. Only Status is applied — see the
		// "recover" contract on StatusSignal.
		if upd, ok := m.recoverStatusVerdict(session, persisted); ok {
			session.Status = upd.Status
		}

		session.LastOutputTime = time.Now()
		_ = m.store.Save(session)
		debugLog("[RECOVER] Session %s has live inner tmux session, resuming monitoring (status: %s)", session.Description, session.Status)

		go m.captureOutputTmux(session)
	}
}

// recoverStatusVerdict asks the session's agent adapter to re-derive the
// status of a recovered pane-alive session from agent-side persistent data
// (the Claude Code adapter reads the transcript's last turn). persisted is
// the status loaded from disk, snapshotted before the caller's Running
// normalization. Returns false when no resolver is configured, the kind is
// unknown, or the adapter cannot tell — the caller then keeps its own
// decision. Caller must hold m.mu.
func (m *Manager) recoverStatusVerdict(session *Session, persisted Status) (StatusUpdate, bool) {
	if m.agentResolver == nil {
		return StatusUpdate{}, false
	}
	ag, err := m.agentResolver.Resolve(session.AgentKind)
	if err != nil {
		debugLog("[RECOVER] Session %s: cannot resolve agent %q: %v", session.Description, session.AgentKind, err)
		return StatusUpdate{}, false
	}
	return ag.StatusSource().Interpret(StatusSignal{
		Kind: "recover",
		Payload: map[string]string{
			"persisted_status": string(persisted),
			"agent_session_id": session.AgentSessionID,
			"workdir":          session.WorkDir,
		},
	})
}

// ensureTmuxClient lazily initializes the inner tmux client (-L jin).
// Each CC session creates its own tmux session, so no shared session is needed.
//
// Uses tmux.SocketName ("jin") in production; tests override via
// SetTmuxSocketName so an auto-init on the shared socket doesn't leak a
// server the next daemon start would inherit env from.
func (m *Manager) ensureTmuxClient() {
	if m.tmuxClient != nil {
		return
	}
	socketName := m.tmuxSocketName
	if socketName == "" {
		socketName = tmux.SocketName
	}
	tc, err := tmux.NewClientWithSocket(socketName)
	if err != nil {
		return
	}
	m.tmuxClient = tc
	debugLog("[TMUX] Inner tmux client initialized (socket: %s)", socketName)
	// Don't call configureInnerTmux here — the inner tmux server may not exist yet.
	// Configuration is applied in startSessionTmux after the first session is created.
	m.recoverTmuxSessionsLocked()
}

// configureInnerTmux applies jin-specific settings to the inner tmux server.
// User's ~/.tmux.conf is automatically loaded by tmux on server startup.
// Must only be called after the inner tmux server is confirmed to exist (i.e., after
// a session has been created).
// This is called every time a session is started (not just once) because the inner
// tmux server may have exited and restarted between sessions. The overhead is minimal.
//
// Note: remain-on-exit is NOT set globally. It is set per-pane only on managed
// (tagged) panes via TagManagedPane, so user-added panes are immediately destroyed
// on exit instead of showing "Pane is dead".
func (m *Manager) configureInnerTmux() {
	if m.tmuxClient == nil {
		return
	}
	// pane-died hook as safety net: kill any dead panes that lack the keep tag.
	_ = m.tmuxClient.SetupAutoCleanDeadPanes()
	debugLog("[TMUX] Inner tmux server configured (pane-died hook)")
}

// NewManager creates a new session manager.
//
// sessionsDir is where per-session JSON files live; stateDir is where
// generated artifacts such as hooks-settings.json are written.
func NewManager(sessionsDir, stateDir string, configMgr *config.Manager) (*Manager, error) {
	store, err := NewStore(sessionsDir)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		sessions:  make(map[string]*Session),
		store:     store,
		configMgr: configMgr,
		gitClient: git.NewClient(),
		stateDir:  stateDir,
	}

	// Load existing sessions
	sessions, err := store.LoadAll()
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		// Normalize to Stopped in memory (the process may be gone), but keep
		// the on-disk value: recovery uses it to restore the hook-derived
		// status of sessions whose pane turns out to still be alive.
		s.PersistedStatus = s.Status
		s.Status = StatusStopped
		if s.Fleet == "" {
			s.Fleet = DefaultFleet
		}
		// IsWorktree is json:"-" so it's lost on restart; recover it from
		// disk so the TUI's delete modal shows the worktree option
		// immediately, without waiting for the 10s captureOutputTmux poll.
		s.IsWorktree = git.IsGitWorktreeDir(s.WorkDir)
		m.sessions[s.ID] = s
	}

	return m, nil
}

// CreateOptions contains options for creating a new session
type CreateOptions struct {
	WorkDir     string // Working directory path
	Description string // Human-readable session description (empty = auto-generated)
	Fleet       string // Fleet name for session grouping; defaults to DefaultFleet if empty
	AgentKind   string // Adapter identifier; defaults to "claude" if empty

	Worktree       bool   // Create a git worktree for this session
	NoHook         bool   // Skip the worktree post-create hook (worktree path only)
	WorktreeName   string // Override auto-generated worktree name
	WorktreeBranch string // Override auto-generated branch name
	WorktreeBase   string // Override auto-detected base branch (default: origin/HEAD)
}

// CreateWithOptions creates a new session with full options.
//
// The second return value is a non-fatal warning message (e.g. hook skipped
// because the repository is not allowlisted). Empty when there is nothing to
// surface. It is intentionally NOT stored on Session, so subsequent Get/List
// responses do not carry a stale warning.
//
// Uses named returns so a deferred rollback (in the worktree path) can detect
// whether a later step failed and clean up the created worktree/branch.
//
// Lock discipline (worktree path): git operations run outside m.mu; the
// sessions map is re-checked under lock after worktree creation. Holding
// m.mu across git subprocesses would block reads (List, Get, SetStatus)
// on the whole daemon for the duration of the worktree add.
func (m *Manager) CreateWithOptions(opts CreateOptions) (result *Session, warning string, retErr error) {
	if opts.Fleet == "" {
		opts.Fleet = DefaultFleet
	}

	// WorkDir is required
	if opts.WorkDir == "" {
		return nil, "", fmt.Errorf("work directory is required")
	}

	// Pre-generate the session ID so the auto-derived worktree name can key
	// off it. Also becomes Session.ID below so we only ever mint one UUID.
	sessionID := uuid.New().String()

	var (
		worktreeCreated bool
		worktreePath    string
		branch          string
		originalRepoDir string
		hookWarning     string
	)

	if opts.Worktree {
		if !git.IsGitRoot(opts.WorkDir) {
			return nil, "", fmt.Errorf("not a git repository: %s", opts.WorkDir)
		}

		cfg := m.configMgr.GetWorktreeConfig()

		base := opts.WorktreeBase
		if base == "" {
			detected, err := m.gitClient.DetectDefaultBranch(opts.WorkDir)
			if err != nil {
				base = cfg.DefaultBranch
				if base == "" {
					return nil, "", fmt.Errorf("cannot detect default branch: %w", err)
				}
			} else {
				base = detected
			}
		}

		originalRepoDir = opts.WorkDir
		repoBasename := filepath.Base(originalRepoDir)
		baseName := deriveWorktreeName(sessionID, opts.WorktreeName)

		// Clear orphan worktree registrations (`.git/worktrees/<name>/` metadata
		// left after a manual `rm -rf` of the worktree directory) so the
		// collision check below reflects the true git state. Best-effort:
		// prune failures shouldn't block session creation.
		if err := m.gitClient.PruneWorktrees(originalRepoDir); err != nil {
			debugLog("[WORKTREE] prune failed for %s: %v", originalRepoDir, err)
		}

		var finalName string
		if opts.WorktreeName != "" {
			// Explicit override: honour the user's choice verbatim. Pre-check
			// the branch so we fail fast with a clear message instead of
			// leaking git's raw "fatal: branch 'X' already exists" through
			// AddWorktree.
			finalName = opts.WorktreeName
			branch = deriveBranchName(finalName, cfg.BranchPrefix, opts.WorktreeBranch)
			if m.gitClient.BranchExists(originalRepoDir, branch) {
				return nil, "", fmt.Errorf("branch %q already exists", branch)
			}
		} else {
			collides := func(candidate string) bool {
				candidatePath, err := expandBaseDir(cfg.BaseDir, candidate, repoBasename)
				if err != nil {
					return true
				}
				if _, err := os.Stat(candidatePath); err == nil {
					return true
				}
				candidateBranch := deriveBranchName(candidate, cfg.BranchPrefix, opts.WorktreeBranch)
				return m.gitClient.BranchExists(originalRepoDir, candidateBranch)
			}
			name, err := findAvailableWorktreeName(baseName, collides)
			if err != nil {
				return nil, "", err
			}
			finalName = name
			branch = deriveBranchName(finalName, cfg.BranchPrefix, opts.WorktreeBranch)
		}

		var err error
		worktreePath, err = expandBaseDir(cfg.BaseDir, finalName, repoBasename)
		if err != nil {
			return nil, "", err
		}

		if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
			return nil, "", fmt.Errorf("creating worktree parent dir: %w", err)
		}

		if err := m.gitClient.AddWorktree(originalRepoDir, branch, worktreePath, "origin/"+base); err != nil {
			return nil, "", fmt.Errorf("git worktree add: %w", err)
		}

		worktreeCreated = true
		defer func() {
			if retErr != nil && worktreeCreated {
				if err := m.gitClient.RemoveWorktree(worktreePath, true); err != nil {
					debugLog("[WORKTREE] rollback: RemoveWorktree failed for %s: %v", worktreePath, err)
				}
				if err := m.gitClient.DeleteBranch(originalRepoDir, branch); err != nil {
					debugLog("[WORKTREE] rollback: DeleteBranch failed for %s: %v", branch, err)
				}
			}
		}()

		// Post-create hook: runs synchronously inside the rollback window so a
		// non-zero exit tears the worktree/branch back down. Skipped silently
		// when the caller opts out, the runner is not wired up, or config
		// disables the feature.
		if !opts.NoHook && m.hookRunner != nil &&
			(cfg.HookEnabled == nil || *cfg.HookEnabled) {

			scriptPath, exists := m.hookRunner.Discover(originalRepoDir)
			if exists {
				verdict, verifyErr := m.hookRunner.Verify(scriptPath, originalRepoDir)
				if verifyErr != nil {
					// Verify may return a verdict alongside err (e.g. hash
					// failure); treat err as authoritative and abort before
					// switching on verdict to avoid running an unverified hook.
					return nil, "", fmt.Errorf("verify worktree hook: %w", verifyErr)
				}
				switch verdict {
				case worktreehook.VerdictOK:
					timeout := time.Duration(cfg.HookTimeout) * time.Second
					ctx, cancel := context.WithTimeout(context.Background(), timeout)
					logPath := m.hookRunner.HookLogPath(m.stateDir, sessionID)
					runErr := m.hookRunner.Run(ctx, worktreehook.RunOptions{
						ScriptPath:   scriptPath,
						WorktreePath: worktreePath,
						RepoRoot:     originalRepoDir,
						Branch:       branch,
						Base:         base,
						SessionID:    sessionID,
						SessionName:  opts.Description,
						LogPath:      logPath,
						Timeout:      timeout,
					})
					cancel()
					if runErr != nil {
						return nil, "", fmt.Errorf("worktree post-create hook failed: %w (log: %s)", runErr, logPath)
					}
				case worktreehook.VerdictNotAllowed:
					hookWarning = fmt.Sprintf("Post-create hook detected but not allowed: %s. Run `jin worktree allow` to trust this repository.", scriptPath)
					debugLog("[WORKTREE] hook not allowed for %s (run: jin worktree allow)", originalRepoDir)
				case worktreehook.VerdictChanged:
					hookWarning = "Post-create hook script changed since last allow. Run `jin worktree allow` to re-trust."
					debugLog("[WORKTREE] hook script changed for %s (run: jin worktree allow)", originalRepoDir)
				}
			}
		}

		opts.WorkDir = worktreePath
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for duplicate directories
	// Skip sessions whose CurrentWorkDir is inside a worktree — they have
	// "moved away" from their persisted WorkDir and should not block new
	// sessions for that directory.
	for _, s := range m.sessions {
		if s.WorkDir == opts.WorkDir && !git.IsClaudeWorktreePath(s.CurrentWorkDir) {
			return nil, "", fmt.Errorf("session already exists for directory: %s (session: %s)", opts.WorkDir, s.Description)
		}
	}

	id := sessionID

	// Layer A: derive a repo-based baseline label when the caller did not
	// supply a manual description. opts.WorkDir here is the *final* path (a
	// worktree path in the worktree branch, otherwise the caller's request),
	// which is the invariant GenerateBaselineDescription documents. The
	// original repo name is intentionally *not* threaded into worktree
	// sessions at this layer — Layer C is expected to enrich those later.
	description := strings.TrimSpace(opts.Description)
	locked := true
	if description == "" {
		description = GenerateBaselineDescription(opts.WorkDir, "", false, "")
		locked = false
	}

	// Mint the adapter-side session ID up front. Every adapter jind-ai knows
	// about needs some kind of persistent handle (Claude Code's --session-id,
	// Codex's conversation id, ...) and a fresh UUID is a safe universal
	// default. Adapters that don't need one can ignore the value.
	agentSessionID := uuid.New().String()

	agentKind := opts.AgentKind
	if agentKind == "" {
		agentKind = "claude"
	}

	session := &Session{
		ID:                id,
		Description:       description,
		DescriptionLocked: locked,
		WorkDir:           opts.WorkDir,
		CreatedAt:         time.Now(),
		Status:            StatusStopped,
		AgentKind:         agentKind,
		AgentSessionID:    agentSessionID,
		Fleet:             opts.Fleet,
		// Set IsWorktree immediately so the TUI delete modal offers the
		// worktree removal option without waiting for the 10s
		// captureOutputTmux poll cycle. `opts.Worktree` reflects "did we
		// just create a worktree"; also check the WorkDir for cases where
		// the user pointed at an existing worktree directly.
		IsWorktree: opts.Worktree || git.IsGitWorktreeDir(opts.WorkDir),
	}

	if err := m.store.Save(session); err != nil {
		return nil, "", err
	}
	m.sessions[id] = session

	return session, hookWarning, nil
}

// List returns all sessions sorted by creation time
func (m *Manager) List() []Info {
	// Phase 1: Snapshot session data under RLock (no I/O)
	m.mu.RLock()
	infos := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		infos = append(infos, s.ToInfo())
	}
	m.mu.RUnlock()

	// Phase 2: Enrich with transcript data outside lock (slow I/O)
	reader := transcript.NewReader()
	for i := range infos {
		info := &infos[i]
		if info.AgentSessionID != "" && info.WorkDir != "" {
			if msgs, err := reader.GetLastMessages(info.WorkDir, info.AgentSessionID); err == nil && msgs != nil {
				if msgs.User != nil {
					info.LastUserMessage = transcript.TruncateMessage(msgs.User.Content, 500)
				}
				if msgs.Assistant != nil {
					info.LastAssistantMessage = transcript.TruncateMessageFromEnd(msgs.Assistant.Content, 500)
				}
			}
		}
	}

	SortInfos(infos)

	return infos
}

// Get returns a session by ID
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// SetStatus updates the status of a session
func (m *Manager) SetStatus(id string, status Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[id]; ok {
		session.Status = status
	}
}

// Verify-by-capture tuning. Kept as vars so tests can shorten them.
// See SendPrompt for the mechanism, and docs/gotchas.md for the rationale.
var (
	// sendVerifyTimeout bounds the whole send-verify retry loop. On
	// timeout SendPrompt returns an error before ever pressing Enter,
	// so buffered/dropped keystrokes never reach the agent as a
	// half-formed prompt.
	sendVerifyTimeout = 5 * time.Second
	// sendVerifySettleDelay is how long we wait between SendKeysLiteral
	// and the follow-up CapturePane. Empirically ~50ms is enough for
	// tmux to reflect the literal into the pane's visible buffer.
	sendVerifySettleDelay = 50 * time.Millisecond
	// sendVerifyBackoff is the pause between a failed verify and the
	// next re-send. Kept small so a genuinely-not-ready TUI recovers
	// within a few hundred ms once it is ready.
	sendVerifyBackoff = 100 * time.Millisecond
	// sendVerifyTailBytes controls how many trailing bytes of the prompt
	// we look for in the capture. Long prompts get wrapped by the TUI's
	// input area, so matching only the tail avoids reflow false negatives
	// while still uniquely identifying the send.
	sendVerifyTailBytes = 32
)

// promptTail returns the last n bytes of prompt with whitespace
// collapsed. Used by sendVerifyOK to build a compact needle that
// survives TUI reflow, ANSI decoration and trailing newlines.
func promptTail(prompt string, n int) string {
	s := collapseWS(prompt)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// collapseWS folds every run of whitespace into a single space and trims
// the result. This makes verify tolerant to the TUI splitting the input
// across multiple visible rows or padding it with cursor-positioning
// spaces. strings.Fields already splits on unicode.IsSpace, so joining
// its output with a single space produces the same normalized form.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// sendVerifyOK reports whether the pane's captured content shows that
// the prompt landed in the TUI's input area since the pre-send snapshot.
// The check compares occurrence counts of promptTail(prompt) between
// before and after so that pane content that already carried the tail
// (previous conversation, help text, etc.) does not falsely satisfy
// the verify.
//
// An empty prompt is treated as trivially accepted: SendPrompt is only
// reachable via daemon.handleSend which already rejects empty and
// whitespace-only prompts, so this branch exists only to keep the
// helper total.
func sendVerifyOK(before, after, prompt string) bool {
	tail := promptTail(prompt, sendVerifyTailBytes)
	if tail == "" {
		return true
	}
	nAfter := strings.Count(collapseWS(after), tail)
	if nAfter == 0 {
		return false
	}
	return nAfter > strings.Count(collapseWS(before), tail)
}

// SendPrompt sends a prompt to a session's tmux pane.
// The session must be in idle status.
//
// tmux send-keys is fire-and-forget from tmux's point of view: it
// always reports success, even when the TUI has not finished its
// startup redraw and drops the incoming keys. To make this observable,
// SendPrompt captures the pane before and after each send attempt and
// checks that the tail of prompt appeared in the visible buffer.
// Attempts repeat with backoff until the check passes or
// sendVerifyTimeout elapses. Enter is only pressed after verify
// succeeds, so fully-dropped prompts never get committed.
//
// Known limit: verify checks "did the tail appear once more", not
// "did exactly the prompt land once". A TUI that keeps a dropped
// prefix from the first attempt can end up with "<prefix><full prompt>"
// concatenated in its input buffer, and verify still passes. Agent-
// agnostic guards (kill-line before retry, echo-diff on the exact
// prompt) all leak per-TUI assumptions into this transport-layer
// helper, so we accept the risk. Revisit if corruption is ever
// observed against a real agent.
func (m *Manager) SendPrompt(id, prompt string) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("session not found: %s", id)
	}
	if sess.Status != StatusIdle {
		m.mu.RUnlock()
		return fmt.Errorf("session is not idle (current status: %s)", sess.Status)
	}
	paneID := sess.TmuxPaneID
	m.mu.RUnlock()

	if paneID == "" {
		return fmt.Errorf("session has no tmux pane")
	}
	if m.tmuxClient == nil {
		return fmt.Errorf("tmux client not available")
	}

	deadline := time.Now().Add(sendVerifyTimeout)
	attempts := 0
	for {
		attempts++

		before, err := m.tmuxClient.CapturePane(paneID, false)
		if err != nil {
			return fmt.Errorf("capture-pane before failed: %w", err)
		}

		if err := m.tmuxClient.SendKeysLiteral(paneID, prompt); err != nil {
			return fmt.Errorf("failed to send prompt: %w", err)
		}

		time.Sleep(sendVerifySettleDelay)

		after, err := m.tmuxClient.CapturePane(paneID, false)
		if err != nil {
			return fmt.Errorf("capture-pane after failed: %w", err)
		}

		if sendVerifyOK(before, after, prompt) {
			break
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"send verify: prompt did not appear in pane within %s (attempts=%d); "+
					"the TUI may not have been ready to receive input",
				sendVerifyTimeout, attempts)
		}
		time.Sleep(sendVerifyBackoff)
	}

	if err := m.tmuxClient.SendKeys(paneID, "Enter"); err != nil {
		return fmt.Errorf("failed to send Enter: %w", err)
	}
	return nil
}

// paneTargetLocked resolves a session's tmux target: the recorded pane ID when
// available, else the window.pane fallback. It reads session fields directly
// and takes no lock, so callers arrange safe access themselves (PaneTarget
// holds the read lock; captureOutputTmux reads at startup like pre-refactor).
func paneTargetLocked(session *Session) (string, error) {
	if session.TmuxPaneID != "" {
		return session.TmuxPaneID, nil
	}
	if session.TmuxWindowName != "" {
		return tmux.WindowTarget(session.TmuxWindowName, 0), nil
	}
	return "", fmt.Errorf("session has no tmux pane")
}

// PaneTarget resolves the tmux target for a session by ID.
func (m *Manager) PaneTarget(id string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[id]
	if !ok {
		return "", fmt.Errorf("session not found: %s", id)
	}
	return paneTargetLocked(sess)
}

// PanePopup opens a tmux popup running cmd for the session, anchored to its
// pane and started in the session's working directory.
func (m *Manager) PanePopup(id, cmd, title, width, height string) error {
	if m.tmuxClient == nil {
		return fmt.Errorf("tmux is not available")
	}
	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("session not found: %s", id)
	}
	target, err := paneTargetLocked(sess)
	workDir := sess.WorkDir
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	return m.tmuxClient.DisplayPopup(tmux.DisplayPopupOptions{
		Target: target,
		Cmd:    cmd,
		Title:  title,
		Width:  width,
		Height: height,
		Dir:    workDir,
	})
}

// PaneSplit splits the session's pane in its working directory and returns
// the new pane's ID. With name set the split becomes idempotent: when a pane
// with that name already exists in the session's window, no new pane is
// created — the existing pane is returned as-is (noop), respawned with
// opts.Cmd (respawn), or reported as an error (error), per ifExists.
// The caller (daemon handler) validates name/ifExists/opts; the manager
// trusts them and only injects the session's working directory.
func (m *Manager) PaneSplit(id, name, ifExists string, opts tmux.SplitOptions) (string, error) {
	if m.tmuxClient == nil {
		return "", fmt.Errorf("tmux is not available")
	}
	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return "", fmt.Errorf("session not found: %s", id)
	}
	target, err := paneTargetLocked(sess)
	opts.Dir = sess.WorkDir
	m.mu.RUnlock()
	if err != nil {
		return "", err
	}

	// Named-slot idempotency is check-then-act inside EnsureNamedPane, and the
	// daemon handles connections concurrently — serialize so two simultaneous
	// splits of the same slot cannot both miss the find and split twice.
	if name != "" {
		m.paneSlotMu.Lock()
		defer m.paneSlotMu.Unlock()
	}
	return tmux.EnsureNamedPane(m.tmuxClient, target, name, ifExists, opts)
}

// PaneClose kills the pane named name in the session's window. It refuses to
// kill the session's agent pane even if that pane somehow carries the name.
func (m *Manager) PaneClose(id, name string) error {
	if m.tmuxClient == nil {
		return fmt.Errorf("tmux is not available")
	}
	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("session not found: %s", id)
	}
	target, err := paneTargetLocked(sess)
	agentPane := sess.TmuxPaneID
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	m.paneSlotMu.Lock()
	defer m.paneSlotMu.Unlock()
	return tmux.CloseNamedPane(m.tmuxClient, target, name, agentPane)
}

// PaneCapture returns the visible contents of the session's pane.
func (m *Manager) PaneCapture(id string, ansi bool) (string, error) {
	if m.tmuxClient == nil {
		return "", fmt.Errorf("tmux is not available")
	}
	target, err := m.PaneTarget(id)
	if err != nil {
		return "", err
	}
	return m.tmuxClient.CapturePane(target, ansi)
}

// PaneSendKeys sends keys to the session's pane. When literal is true the keys
// are typed verbatim; otherwise they are interpreted as tmux key names.
func (m *Manager) PaneSendKeys(id, keys string, literal bool) error {
	if m.tmuxClient == nil {
		return fmt.Errorf("tmux is not available")
	}
	target, err := m.PaneTarget(id)
	if err != nil {
		return err
	}
	if literal {
		return m.tmuxClient.SendKeysLiteral(target, keys)
	}
	return m.tmuxClient.SendKeys(target, keys)
}

// SetStatusWithError updates the status and error message of a session
func (m *Manager) SetStatusWithError(id string, status Status, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[id]; ok {
		session.Status = status
		session.ErrorMessage = errMsg
		_ = m.store.Save(session)
	}
}

// SetWorkDir updates the work directory of a session
// Returns error if the workDir is already in use by another session
func (m *Manager) SetWorkDir(id string, workDir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Duplicate check (prevents conflicts in async mode)
	if workDir != "" {
		for _, s := range m.sessions {
			if s.ID != id && s.WorkDir == workDir && !git.IsClaudeWorktreePath(s.CurrentWorkDir) {
				return fmt.Errorf("WorkDir already in use by session %s", s.Description)
			}
		}
	}

	if session, ok := m.sessions[id]; ok {
		session.WorkDir = workDir
		// Persist the change
		_ = m.store.Save(session)
	}
	return nil
}

// SetDescription updates a session's description. Passing an empty value
// (or a whitespace-only value) clears the manual lock and regenerates the
// Layer A baseline from the session's WorkDir, so subsequent Layer C upgrades
// can take over again.
//
// The baseline is regenerated with the same (WorkDir, "", false, "") arguments
// that CreateWithOptions and TryUpgradeDescription use, keeping all three
// call sites' notion of "the baseline" byte-identical. Any drift here would
// silently block Layer C from firing after unlock (see F001/F004).
func (m *Manager) SetDescription(id string, desc string) error {
	desc = strings.TrimSpace(desc)

	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}

	if desc == "" {
		session.Description = GenerateBaselineDescription(session.WorkDir, "", false, "")
		session.DescriptionLocked = false
		session.DescriptionLayer = DescriptionLayerBaseline
	} else {
		session.Description = desc
		session.DescriptionLocked = true
	}
	return m.store.Save(session)
}

// TryUpgradeDescription asks the given enhancer for a Layer C description and
// applies it when two layer guards allow the write. Callers should invoke it
// from every hook event that might carry new signal; guard-heavy internal
// short-circuiting is what keeps repeated calls cheap.
//
// Guard 1 (restart protection) is Session.descriptionDriftedFrom: refuse to
// overwrite a Description that a previous daemon process already upgraded.
//
// Guard 2 (monotonic layer): reject candidates whose layer is not strictly
// greater than the session's current layer. This lets us call the same
// enhancer on both SessionStart (transcript miss → LayerAgentName) and later
// UserPromptSubmit (transcript hit → LayerTranscript) without the second call
// getting rejected by a baseline-equality check, while still preventing a
// same-layer or lower-layer proposal from clobbering a better value.
//
// A nil enhancer (or an unknown session id, or a locked description) is a
// silent no-op so callers do not need to guard hook wiring.
//
// The enhancer scans the agent transcript end to end and the store write hits
// the filesystem, so neither runs under m.mu — that is the Manager's central
// lock, and holding it across this I/O stalls every other session. Only the
// snapshot and the commit take the lock; everything between them is lock-free,
// which means the session can change in the gap. commitDescriptionUpgrade
// therefore re-evaluates every guard against live state before writing.
func (m *Manager) TryUpgradeDescription(id string, enhancer DescriptionEnhancer) {
	if enhancer == nil {
		return
	}

	snapshot, ok := m.snapshotForUpgrade(id)
	if !ok {
		return
	}

	// Baseline must be computed with the same arguments CreateWithOptions and
	// SetDescription use. Threading CurrentBranch / IsWorktree / TmuxWindowName
	// here would make the comparison miss as soon as captureOutputTmux populates
	// those runtime fields, silently disabling Layer C on the very first poll.
	baseline := GenerateBaselineDescription(snapshot.WorkDir, "", false, "")

	// Guard 1, evaluated against the snapshot purely to skip the transcript
	// scan. commitDescriptionUpgrade runs the authoritative one.
	if snapshot.descriptionDriftedFrom(baseline) {
		return
	}

	candidate, layer, ok := enhancer.TryGenerate(&snapshot)
	if !ok || candidate == "" {
		return
	}

	saved, ok := m.commitDescriptionUpgrade(id, &snapshot, baseline, candidate, layer)
	if !ok {
		return
	}
	// Save the copy rather than the live session: Store.Save marshals every
	// field, so handing it the live pointer outside the lock would race with
	// concurrent mutators. The copy can persist a Status that a concurrent Save
	// has already superseded; that is accepted, since memory stays
	// authoritative and the next Save reconverges.
	_ = m.store.Save(&saved)
}

// snapshotForUpgrade returns an independent copy of the session to hand to an
// enhancer running without the lock, or ok=false when the session is unknown or
// its description is user-locked.
//
// The copy is safe because no Session field aliases mutable state: they are
// strings, bools, ints and time.Time, whose internal *Location is immutable
// and shared.
func (m *Manager) snapshotForUpgrade(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok || session.DescriptionLocked {
		return Session{}, false
	}
	return *session, true
}

// commitDescriptionUpgrade applies a candidate produced from snapshot, after
// re-running every guard against live state. It returns the value to persist.
//
// Re-running the guards, rather than diffing snapshot against live field by
// field, is what lets a write that landed during the unlocked window win: a
// deletion misses the map, a manual SetDescription has set DescriptionLocked,
// and a concurrent upgrade has raised DescriptionLayer past Guard 2.
func (m *Manager) commitDescriptionUpgrade(id string, snapshot *Session, baseline, candidate string, layer DescriptionLayer) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok || session.DescriptionLocked {
		return Session{}, false
	}

	// baseline describes snapshot.WorkDir. Once the session moves it says
	// nothing about the session in front of us, so drop the round rather than
	// compare against a stale value; the next hook recomputes both. (baseline
	// also depends on the filesystem layout around WorkDir, which cannot be
	// pinned down the same way — this is a best-effort check, and a miss only
	// costs one skipped round.)
	if session.WorkDir != snapshot.WorkDir {
		return Session{}, false
	}

	// Guard 1, authoritative.
	if session.descriptionDriftedFrom(baseline) {
		return Session{}, false
	}

	// Guard 2: only promote strictly upward.
	if layer <= session.DescriptionLayer {
		return Session{}, false
	}

	session.Description = candidate
	session.DescriptionLayer = layer
	return *session, true
}

// CountActive returns the number of active sessions (creating, running, thinking, permission)
// Excludes: stopped, idle, error
func (m *Manager) CountActive() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, s := range m.sessions {
		switch s.Status {
		case StatusCreating, StatusRunning, StatusThinking, StatusPermission:
			count++
		}
	}
	return count
}

// StartBackground starts a session in the background
func (m *Manager) StartBackground(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}

	if isProcessRunning(session) {
		return nil // Already running
	}

	return m.startSession(session)
}

// isProcessRunning returns true if the session process is running
// (any status except StatusStopped means the process is alive)
func isProcessRunning(s *Session) bool {
	if s.Status == StatusStopped {
		return false
	}
	// tmux mode: process is running if we have a tmux window name
	return s.TmuxWindowName != ""
}

// startSession starts a session's process in a tmux window.
func (m *Manager) startSession(session *Session) error {
	// Try to detect tmux session if not already connected
	// (may trigger recovery which sets session to Running)
	m.ensureTmuxClient()

	// Re-check: recovery in ensureTmuxClient may have found this session alive
	if isProcessRunning(session) {
		return nil
	}

	return m.startSessionTmux(session)
}

// expandTilde expands a leading ~ in a path to the current user's home directory.
// This runs on the target machine (local or remote slave), so os.UserHomeDir()
// returns the correct home directory for the environment where the session runs.
func expandTilde(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// workDirForShell returns a shell-safe directory expression for use in cd commands.
// Converts ~/path to $HOME/path so the shell handles expansion (tmux's -c doesn't expand ~).
func workDirForShell(dir string) string {
	if dir == "~" {
		return "$HOME"
	}
	if strings.HasPrefix(dir, "~/") {
		return "$HOME/" + dir[2:]
	}
	return dir
}

// quickResumeFailWindow bounds "how long after startup does a pane death
// still count as a resume failure worth retrying with a fresh session id".
// Set to 10s: shorter would miss slow-machine resumes, longer would treat a
// deliberate quick exit as a resume failure.
const quickResumeFailWindow = 10 * time.Second

// spawnSnapshot is a value-typed snapshot of the session fields
// buildAgentShellCmd needs. Callers copy the fields they care about while
// holding m.mu, then release the lock before calling the builder — this
// makes buildAgentShellCmd safe to run concurrently with HandleHookEvent /
// List / Get, which mutate the source session under lock.
type spawnSnapshot struct {
	JinSessionID        string
	AgentKind           string
	AgentSessionID      string
	AgentSessionStarted bool
	StartDir            string // pre-tmux shell workdir (may be ~-prefixed)
	ExpandedWorkDir     string // absolute, ~-expanded workdir handed to Setup()
}

// snapshotForSpawn takes the fields buildAgentShellCmd depends on. Callers
// must hold m.mu — the read must be atomic with respect to the field
// writes the daemon performs elsewhere.
func snapshotForSpawn(session *Session, startDir, expandedWorkDir string) spawnSnapshot {
	return spawnSnapshot{
		JinSessionID:        session.ID,
		AgentKind:           session.AgentKind,
		AgentSessionID:      session.AgentSessionID,
		AgentSessionStarted: session.AgentSessionStarted,
		StartDir:            startDir,
		ExpandedWorkDir:     expandedWorkDir,
	}
}

// buildAgentShellCmd wraps the adapter's SpawnPlan in the fixed shell
// template Manager uses everywhere it spawns an agent (start and quick-fail
// retry). Centralising the assembly keeps the two call sites in lock-step
// on env vars, shell escaping, and the Setup() invariant.
//
// Pure builder: reads only the immutable snapshot; performs NO Session
// writes. Callers own the "started once" invariant
// (session.AgentSessionStarted = true) and must set it inside their own
// lock context. Callers ALSO own the read side: buildAgentShellCmd takes a
// value-typed snapshot precisely so the retry path in captureOutputTmux
// can call it after m.mu.Unlock() without racing HandleHookEvent's writes
// to session.WorkDir / AgentSessionID / etc.
func (m *Manager) buildAgentShellCmd(snap spawnSnapshot) (string, error) {
	if m.agentResolver == nil {
		return "", fmt.Errorf("agent resolver not configured")
	}
	ag, err := m.agentResolver.Resolve(snap.AgentKind)
	if err != nil {
		return "", fmt.Errorf("resolve agent %q: %w", snap.AgentKind, err)
	}

	execPath, execErr := os.Executable()
	if execErr != nil {
		debugLog("[AGENT] Warning: failed to get executable path: %v", execErr)
	}
	if err := ag.Setup(SetupContext{
		StateDir: m.stateDir,
		ExecPath: execPath,
		WorkDir:  snap.ExpandedWorkDir,
	}); err != nil {
		debugLog("[AGENT] Setup returned error: %v", err)
	}

	plan := ag.SpawnCommand(SpawnOptions{
		JinSessionID:        snap.JinSessionID,
		AgentSessionID:      snap.AgentSessionID,
		AgentSessionStarted: snap.AgentSessionStarted,
		WorkDir:             snap.ExpandedWorkDir,
		CustomEnv:           m.configMgr.GetEnv(),
	})

	shellDir := workDirForShell(snap.StartDir)
	customEnv := buildEnvString(m.configMgr.GetEnv())
	envVars := fmt.Sprintf("JIN_SESSION_ID=%s TERM=xterm-256color COLORTERM=truecolor FORCE_COLOR=1", snap.JinSessionID)
	if customEnv != "" {
		envVars += " " + customEnv
	}
	for k, v := range plan.ExtraEnv {
		// Keys go through the same env-name validation as UnsetEnv; the
		// value is single-quoted so any adapter output survives the outer
		// -ic 'cmd' wrapping.
		if !validEnvKeyPattern.MatchString(k) {
			return "", fmt.Errorf("agent %q returned invalid ExtraEnv key %q", snap.AgentKind, k)
		}
		envVars += fmt.Sprintf(" %s=%s", k, shellEscape(v))
	}
	unsetFlags := " -u TMUX -u TMUX_PANE"
	for _, k := range plan.UnsetEnv {
		if !validEnvKeyPattern.MatchString(k) {
			return "", fmt.Errorf("agent %q returned invalid UnsetEnv name %q", snap.AgentKind, k)
		}
		unsetFlags += " -u " + k
	}
	// plan.Command is spliced verbatim into `-ic '...'`. Per the SpawnPlan
	// doc comment, adapters emit the raw command and Manager defensively
	// escapes any single quote that slipped through — so a malformed
	// adapter can't break out of the wrapper into the parent shell.
	safeCmd := strings.ReplaceAll(plan.Command, "'", `'\''`)
	shellCmd := fmt.Sprintf("cd \"%s\" 2>/dev/null; env%s %s %s -ic '%s'",
		shellDir, unsetFlags, envVars, m.configMgr.GetShell(), safeCmd)
	return shellCmd, nil
}

// startSessionTmux starts a session in a tmux window.
func (m *Manager) startSessionTmux(session *Session) error {
	// Resume in the last known cwd (e.g. worktree) when available, so the
	// session lands in the same directory it was in when it stopped. If the
	// session never moved out of WorkDir, CurrentWorkDir is empty and WorkDir
	// is used instead. We do NOT silently fall back from a missing
	// CurrentWorkDir to WorkDir: a session that was bound to a worktree
	// cannot be meaningfully resumed at the project root once the worktree
	// is gone — fail loudly so the user can delete or recreate the session.
	startDir := session.WorkDir
	if session.CurrentWorkDir != "" {
		startDir = session.CurrentWorkDir
	}

	// Expand ~ for tmux -c flag and trust state check
	expandedWorkDir := expandTilde(startDir)

	// Error if start directory does not exist (can happen after worktree deletion etc.)
	if info, err := os.Stat(expandedWorkDir); err != nil || !info.IsDir() {
		return fmt.Errorf("work directory does not exist: %s (may have been deleted, e.g. git worktree removed)", startDir)
	}

	// Snapshot the session fields buildAgentShellCmd needs. Reading here is
	// safe: startSessionTmux runs under StartBackground's m.mu.Lock() (see
	// callchain StartBackground → startSession → startSessionTmux) so no
	// other goroutine can mutate the session under us.
	shellCmd, err := m.buildAgentShellCmd(snapshotForSpawn(session, startDir, expandedWorkDir))
	if err != nil {
		return err
	}

	// Commit the "started once" invariant: from this point a subsequent
	// resume must take the --resume branch even if SessionStart never fires
	// (crashes on start, no hook binary, etc.). Same lock context as the
	// snapshot above.
	session.AgentSessionStarted = true

	innerSessionName := tmux.InnerSessionName(session.ID)

	// Try to revive CC in existing inner tmux session (preserves user panes)
	if session.TmuxWindowName != "" && m.tmuxClient.HasSession(session.TmuxWindowName) {
		target := session.TmuxPaneID
		if err := m.tmuxClient.RespawnPane(target, shellCmd); err == nil {
			session.Status = StatusRunning
			session.LastOutputTime = time.Now()
			session.StartedAt = time.Now()
			_ = m.store.Save(session)
			debugLog("[TMUX] Session %s CC revived via RespawnPane in inner session", session.Description)
			go m.captureOutputTmux(session)
			return nil
		}
		// Fall through: session gone or respawn failed → create new
		session.TmuxWindowName = ""
	}

	// Kill existing inner session with the same name if it exists (stale from daemon restart)
	_ = m.tmuxClient.KillSession(innerSessionName) // ignore error (session might not exist)

	// Create a new inner tmux session (-L jin) for this CC session
	if err := m.tmuxClient.NewSessionWithCmdInDir(innerSessionName, 200, 50, expandedWorkDir, shellCmd); err != nil {
		return fmt.Errorf("failed to create inner tmux session: %w", err)
	}

	// Get the pane's unique ID (%N) — reliable regardless of base-index/pane-base-index.
	// User's ~/.tmux.conf may set base-index=1, making ":0.0" targets invalid.
	paneID, err := m.tmuxClient.GetPaneID(innerSessionName)
	if err != nil {
		debugLog("[TMUX] GetPaneID failed for %s: %v", innerSessionName, err)
		paneID = ""
	}

	// Tag CC pane FIRST — must happen before pane-died hook is active,
	// otherwise a quick process exit triggers kill-pane on the untagged pane.
	if paneID != "" {
		_ = m.tmuxClient.TagManagedPane(paneID)
	}

	// Then apply server config (remain-on-exit + pane-died hook)
	m.configureInnerTmux()

	session.TmuxPaneID = paneID

	session.TmuxWindowName = innerSessionName // Reuse field for inner session name
	session.Status = StatusRunning
	session.LastOutputTime = time.Now()
	session.StartedAt = time.Now()

	// Persist inner session name
	_ = m.store.Save(session)

	// Start status detection via capture-pane polling
	go m.captureOutputTmux(session)

	return nil
}

// updateGitBranch checks the git branch for the given path and updates session fields.
// It runs git rev-parse (lightweight, <5ms) and acquires the lock internally.
// lastTrackedPath is used to avoid clearing git info on every poll when already in a non-git dir.
func (m *Manager) updateGitBranch(session *Session, currentPath, lastTrackedPath string) {
	cmd := exec.Command("git", "-C", currentPath, "rev-parse", "--abbrev-ref", "HEAD")
	if output, err := cmd.Output(); err == nil {
		branch := strings.TrimSpace(string(output))
		// Detect if currentPath is a git worktree (.git is a file, not a directory)
		isWorktree := false
		gitPath := filepath.Join(currentPath, ".git")
		if fi, err := os.Lstat(gitPath); err == nil {
			isWorktree = fi.Mode().IsRegular()
		}
		m.mu.Lock()
		session.CurrentBranch = branch
		session.IsGitRepo = true
		session.IsWorktree = isWorktree
		m.mu.Unlock()
	} else if currentPath != lastTrackedPath {
		// Only clear git info when entering a non-git directory
		m.mu.Lock()
		session.CurrentBranch = ""
		session.IsGitRepo = false
		session.IsWorktree = false
		m.mu.Unlock()
	}
}

// captureOutputTmux polls tmux for process death detection and CWD/branch tracking.
// Status detection is handled by Claude Code hooks (see HandleHookEvent).
func (m *Manager) captureOutputTmux(session *Session) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Use pane ID (%N) when available (stable across join-pane reordering),
	// else the window.pane index. paneTargetLocked only errors when both are
	// unset, which a monitored session shouldn't hit; fall back to the bare
	// window target to preserve the pre-refactor poll behavior.
	target, err := paneTargetLocked(session)
	if err != nil {
		target = tmux.WindowTarget(session.TmuxWindowName, 0)
	}
	lastTrackedPath := ""

	for range ticker.C {
		m.mu.RLock()
		_, exists := m.sessions[session.ID]
		if !exists || session.Status == StatusStopped {
			m.mu.RUnlock()
			return
		}
		sessionName := session.Description
		m.mu.RUnlock()

		// Check if pane process has exited
		if m.tmuxClient.IsPaneDead(target) {
			m.mu.Lock()
			// Exit without saving if session was already deleted
			if _, exists := m.sessions[session.ID]; !exists {
				m.mu.Unlock()
				debugLog("[TMUX] Session %s pane died but session already deleted, skipping save", sessionName)
				return
			}

			// If the agent's --resume fails immediately (within 10 seconds of
			// startup), auto-restart with a fresh session ID by going back
			// through the adapter's SpawnCommand (this way agents without a
			// --resume concept still get sensible retry semantics).
			if session.AgentSessionStarted && time.Since(session.StartedAt) < quickResumeFailWindow {
				debugLog("[TMUX] Session %s pane died quickly (resume likely failed), retrying with fresh agent session", session.Description)
				newSessionID := uuid.New().String()
				session.AgentSessionStarted = false
				session.AgentSessionID = newSessionID
				// Snapshot every field buildAgentShellCmd needs BEFORE
				// releasing m.mu. Without this the retry runs the builder
				// with lock-free reads of session.WorkDir /
				// AgentSessionID / AgentSessionStarted, racing writes from
				// HandleHookEvent.
				retrySnap := snapshotForSpawn(session, session.WorkDir, expandTilde(session.WorkDir))
				m.mu.Unlock()
				_ = m.store.Save(session)

				shellCmd, buildErr := m.buildAgentShellCmd(retrySnap)
				if buildErr != nil {
					debugLog("[TMUX] Session %s: cannot build retry cmd: %v", session.Description, buildErr)
					m.mu.Lock()
					if _, exists := m.sessions[session.ID]; !exists {
						m.mu.Unlock()
						return
					}
					session.Status = StatusStopped
					session.LastActiveAt = time.Now()
					m.mu.Unlock()
					_ = m.store.Save(session)
					return
				}
				if err := m.tmuxClient.RespawnPane(target, shellCmd); err == nil {
					m.mu.Lock()
					session.Status = StatusRunning
					session.AgentSessionStarted = true
					session.StartedAt = time.Now()
					session.LastOutputTime = time.Now()
					m.mu.Unlock()
					_ = m.store.Save(session)
					debugLog("[TMUX] Session %s restarted with fresh agent session (id: %s)", session.Description, newSessionID)
					continue
				}
				debugLog("[TMUX] Session %s respawn failed after quick death", session.Description)
				m.mu.Lock()
				if _, exists := m.sessions[session.ID]; !exists {
					m.mu.Unlock()
					return
				}
			}

			session.Status = StatusStopped
			session.LastActiveAt = time.Now()
			// Keep TmuxWindowName: window survives (remain-on-exit), only CC pane is dead.
			// RespawnPane can revive CC while preserving user panes in the same window.
			m.mu.Unlock()
			_ = m.store.Save(session)
			debugLog("[TMUX] Session %s pane died, marked as stopped (window preserved)", sessionName)
			return
		}

		// Track current working directory and git branch
		if currentPath, err := m.tmuxClient.GetPaneCurrentPath(target); err == nil {
			currentPath = strings.TrimSpace(currentPath)
			if currentPath != "" {
				m.mu.Lock()
				session.CurrentWorkDir = currentPath
				// Only update persisted WorkDir when the new path is a git root
				// (project root or worktree root). This prevents WorkDir from
				// drifting to subdirectories like .claude/workdir/.
				workDirChanged := false
				if session.WorkDir != currentPath && git.IsGitRoot(currentPath) && !git.IsClaudeWorktreePath(currentPath) {
					session.WorkDir = currentPath
					workDirChanged = true
				}
				m.mu.Unlock()
				if workDirChanged {
					_ = m.store.Save(session)
					debugLog("[CWD] Session %s WorkDir updated to %s", sessionName, currentPath)
				}

				m.updateGitBranch(session, currentPath, lastTrackedPath)
				lastTrackedPath = currentPath
			}
		}

		// Fallback: if the session has been in "running" since a fresh start and no
		// hook has arrived within hookIdleTimeout, assume Claude is idle and waiting
		// for input. This handles the case where Claude Code does not fire Stop or
		// idle_prompt during initial startup.
		//
		// StartedAt is json:"-" (runtime-only) so it is always zero after a daemon
		// restart. The !startedAt.IsZero() guard ensures this fallback never fires
		// for daemon-recovered sessions (preventing false idle transitions while a
		// task is still running).
		m.mu.RLock()
		fbStatus := session.Status
		fbLastOutput := session.LastOutputTime
		fbStartedAt := session.StartedAt
		m.mu.RUnlock()

		const hookIdleTimeout = 30 * time.Second
		if fbStatus == StatusRunning && !fbStartedAt.IsZero() && time.Since(fbLastOutput) > hookIdleTimeout {
			m.mu.Lock()
			if _, exists := m.sessions[session.ID]; exists && session.Status == StatusRunning {
				session.Status = StatusIdle
				session.LastOutputTime = time.Now()
				debugLog("[POLL] Session %s: running -> idle (no hook received for %s, fallback)", session.Description, hookIdleTimeout)
			}
			m.mu.Unlock()
			_ = m.store.Save(session)
		}
	}
}

// FindByAgentSessionID finds a session by its adapter-side session ID
// (Claude Code --session-id UUID, Codex conversation id, ...).
func (m *Manager) FindByAgentSessionID(agentSessionID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.AgentSessionID == agentSessionID {
			return s, true
		}
	}
	return nil, false
}

// HandleHookEvent processes an incoming agent hook event and updates the
// session state. The event vocabulary itself (which event name means what
// status) is owned by the adapter — this function is agent-agnostic wiring:
//
//  1. resolve the session (jin id preferred, adapter-side id as fallback)
//  2. update the adapter session id if the adapter has re-keyed it
//  3. run agent-agnostic side effects (CWD tracking, AgentSessionStarted)
//  4. hand the raw event to the adapter's StatusSource for interpretation
//  5. dispatch notifications the adapter requested
//  6. trigger a Layer C description upgrade on prompt/stop events
func (m *Manager) HandleHookEvent(agentSessionID, jinSessionID, eventName, notificationType, cwd, stopReason string) {
	var session *Session
	var ok bool

	// Try jin session ID first (from JIN_SESSION_ID env var, most reliable)
	if jinSessionID != "" {
		m.mu.RLock()
		session, ok = m.sessions[jinSessionID]
		m.mu.RUnlock()
	}

	// Fall back to adapter-side session ID
	if !ok {
		session, ok = m.FindByAgentSessionID(agentSessionID)
	}

	if !ok {
		debugLog("[HOOK] Unknown session: jin=%s agent=%s", jinSessionID, agentSessionID)
		return
	}

	m.mu.RLock()
	kind := session.AgentKind
	m.mu.RUnlock()
	if m.agentResolver == nil {
		debugLog("[HOOK] Session %s: no agent resolver configured", session.Description)
		return
	}
	ag, err := m.agentResolver.Resolve(kind)
	if err != nil {
		debugLog("[HOOK] Session %s: cannot resolve agent %q: %v", session.Description, kind, err)
		return
	}

	upd, updOK := ag.StatusSource().Interpret(StatusSignal{
		Kind: "hook",
		Payload: map[string]string{
			"event":             eventName,
			"notification_type": notificationType,
			"stop_reason":       stopReason,
			"cwd":               cwd,
		},
	})

	// git.IsGitRoot stats the filesystem, so settle it before taking the lock.
	// cwd comes from the hook payload, so this is a pure function of the event.
	cwdIsGitRoot := cwd != "" && git.IsGitRoot(cwd) && !git.IsClaudeWorktreePath(cwd)

	m.mu.Lock()
	oldStatus := session.Status
	sessionID := session.ID
	sessionName := session.Description

	// Update AgentSessionID if it changed (adapter may re-key it, e.g. CC
	// assigns its own UUID when we started with an empty one).
	if agentSessionID != "" && session.AgentSessionID != agentSessionID {
		debugLog("[HOOK] Updating AgentSessionID for %s: %s -> %s", sessionName, session.AgentSessionID, agentSessionID)
		session.AgentSessionID = agentSessionID
	}

	// Update CWD from the agent's actual working directory.
	// Only update persisted WorkDir when the new path is a git root
	// (project root or worktree root) to prevent drift to subdirectories.
	cwdChanged := false
	if cwd != "" {
		session.CurrentWorkDir = cwd
		if session.WorkDir != cwd && cwdIsGitRoot {
			session.WorkDir = cwd
			cwdChanged = true
		}
	}

	// SessionStart bookkeeping is agent-agnostic: any "first hook" observed
	// after spawn confirms the agent came up successfully. Adapters that
	// don't emit an explicit SessionStart event won't hit this branch, but
	// startSessionTmux already flips the flag defensively before the spawn.
	sessionStarted := false
	if eventName == "SessionStart" && !session.AgentSessionStarted {
		session.AgentSessionStarted = true
		sessionStarted = true
	}

	// SessionEnd on an already-stopped session: no verdict fields should be
	// applied (they would mutate LastOutputTime / LastActiveAt in memory but
	// only persist on cwdChanged, which drops the change on daemon restart).
	// Take the early return before assigning anything from upd — mirrors the
	// pre-refactor SessionEnd branch that also short-circuited here.
	if updOK && upd.Status == StatusStopped && oldStatus == StatusStopped {
		m.mu.Unlock()
		if cwdChanged {
			_ = m.store.Save(session)
			debugLog("[HOOK] Session %s: CWD updated to %s (SessionEnd, already stopped)", sessionName, cwd)
		}
		return
	}

	// Fold in the adapter's status verdict. A missing verdict (updOK=false)
	// still lets us persist CWD / SessionStart changes, but leaves Status
	// alone. ErrorMessage uses the tri-state documented on StatusUpdate:
	// non-empty means set, ClearError means clear, both zero means leave.
	if updOK {
		session.Status = upd.Status
		if upd.ErrorMessage != "" {
			session.ErrorMessage = upd.ErrorMessage
		} else if upd.ClearError {
			session.ErrorMessage = ""
		}
		session.LastOutputTime = time.Now()
		if upd.Status == StatusStopped {
			session.LastActiveAt = time.Now()
		}
	} else if eventName == "CwdChanged" || eventName == "SessionStart" {
		// Non-status events that we still track internally: keep
		// LastOutputTime moving so the "no hook for 30s" fallback in
		// captureOutputTmux doesn't fire.
		session.LastOutputTime = time.Now()
	}

	// Snapshot everything the post-unlock code needs. Reading session.* after
	// Unlock would race with concurrent mutators, so the plugin event is built
	// from these copies only.
	newStatus := session.Status
	workDir := session.WorkDir
	tmuxPaneID := session.TmuxPaneID
	pluginDisp := m.pluginDisp
	m.mu.Unlock()

	// CwdChanged: immediately check git branch outside the lock
	if eventName == "CwdChanged" && cwd != "" {
		m.updateGitBranch(session, cwd, "")
	}

	// Persist status/CWD/session-started changes
	if oldStatus != newStatus || cwdChanged || sessionStarted {
		_ = m.store.Save(session)
		if oldStatus != newStatus {
			debugLog("[HOOK] Session %s: %s -> %s (hook: %s)", sessionName, oldStatus, newStatus, eventName)
		}
		if cwdChanged {
			debugLog("[HOOK] Session %s: CWD updated to %s", sessionName, cwd)
		}
	}

	if pluginDisp != nil && updOK && oldStatus != newStatus {
		pluginDisp.Publish(plugin.Event{
			Name:       manifest.EventStatusChanged,
			SessionID:  sessionID,
			Status:     string(newStatus),
			PrevStatus: string(oldStatus),
			AgentKind:  kind,
			WorkDir:    workDir,
			TmuxPaneID: tmuxPaneID,
			NotifyKind: string(upd.Notify),
		})
	}

	// Layer C: opportunistically upgrade the description. Runs on three events
	// that each expose a different signal source:
	//
	//   - SessionStart is the earliest hook; the transcript is still empty but
	//     the agent may already have written a session-name file (Claude Code
	//     2.x populates ~/.claude/sessions/<PID>.json by then). The enhancer
	//     returns LayerAgentName here.
	//   - UserPromptSubmit races Claude Code's transcript flush by ~10ms, so
	//     it sometimes still sees an empty jsonl but is our fastest chance at
	//     a LayerTranscript win.
	//   - Stop fires after the assistant response completes, by which point
	//     the transcript is guaranteed to be flushed. It is the reliable
	//     upgrade path to LayerTranscript.
	//
	// TryUpgradeDescription self-limits via the monotonic-layer guard, so
	// calling it on all three events at most produces one write per layer per
	// session. Agents that can't produce a description (Description() == nil)
	// simply skip the upgrade.
	if eventName == "SessionStart" || eventName == "UserPromptSubmit" || eventName == "Stop" {
		if enh := ag.Description(); enh != nil {
			m.TryUpgradeDescription(sessionID, enh)
		}
	}
}

// HandleAgentSignal is the agent-agnostic entry point for status signals.
// Currently only kind="hook" is fully wired: it forwards to HandleHookEvent
// so the existing Claude Code hook route works verbatim over the new IPC
// action. Other kinds are logged and dropped — future adapters (pane-tail,
// log-tail) can add cases here without touching the daemon transport layer.
func (m *Manager) HandleAgentSignal(jinSessionID, kind string, payload map[string]string) {
	switch kind {
	case "hook":
		m.HandleHookEvent(
			payload["agent_session_id"],
			jinSessionID,
			payload["event"],
			payload["notification_type"],
			payload["cwd"],
			payload["stop_reason"],
		)
	default:
		debugLog("[SIGNAL] Session %s: unsupported signal kind %q", jinSessionID, kind)
	}
}

// Kill terminates a session
func (m *Manager) Kill(id string) error {
	m.mu.Lock()

	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}

	// Kill CC pane in the inner tmux session
	if m.tmuxClient != nil && session.TmuxPaneID != "" {
		_ = m.tmuxClient.KillPane(session.TmuxPaneID)
		session.TmuxPaneID = ""
		session.TmuxWindowName = ""
	} else if m.tmuxClient != nil && session.TmuxWindowName != "" {
		// Fallback: no pane ID, kill the inner tmux session
		_ = m.tmuxClient.KillSession(session.TmuxWindowName)
		session.TmuxWindowName = ""
	}

	session.Status = StatusStopped
	// Update LastActiveAt for persistence
	if !session.LastOutputTime.IsZero() {
		session.LastActiveAt = session.LastOutputTime
	} else {
		session.LastActiveAt = time.Now()
	}

	m.mu.Unlock()
	// Persist LastActiveAt
	_ = m.store.Save(session)

	return nil
}

// Delete removes a session completely.
// If removeWorktree is true and the session's WorkDir is a git worktree,
// the worktree will also be removed. If the worktree has uncommitted changes
// and forceRemoveWorktree is false, ErrWorktreeDirty is returned.
func (m *Manager) Delete(id string, removeWorktree, forceRemoveWorktree bool) error {
	// Defense-in-depth: the CLI validates the same combination, but non-CLI
	// callers (TUI, integration tests, future clients) reach Manager directly.
	if forceRemoveWorktree && !removeWorktree {
		return fmt.Errorf("forceRemoveWorktree requires removeWorktree")
	}

	m.mu.Lock()

	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}

	currentWorkDir := session.CurrentWorkDir
	persistedWorkDir := session.WorkDir
	m.mu.Unlock()

	// Resolve the actual worktree path outside the lock — ResolveWorktreeDir
	// performs os.Lstat probes which would otherwise block other goroutines.
	workDir := git.ResolveWorktreeDir(currentWorkDir, persistedWorkDir)

	// Remove worktree if requested (outside lock to avoid blocking during exec).
	// This runs before tmux kill so that ErrWorktreeDirty / ErrNotWorktree can
	// abort without side effects.
	if removeWorktree && workDir != "" {
		if err := m.removeGitWorktree(workDir, forceRemoveWorktree); err != nil {
			if errors.Is(err, ErrWorktreeDirty) || errors.Is(err, ErrNotWorktree) {
				return err
			}
			debugLog("[DELETE] worktree removal failed for %s: %v", workDir, err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Kill the inner tmux session entirely
	if m.tmuxClient != nil && session.TmuxWindowName != "" {
		_ = m.tmuxClient.KillSession(session.TmuxWindowName)
	}

	// Remove from store
	if err := m.store.Delete(id); err != nil {
		return err
	}

	delete(m.sessions, id)
	return nil
}

// removeGitWorktree removes a git worktree at the given path.
// Returns ErrWorktreeDirty if the worktree has uncommitted changes and force
// is false. Returns ErrNotWorktree if workDir is not a git worktree.
func (m *Manager) removeGitWorktree(workDir string, force bool) error {
	err := m.gitClient.RemoveWorktree(workDir, force)
	switch {
	case errors.Is(err, git.ErrDirty):
		return ErrWorktreeDirty
	case errors.Is(err, git.ErrNotWorktree):
		return ErrNotWorktree
	default:
		return err
	}
}

// Claude Code-specific setup helpers (hooks-settings.json generation, trust
// dialog suppression) live under internal/agent/claude/. The adapter's
// Setup() is invoked from startSessionTmux, so no CC-specific code remains
// in this file.
