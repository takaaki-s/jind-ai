package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/takaaki-s/honjin/internal/config"
	"github.com/takaaki-s/honjin/internal/debug"
	"github.com/takaaki-s/honjin/internal/git"
	"github.com/takaaki-s/honjin/internal/notify"
	"github.com/takaaki-s/honjin/internal/tmux"
	"github.com/takaaki-s/honjin/internal/transcript"
	"github.com/takaaki-s/honjin/internal/worktreehook"
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

// Manager manages multiple Claude Code sessions
type Manager struct {
	sessions   map[string]*Session
	store      *Store
	notifier   *notify.Notifier
	configMgr  *config.Manager
	tmuxClient tmux.Runner // tmux client for session management
	hookRunner worktreehook.Runner
	gitClient  *git.Client
	mu         sync.RWMutex
	stateDir   string
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
		if session.TmuxWindowName == "" {
			// Fix stale sessions: active status but no tmux session (from prior recovery bug)
			if session.Status != StatusStopped && session.Status != StatusCreating {
				session.Status = StatusStopped
				_ = m.store.Save(session)
				debugLog("[RECOVER] Session %s has active status but no tmux session, marked stopped", session.Name)
			}
			continue
		}

		// Check if the inner tmux session still exists
		if !m.tmuxClient.HasSession(session.TmuxWindowName) {
			session.TmuxWindowName = ""
			session.Status = StatusStopped
			_ = m.store.Save(session)
			debugLog("[RECOVER] Session %s inner tmux session gone, marked stopped", session.Name)
			continue
		}

		target := session.TmuxPaneID

		// Check if pane is dead — keep TmuxWindowName (session alive via remain-on-exit)
		if m.tmuxClient.IsPaneDead(target) {
			session.Status = StatusStopped
			_ = m.store.Save(session)
			debugLog("[RECOVER] Session %s tmux pane dead, kept TmuxWindowName (session preserved)", session.Name)
			continue
		}

		// Session exists and pane is alive - resume monitoring
		session.Status = StatusRunning
		session.LastOutputTime = time.Now()
		_ = m.store.Save(session)
		debugLog("[RECOVER] Session %s has live inner tmux session, resuming monitoring", session.Name)

		go m.captureOutputTmux(session)
	}
}

// ensureTmuxClient lazily initializes the inner tmux client (-L jin).
// Each CC session creates its own tmux session, so no shared session is needed.
func (m *Manager) ensureTmuxClient() {
	if m.tmuxClient != nil {
		return
	}
	tc, err := tmux.NewClient() // Uses SocketName = "jin"
	if err != nil {
		return
	}
	m.tmuxClient = tc
	debugLog("[TMUX] Inner tmux client initialized (socket: %s)", tmux.SocketName)
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
		notifier:  notify.NewNotifier(),
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
		s.Status = StatusStopped // All loaded sessions start as stopped
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
	WorkDir string // Working directory path
	Name    string // Session name (defaults to directory basename)
	Fleet   string // Fleet name for session grouping; defaults to DefaultFleet if empty

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
// sessions map is re-checked under lock after worktree creation. Fetch is
// slow (network) so holding m.mu through it would block reads (List, Get,
// SetStatus) across the whole daemon.
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

	// Captured BEFORE the worktree block overwrites opts.WorkDir, so the
	// default session name reflects the ORIGINAL repository basename rather
	// than the auto-generated worktree directory name (e.g. "jin-abcd1234").
	defaultName := filepath.Base(opts.WorkDir)

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

		if err := m.gitClient.Fetch(opts.WorkDir, "origin", base); err != nil {
			if cfg.FetchFailure == config.FetchFailureStrict {
				return nil, "", err
			}
			debugLog("[WORKTREE] fetch failed, continuing with local origin/%s: %v", base, err)
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
						SessionName:  opts.Name,
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
			return nil, "", fmt.Errorf("session already exists for directory: %s (session: %s)", opts.WorkDir, s.Name)
		}
	}

	id := sessionID

	// Determine session name (default: original repository basename)
	name := opts.Name
	if name == "" {
		name = defaultName
	}

	// Check session name uniqueness
	for _, s := range m.sessions {
		if s.Name == name {
			return nil, "", fmt.Errorf("session with name '%s' already exists. Use --name to specify a different name", name)
		}
	}

	// Generate Claude session ID for session persistence
	claudeSessionID := uuid.New().String()

	session := &Session{
		ID:              id,
		Name:            name,
		WorkDir:         opts.WorkDir,
		CreatedAt:       time.Now(),
		Status:          StatusStopped,
		ClaudeSessionID: claudeSessionID,
		Fleet:           opts.Fleet,
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
		if info.ClaudeSessionID != "" && info.WorkDir != "" {
			if msgs, err := reader.GetLastMessages(info.WorkDir, info.ClaudeSessionID); err == nil && msgs != nil {
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

// SendPrompt sends a prompt to a session's tmux pane.
// The session must be in idle status.
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

	if err := m.tmuxClient.SendKeysLiteral(paneID, prompt); err != nil {
		return fmt.Errorf("failed to send prompt: %w", err)
	}
	if err := m.tmuxClient.SendKeys(paneID, "Enter"); err != nil {
		return fmt.Errorf("failed to send Enter: %w", err)
	}
	return nil
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
				return fmt.Errorf("WorkDir already in use by session %s", s.Name)
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

	// Set trust state
	if err := ensureClaudeTrustState(expandedWorkDir); err != nil {
		debugLog("[TRUST] Warning: failed to set trust state: %v", err)
	}

	// Build Claude command
	shell := m.configMgr.GetShell()

	// Generate hooks settings file so Claude Code hooks are auto-configured
	claudeCmd := "claude"
	execPath, err := os.Executable()
	if err == nil {
		if hooksPath, err := ensureHooksSettingsFile(m.stateDir, execPath); err == nil {
			claudeCmd = fmt.Sprintf("claude --settings %s", hooksPath)
		} else {
			debugLog("[HOOKS] Warning: failed to generate hooks settings: %v", err)
		}
	} else {
		debugLog("[HOOKS] Warning: failed to get executable path: %v", err)
	}

	if session.ClaudeSessionID != "" {
		if session.ClaudeSessionStarted {
			claudeCmd += fmt.Sprintf(" --resume %s", session.ClaudeSessionID)
			debugLog("[SESSION] Resuming Claude session: %s", session.ClaudeSessionID)
		} else {
			claudeCmd += fmt.Sprintf(" --session-id %s", session.ClaudeSessionID)
			debugLog("[SESSION] Starting new Claude session with ID: %s", session.ClaudeSessionID)
			session.ClaudeSessionStarted = true
		}
	}

	// Build shell command with environment setup
	// Unset TMUX/TMUX_PANE to prevent nested tmux detection
	// Embed cd to WorkDir so the shell expands ~ and $HOME
	// (tmux's -c flag doesn't expand ~, and RespawnPane doesn't accept -c at all)
	// Use ; instead of && so cd failure doesn't prevent claude from starting
	shellDir := workDirForShell(startDir)
	customEnv := buildEnvString(m.configMgr.GetEnv())
	envVars := fmt.Sprintf("JIN_SESSION_ID=%s TERM=xterm-256color COLORTERM=truecolor FORCE_COLOR=1", session.ID)
	if customEnv != "" {
		envVars += " " + customEnv
	}
	shellCmd := fmt.Sprintf("cd \"%s\" 2>/dev/null; env -u TMUX -u TMUX_PANE -u CLAUDECODE %s %s -ic '%s'",
		shellDir, envVars, shell, claudeCmd)

	innerSessionName := tmux.InnerSessionName(session.ID)

	// Try to revive CC in existing inner tmux session (preserves user panes)
	if session.TmuxWindowName != "" && m.tmuxClient.HasSession(session.TmuxWindowName) {
		target := session.TmuxPaneID
		if err := m.tmuxClient.RespawnPane(target, shellCmd); err == nil {
			session.Status = StatusRunning
			session.LastOutputTime = time.Now()
			session.StartedAt = time.Now()
			_ = m.store.Save(session)
			debugLog("[TMUX] Session %s CC revived via RespawnPane in inner session", session.Name)
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

	// Use pane ID (%N) if available; falls back to window.pane index.
	// Pane IDs are stable even when join-pane reorders pane indices.
	target := session.TmuxPaneID
	if target == "" {
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
		sessionName := session.Name
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

			// If claude --resume fails immediately (within 10 seconds of startup),
			// auto-restart with a fresh session ID using plain claude
			if session.ClaudeSessionStarted && time.Since(session.StartedAt) < 10*time.Second {
				debugLog("[TMUX] Session %s pane died quickly (resume likely failed), retrying with fresh claude", session.Name)
				newSessionID := uuid.New().String()
				session.ClaudeSessionStarted = false
				session.ClaudeSessionID = newSessionID
				m.mu.Unlock()
				_ = m.store.Save(session)

				shell := m.configMgr.GetShell()
				shellDir := workDirForShell(session.WorkDir)
				customEnv := buildEnvString(m.configMgr.GetEnv())
				envVars := fmt.Sprintf("JIN_SESSION_ID=%s TERM=xterm-256color COLORTERM=truecolor FORCE_COLOR=1", session.ID)
				if customEnv != "" {
					envVars += " " + customEnv
				}
				shellCmd := fmt.Sprintf("cd \"%s\" 2>/dev/null; env -u TMUX -u TMUX_PANE -u CLAUDECODE %s %s -ic 'claude --session-id %s'",
					shellDir, envVars, shell, newSessionID)
				if err := m.tmuxClient.RespawnPane(target, shellCmd); err == nil {
					m.mu.Lock()
					session.Status = StatusRunning
					session.ClaudeSessionStarted = true
					session.StartedAt = time.Now()
					session.LastOutputTime = time.Now()
					m.mu.Unlock()
					_ = m.store.Save(session)
					debugLog("[TMUX] Session %s restarted with fresh claude (session-id: %s)", session.Name, newSessionID)
					continue
				}
				debugLog("[TMUX] Session %s respawn failed after quick death", session.Name)
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
				debugLog("[POLL] Session %s: running -> idle (no hook received for %s, fallback)", session.Name, hookIdleTimeout)
			}
			m.mu.Unlock()
			_ = m.store.Save(session)
		}
	}
}

// FindByClaudeSessionID finds a session by its Claude Code session ID
func (m *Manager) FindByClaudeSessionID(ccSessionID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.ClaudeSessionID == ccSessionID {
			return s, true
		}
	}
	return nil, false
}

// HandleHookEvent processes a Claude Code hook event and updates session status
func (m *Manager) HandleHookEvent(ccSessionID, jinSessionID, eventName, notificationType, cwd, stopReason string) {
	var session *Session
	var ok bool

	// Try jin session ID first (from JIN_SESSION_ID env var, most reliable)
	if jinSessionID != "" {
		m.mu.RLock()
		session, ok = m.sessions[jinSessionID]
		m.mu.RUnlock()
	}

	// Fall back to Claude Code session ID
	if !ok {
		session, ok = m.FindByClaudeSessionID(ccSessionID)
	}

	if !ok {
		debugLog("[HOOK] Unknown session: jin=%s cc=%s", jinSessionID, ccSessionID)
		return
	}

	m.mu.Lock()
	oldStatus := session.Status
	sessionID := session.ID
	sessionName := session.Name

	// Update ClaudeSessionID if it changed (Claude Code may assign its own)
	if ccSessionID != "" && session.ClaudeSessionID != ccSessionID {
		debugLog("[HOOK] Updating ClaudeSessionID for %s: %s -> %s", sessionName, session.ClaudeSessionID, ccSessionID)
		session.ClaudeSessionID = ccSessionID
	}

	// Update CWD from Claude Code's actual working directory.
	// Only update persisted WorkDir when the new path is a git root
	// (project root or worktree root) to prevent drift to subdirectories.
	cwdChanged := false
	if cwd != "" {
		session.CurrentWorkDir = cwd
		if session.WorkDir != cwd && git.IsGitRoot(cwd) && !git.IsClaudeWorktreePath(cwd) {
			session.WorkDir = cwd
			cwdChanged = true
		}
	}

	sessionStarted := false
	switch eventName {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		session.Status = StatusThinking
		session.ErrorMessage = ""
		session.LastOutputTime = time.Now()
	case "Stop":
		session.Status = StatusIdle
		session.ErrorMessage = ""
		session.LastOutputTime = time.Now()
	case "StopFailure":
		session.Status = StatusIdle
		session.ErrorMessage = stopReason
		session.LastOutputTime = time.Now()
	case "CwdChanged":
		// CWD is already updated in the common block above.
		// Just update LastOutputTime; status is unchanged.
		session.LastOutputTime = time.Now()
	case "SessionStart":
		if !session.ClaudeSessionStarted {
			session.ClaudeSessionStarted = true
			sessionStarted = true
		}
		session.LastOutputTime = time.Now()
	case "SessionEnd":
		if session.Status == StatusStopped {
			// Already stopped — save any CWD/session-ID changes, then return
			m.mu.Unlock()
			if cwdChanged {
				_ = m.store.Save(session)
				debugLog("[HOOK] Session %s: CWD updated to %s (SessionEnd, already stopped)", sessionName, cwd)
			}
			return
		}
		session.Status = StatusStopped
		session.LastActiveAt = time.Now()
	case "Notification":
		switch notificationType {
		case "permission_prompt", "elicitation_dialog":
			session.Status = StatusPermission
			session.LastOutputTime = time.Now()
		case "idle_prompt":
			session.Status = StatusIdle
			session.LastOutputTime = time.Now()
		default:
			m.mu.Unlock()
			return
		}
	default:
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// CwdChanged: immediately check git branch outside the lock
	if eventName == "CwdChanged" && cwd != "" {
		m.updateGitBranch(session, cwd, "")
	}

	// Persist status/CWD/session-started changes and send notifications
	if oldStatus != session.Status || cwdChanged || sessionStarted {
		_ = m.store.Save(session)
		if oldStatus != session.Status {
			debugLog("[HOOK] Session %s: %s -> %s (hook: %s)", sessionName, oldStatus, session.Status, eventName)
		}
		if cwdChanged {
			debugLog("[HOOK] Session %s: CWD updated to %s", sessionName, cwd)
		}
	}

	switch eventName {
	case "Stop":
		m.notifier.NotifyTaskComplete(sessionID, sessionName)
	case "StopFailure":
		m.notifier.NotifyError(sessionID, sessionName, stopReason)
	case "Notification":
		if notificationType == "permission_prompt" || notificationType == "elicitation_dialog" {
			m.notifier.NotifyPermission(sessionID, sessionName)
		}
	}
}

// NotificationHistory returns the notification history
func (m *Manager) NotificationHistory() []notify.Entry {
	return m.notifier.NotificationHistory()
}

// NotifyDesktop sends a local desktop notification (used for relaying remote events)
func (m *Manager) NotifyDesktop(title, message string) {
	m.notifier.SendDesktop(title, message)
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

// ClaudeSettings represents the structure of ~/.claude/settings.local.json
type ClaudeSettings struct {
	Projects map[string]ClaudeProjectSettings `json:"projects,omitempty"`
}

// ClaudeProjectSettings represents project-specific settings in Claude
type ClaudeProjectSettings struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted,omitempty"`
}

// ensureClaudeTrustState sets hasTrustDialogAccepted=true in ~/.claude/settings.local.json
// Claude Code checks this setting to skip the trust confirmation dialog
func ensureClaudeTrustState(workDir string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	// Get absolute path of workDir
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".claude", "settings.local.json")

	// Ensure .claude directory exists
	claudeDir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}

	// Read existing settings or create new
	var settings ClaudeSettings
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			// If parsing fails, start fresh but preserve the raw JSON
			settings = ClaudeSettings{}
		}
	}

	// Initialize projects map if nil
	if settings.Projects == nil {
		settings.Projects = make(map[string]ClaudeProjectSettings)
	}

	// Check if already trusted
	if projectSettings, exists := settings.Projects[absWorkDir]; exists && projectSettings.HasTrustDialogAccepted {
		return nil // Already trusted
	}

	// Set trust state for this project
	settings.Projects[absWorkDir] = ClaudeProjectSettings{
		HasTrustDialogAccepted: true,
	}

	// Write back to file
	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings file: %w", err)
	}

	debugLog("[TRUST] Set hasTrustDialogAccepted=true for %s", absWorkDir)
	return nil
}

// hooksEntry is a single hook command entry in the hooks settings file.
type hooksEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// hooksMatcher is a hook event matcher with its hooks list.
type hooksMatcher struct {
	Matcher string       `json:"matcher,omitempty"`
	Hooks   []hooksEntry `json:"hooks"`
}

// hooksSettings is the structure written to hooks-settings.json.
type hooksSettings struct {
	Hooks map[string][]hooksMatcher `json:"hooks"`
}

// ensureHooksSettingsFile generates hooks-settings.json inside stateDir with the
// jin hook command for all required hook events. The file is written on
// every call so that it stays up-to-date if the binary path changes.
// Returns the absolute path to the generated file.
func ensureHooksSettingsFile(stateDir, execPath string) (string, error) {
	entry := hooksEntry{
		Type:    "command",
		Command: execPath + " hook",
		Timeout: 5,
	}
	settings := hooksSettings{
		Hooks: map[string][]hooksMatcher{
			"UserPromptSubmit": {{Hooks: []hooksEntry{entry}}},
			"Stop":             {{Hooks: []hooksEntry{entry}}},
			"StopFailure":      {{Hooks: []hooksEntry{entry}}},
			"PostToolUse":      {{Hooks: []hooksEntry{entry}}},
			"CwdChanged":       {{Hooks: []hooksEntry{entry}}},
			"SessionStart":     {{Hooks: []hooksEntry{entry}}},
			"SessionEnd":       {{Hooks: []hooksEntry{entry}}},
			"Notification": {{
				Matcher: "permission_prompt|elicitation_dialog|idle_prompt",
				Hooks:   []hooksEntry{entry},
			}},
		},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal hooks settings: %w", err)
	}

	path := filepath.Join(stateDir, "hooks-settings.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write hooks settings file: %w", err)
	}

	debugLog("[HOOKS] Wrote hooks settings to %s", path)
	return path, nil
}
