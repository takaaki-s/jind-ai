package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/debug"
	"github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
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
	tmuxClient     tmux.Runner // tmux client; installed once at setup (SetTmuxClient) or lazily (ensureTmuxClient), read without m.mu after that — see SetTmuxClient
	hookRunner     worktreehook.Runner
	pluginDisp     plugin.Dispatcher
	gitClient      *git.Client
	agentResolver  AgentResolver // resolves AgentKind → Agent adapter (owns Layer C enhancer via Description())
	mu             sync.RWMutex
	paneSlotMu     sync.Mutex // serializes named-slot pane operations (find-then-split is check-then-act; see PaneSplit/PaneClose)
	tmuxInitMu     sync.Mutex // serializes lazy tmux init AND its recovery pass (see ensureTmuxClient)
	stateDir       string
	identity       jinenv.Identity // the jin the agents this Manager starts are told to call back into; handed in whole, never re-derived from the process
	tmuxSocketName string          // "" ⇒ tmux.SocketName; tests set an isolated name so ensureTmuxClient does not touch the shared "jin" server
}

// Identity is the jin the agents this Manager starts are told to call back
// into — whatever NewManager was handed, unchanged.
func (m *Manager) Identity() jinenv.Identity {
	return m.identity
}

// SetTmuxClient sets the tmux client for tmux-based session management. It is
// a one-shot setup-time setter: call before the daemon serves requests.
// tmuxClient is read without m.mu on hot paths, which is sound only because
// after setup the field is written at most once more, by ensureTmuxClient
// under both tmuxInitMu and m.mu.
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

// SetGitClient replaces the git client. Intended for tests that need to
// substitute a scripted Runner so the manager's git subprocess calls
// (worktree prune/add/remove, branch operations, dirty probes) become
// observable and deterministic. Production code never calls this; NewManager
// wires the real client via git.NewClient().
func (m *Manager) SetGitClient(c *git.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gitClient = c
}

// SetPluginDispatcher installs the plugin event dispatcher. A nil dispatcher
// disables plugin event publishing.
func (m *Manager) SetPluginDispatcher(d plugin.Dispatcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pluginDisp = d
}

// SetTmuxSocketName overrides the tmux socket name used by ensureTmuxClient's
// lazy fallback (production leaves this empty and gets tmux.DefaultSocketName,
// i.e. JIN_TMUX_SOCKET or "jin"). Tests set an isolated per-run name so the
// auto-init path cannot leak a real "-L jin" server that a later daemon start
// would inherit its environment from. Set once, before the first session start.
func (m *Manager) SetTmuxSocketName(name string) {
	m.tmuxSocketName = name
}

// SetAgentResolver installs the AgentResolver used by startSessionTmux and
// HandleHookEvent to fetch adapter behaviour. Left nil, session start returns
// an error rather than defaulting silently.
//
// Must be called exactly once at startup, before any goroutine reads the
// resolver. No lock is taken, as with the other one-shot setters: installing
// at runtime while other goroutines are already reading would race regardless.
func (m *Manager) SetAgentResolver(ar AgentResolver) {
	m.agentResolver = ar
}

// RecoverTmuxSessions checks for sessions with existing tmux windows after a
// daemon restart and resumes monitoring for live ones, or clears stale
// TmuxWindowName for dead ones.
//
// The tmux probes and the adapter's recover verdict are I/O, so none of it runs
// under m.mu — holding the Manager's central lock across the loop would stall
// the whole daemon. The phases are: snapshot under the lock, probe without it,
// re-take it to apply. applyRecovery re-validates each session against live
// state, so one deleted, killed or started while the probes ran keeps it.
func (m *Manager) RecoverTmuxSessions() {
	snaps, tc := m.snapshotForRecovery()
	if tc == nil {
		return
	}
	decisions := m.decideRecovery(snaps, tc)
	saves, monitors := m.applyRecovery(decisions)

	// Save copies rather than the live sessions: Store.Save marshals every
	// field, so handing it a live pointer outside the lock would race with
	// concurrent mutators. A write landing between apply and here can be
	// transiently rolled back on disk; memory stays authoritative and the
	// next Save reconverges (same trade-off as TryUpgradeDescription).
	for i := range saves {
		_ = m.store.Save(saves[i])
	}
	for _, s := range monitors {
		go m.captureOutputTmux(s)
	}
}

// recoverOutcome is what the probe phase concluded about one session.
type recoverOutcome int

const (
	// recoverMarkStopped: no tmux window; stop the session if it still
	// claims an active status (records left by a prior recovery bug).
	recoverMarkStopped recoverOutcome = iota
	// recoverWindowGone: the inner tmux session vanished; clear
	// TmuxWindowName and stop.
	recoverWindowGone
	// recoverPaneDead: the window survives (remain-on-exit) but the agent
	// pane is dead; stop, keeping TmuxWindowName so RespawnPane can revive.
	recoverPaneDead
	// recoverResume: pane is alive; restore the status and resume monitoring.
	recoverResume
)

// recoverDecision is the apply-phase instruction produced for one session.
type recoverDecision struct {
	id string
	// windowName is TmuxWindowName at snapshot time; apply re-validates it,
	// so a Kill or restart during the probe window invalidates the decision.
	windowName string
	// killSeq is Session.killSeq at snapshot time. Apply compares it with the
	// live counter to notice a Kill that landed while the probes ran — a kill
	// keeps the window standing, so windowName cannot catch that on its own.
	killSeq uint64
	// atProbe is Session.Status at snapshot time; apply compares it with the
	// live value to decide whether the verdict is still current.
	atProbe   Status
	outcome   recoverOutcome
	fromDisk  Status
	verdict   StatusUpdate
	verdictOK bool
}

// snapshotForRecovery copies every session under the lock so the probe phase
// can run I/O against the copies. Each copy retains the on-disk
// PersistedStatus while the live field is consumed at pass start, so a later
// pass cannot resurrect a stale value.
//
// The tmux client is captured under the same lock, so recovery never reads
// m.tmuxClient unsynchronized. tc is nil when no client is installed, and the
// caller then skips the pass entirely.
func (m *Manager) snapshotForRecovery() (snaps []Session, tc tmux.Runner) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.tmuxClient == nil {
		return nil, nil
	}
	snaps = make([]Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		snap := *session
		session.PersistedStatus = "" // consumed; the copy keeps it
		snaps = append(snaps, snap)
	}
	return snaps, m.tmuxClient
}

// decideRecovery runs the tmux probes and the adapter verdict for each
// snapshot. No lock is held: HasSession/IsPaneDead exec tmux, and the verdict
// may scan the agent's transcript end to end.
func (m *Manager) decideRecovery(snaps []Session, tc tmux.Runner) []recoverDecision {
	decisions := make([]recoverDecision, 0, len(snaps))
	for i := range snaps {
		snap := &snaps[i]
		d := recoverDecision{
			id:         snap.ID,
			windowName: snap.TmuxWindowName,
			killSeq:    snap.killSeq,
			atProbe:    snap.Status,
			fromDisk:   snap.PersistedStatus,
		}
		switch {
		case snap.TmuxWindowName == "":
			d.outcome = recoverMarkStopped
		case !tc.HasSession(snap.TmuxWindowName):
			d.outcome = recoverWindowGone
		case tc.IsPaneDead(snap.TmuxPaneID):
			d.outcome = recoverPaneDead
		default:
			d.outcome = recoverResume
			// Hooks fired while the daemon was down are lost, so the
			// persisted value can be stale (a missed Stop leaves the
			// session "thinking" forever). Let the adapter re-derive it;
			// a false verdict keeps applyRecovery's fallback decision.
			persisted := resumeStatusSource(snap.Status, snap.PersistedStatus)
			d.verdict, d.verdictOK = m.recoverStatusVerdict(snap, persisted)
		}
		decisions = append(decisions, d)
	}
	return decisions
}

// resumeStatusSource returns the best estimate of a recovered session's real
// state: the live in-memory status when it carries detail (hooks may have
// fired since load), otherwise the value persisted before the restart.
func resumeStatusSource(live, fromDisk Status) Status {
	if (live == StatusStopped || live == StatusCreating) && fromDisk != "" {
		return fromDisk
	}
	return live
}

// interruptedAsyncMessage returns the ErrorMessage to stamp on a session
// whose pre-restart Status implies an async operation was in flight when the
// daemon went down. Empty for statuses that do not carry such an implication.
func interruptedAsyncMessage(persisted Status) string {
	switch persisted {
	case StatusCreating:
		return "provisioning was interrupted by daemon restart"
	case StatusDeleting:
		return "deletion was interrupted by daemon restart; retry with `jin session delete`"
	}
	return ""
}

// applyRecovery re-takes the lock and applies each decision, re-validating it
// against live state first. It returns copies to persist and the live
// sessions to start monitoring, both handled by the caller outside the lock.
func (m *Manager) applyRecovery(decisions []recoverDecision) (saves []Session, monitors []*Session) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, d := range decisions {
		live, ok := m.sessions[d.id]
		if !ok {
			continue // deleted while we probed
		}
		// StartedAt is runtime-only (json:"-"): non-zero means THIS daemon
		// process started the session itself after the snapshot, so a
		// decision derived from pre-restart observations no longer applies.
		if !live.StartedAt.IsZero() {
			continue
		}
		// A restart, or a Kill that had to fall back to destroying the pane,
		// means the probe result describes a window we no longer track.
		if live.TmuxWindowName != d.windowName {
			continue
		}
		// The ordinary Kill leaves the window standing, so the check above
		// cannot see it, and Status cannot stand in either: a session reloaded
		// from disk is already Stopped before anyone kills anything. The counter
		// is the one mark a kill always leaves, and every probe behind this
		// decision predates it.
		if live.killSeq != d.killSeq {
			debugLog("[RECOVER] Session %s killed while probing, dropping the stale decision", live.Description)
			continue
		}

		switch d.outcome {
		case recoverMarkStopped:
			// Fix stale sessions: active status but no tmux session (from
			// prior recovery bug). Checked against live status so an
			// already-stopped session is not re-saved.
			if live.Status != StatusStopped && live.Status != StatusCreating {
				live.Status = StatusStopped
				saves = append(saves, *live)
				debugLog("[RECOVER] Session %s has active status but no tmux session, marked stopped", live.Description)
			} else if msg := interruptedAsyncMessage(d.fromDisk); msg != "" {
				// Persisted Status was Creating or Deleting and there is no
				// live tmux — an async op was interrupted by the daemon
				// restart. Re-save with an ErrorMessage so the user sees why
				// the session is stuck; the "already-stopped" guard above
				// would otherwise skip the write and lose that signal.
				live.Status = StatusStopped
				live.ErrorMessage = msg
				saves = append(saves, *live)
				debugLog("[RECOVER] Session %s: interrupted async op (%s), marked stopped with error", live.Description, d.fromDisk)
			}
		case recoverWindowGone:
			live.TmuxWindowName = ""
			live.Status = StatusStopped
			if msg := interruptedAsyncMessage(d.fromDisk); msg != "" {
				live.ErrorMessage = msg
			}
			saves = append(saves, *live)
			debugLog("[RECOVER] Session %s inner tmux session gone, marked stopped", live.Description)
		case recoverPaneDead:
			live.Status = StatusStopped
			if msg := interruptedAsyncMessage(d.fromDisk); msg != "" {
				live.ErrorMessage = msg
			}
			saves = append(saves, *live)
			debugLog("[RECOVER] Session %s tmux pane dead, kept TmuxWindowName (session preserved)", live.Description)
		case recoverResume:
			// A persisted StatusDeleting means the user was already
			// deleting this session when the daemon went down, and that
			// intent wins over the pane being alive. Mark stopped with the
			// interruption message so a retry is obvious; monitoring is
			// intentionally skipped.
			if d.fromDisk == StatusDeleting {
				live.Status = StatusStopped
				live.ErrorMessage = interruptedAsyncMessage(StatusDeleting)
				saves = append(saves, *live)
				debugLog("[RECOVER] Session %s: delete interrupted with live pane, marked stopped (retry via `jin session delete`)", live.Description)
				continue
			}
			// The verdict was derived from the pre-probe snapshot, so a hook
			// that landed since outranks it. An adapter may answer "recover"
			// from the persisted value alone, and such a verdict has nothing
			// agent-side to reconverge with: see the recovery section of
			// docs/session-lifecycle.md for what it costs to let it win.
			//
			// The comparison is by value, so a hook that rewrites the same
			// status is invisible here — unlike killSeq above, which exists
			// because Status could not stand in for detecting a kill either.
			if d.verdictOK && live.Status == d.atProbe {
				// Only Status is applied — see the "recover" contract on
				// StatusSignal.
				live.Status = d.verdict.Status
			} else {
				// Fallback: the hook-driven status persisted
				// before the restart is the best estimate; only
				// detail-less states fall back to Running.
				// Recomputed from live status so a hook that fired
				// during the probe window still wins.
				switch persisted := resumeStatusSource(live.Status, d.fromDisk); persisted {
				case StatusIdle, StatusThinking, StatusPermission:
					live.Status = persisted
				default:
					live.Status = StatusRunning
				}
			}
			live.LastOutputTime = time.Now()
			saves = append(saves, *live)
			monitors = append(monitors, live)
			debugLog("[RECOVER] Session %s has live inner tmux session, resuming monitoring (status: %s)", live.Description, live.Status)
		}
	}
	return saves, monitors
}

// recoverStatusVerdict asks the session's agent adapter to re-derive the status
// of a recovered pane-alive session from agent-side persistent data. session is
// the probe-phase snapshot copy, and the call runs WITHOUT m.mu held, which is
// the point: the adapter may scan a large transcript. Returns false when no
// resolver is configured, the kind is unknown, or the adapter cannot tell.
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
//
// Must be called WITHOUT m.mu held: on a fresh init it runs recovery, which
// takes and releases the lock per phase. tmuxInitMu is held for the whole init
// INCLUDING that recovery, so a racing loser blocks until the winner's recovery
// has been applied — StartBackground's isProcessRunning check depends on that
// not to double-start a session whose pane is still alive — and recovery runs
// at most once, so captureOutputTmux monitors cannot be spawned twice.
//
// Lock order: tmuxInitMu → m.mu. Nothing takes them in reverse.
//
// The socket name comes from tmux.DefaultSocketName() unless SetTmuxSocketName
// overrode it. Either way the auto-init must not leak a server on the shared
// socket that the next daemon start would inherit its environment from.
func (m *Manager) ensureTmuxClient() {
	m.tmuxInitMu.Lock()
	defer m.tmuxInitMu.Unlock()

	m.mu.RLock()
	have := m.tmuxClient != nil
	socketName := m.tmuxSocketName
	m.mu.RUnlock()
	if have {
		return
	}
	if socketName == "" {
		socketName = tmux.DefaultSocketName()
	}
	// Probes the PATH for the tmux binary — I/O, so outside m.mu.
	tc, err := tmux.NewClientWithSocket(socketName)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.tmuxClient = tc
	m.mu.Unlock()
	debugLog("[TMUX] Inner tmux client initialized (socket: %s)", socketName)
	// Don't call configureInnerTmux here — the inner tmux server may not exist yet.
	// Configuration is applied in startSessionTmux after the first session is created.
	m.RecoverTmuxSessions()
}

// configureInnerTmux applies jin-specific settings to the inner tmux server
// (the user's ~/.tmux.conf is loaded by tmux itself on server startup). Only
// valid once that server is confirmed to exist. It runs on every session start
// rather than once, because the inner server may have exited and restarted in
// between; the overhead is minimal.
//
// remain-on-exit is NOT set globally. TagManagedPane sets it per-pane on
// managed panes only, so user-added panes are destroyed on exit instead of
// showing "Pane is dead".
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
// sessionsDir is where per-session JSON files live; stateDir is where generated
// artifacts such as hooks-settings.json are written; identity is the jin the
// agents this Manager starts are told to call back into.
//
// identity is an argument rather than something assembled here, for the same
// reason stateDir is: a value read from the process would describe whichever
// daemon the reader happens to be, not the one that started the agent. Its
// caller gives the same value to the plugin dispatcher, so an agent and a
// plugin cannot be told to re-enter different binaries.
func NewManager(sessionsDir, stateDir string, identity jinenv.Identity, configMgr *config.Manager) (*Manager, error) {
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
		identity:  identity,
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
		// immediately, without waiting for the next captureOutputTmux poll.
		s.IsWorktree = git.IsGitWorktreeDir(s.WorkDir)
		// Same story for RepoName, and it matters more for stopped sessions:
		// they never reach captureOutputTmux at all, so the poll would never
		// fill it in.
		s.RepoName = ResolveRepoName(s.WorkDir)
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
	Model       string // Agent model in the CLI's own spelling; empty = the agent's own default

	Worktree       bool   // Create a git worktree for this session
	NoHook         bool   // Skip the worktree post-create hook (worktree path only)
	WorktreeName   string // Override auto-generated worktree name
	WorktreeBranch string // Override auto-generated branch name
	WorktreeBase   string // Override auto-detected base branch (default: origin/HEAD)
}

// worktreeProvisioning carries the outputs of provisionWorktree back to its
// caller. undo undoes the on-disk changes (git worktree + branch) and is safe
// to call zero or one times. warning may be non-empty even when err is nil.
type worktreeProvisioning struct {
	worktreePath string
	branch       string
	warning      string
	undo         func()
}

// provisionWorktree runs the git subprocess chain and post-create hook that a
// worktree-backed session needs before it is usable. All I/O runs outside m.mu.
// undo removes the worktree checkout and deletes the branch; the caller must
// invoke it on any later failure that leaves the session unusable.
//
// sessionID seeds the auto-derived worktree name. Worktree=true is required.
func (m *Manager) provisionWorktree(sessionID string, opts CreateOptions) (worktreeProvisioning, error) {
	var out worktreeProvisioning
	if !opts.Worktree {
		return out, fmt.Errorf("provisionWorktree called with Worktree=false")
	}
	if !git.IsGitRoot(opts.WorkDir) {
		return out, fmt.Errorf("not a git repository: %s", opts.WorkDir)
	}

	cfg := m.configMgr.GetWorktreeConfig()

	base := opts.WorktreeBase
	if base == "" {
		detected, err := m.gitClient.DetectDefaultBranch(opts.WorkDir)
		if err != nil {
			base = cfg.DefaultBranch
			if base == "" {
				return out, fmt.Errorf("cannot detect default branch: %w", err)
			}
		} else {
			base = detected
		}
	}

	originalRepoDir := opts.WorkDir
	repoBasename := filepath.Base(originalRepoDir)
	baseName := deriveWorktreeName(sessionID, opts.WorktreeName)
	pathTemplate := worktreeTemplate(cfg.BaseDir, m.stateDir)

	// Clear orphan worktree registrations (`.git/worktrees/<name>/` metadata
	// left after a manual `rm -rf` of the worktree directory) so the
	// collision check below reflects the true git state. Best-effort:
	// prune failures shouldn't block session creation.
	if err := m.gitClient.PruneWorktrees(originalRepoDir); err != nil {
		debugLog("[WORKTREE] prune failed for %s: %v", originalRepoDir, err)
	}

	var (
		finalName string
		branch    string
	)
	if opts.WorktreeName != "" {
		// Explicit override: honour the user's choice verbatim. Pre-check
		// the branch so we fail fast with a clear message instead of
		// leaking git's raw "fatal: branch 'X' already exists" through
		// AddWorktree.
		finalName = opts.WorktreeName
		branch = deriveBranchName(finalName, cfg.BranchPrefix, opts.WorktreeBranch)
		if m.gitClient.BranchExists(originalRepoDir, branch) {
			return out, fmt.Errorf("branch %q already exists", branch)
		}
	} else {
		collides := func(candidate string) bool {
			candidatePath, err := expandBaseDir(pathTemplate, candidate, repoBasename)
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
			return out, err
		}
		finalName = name
		branch = deriveBranchName(finalName, cfg.BranchPrefix, opts.WorktreeBranch)
	}

	worktreePath, err := expandBaseDir(pathTemplate, finalName, repoBasename)
	if err != nil {
		return out, err
	}

	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return out, fmt.Errorf("creating worktree parent dir: %w", err)
	}

	if err := m.gitClient.AddWorktree(originalRepoDir, branch, worktreePath, "origin/"+base); err != nil {
		return out, fmt.Errorf("git worktree add: %w", err)
	}

	// Build undo before anything that might fail after AddWorktree — every
	// subsequent error path returns undo so the caller can invoke it.
	undo := func() {
		if err := m.gitClient.RemoveWorktree(worktreePath, true); err != nil {
			debugLog("[WORKTREE] rollback: RemoveWorktree failed for %s: %v", worktreePath, err)
		}
		if err := m.gitClient.DeleteBranch(originalRepoDir, branch); err != nil {
			debugLog("[WORKTREE] rollback: DeleteBranch failed for %s: %v", branch, err)
		}
	}

	var hookWarning string

	// Post-create hook: runs synchronously so a non-zero exit tears the
	// worktree/branch back down through undo. Skipped silently when the
	// caller opts out, the runner is not wired up, or config disables the
	// feature.
	if !opts.NoHook && m.hookRunner != nil &&
		(cfg.HookEnabled == nil || *cfg.HookEnabled) {

		scriptPath, exists := m.hookRunner.Discover(originalRepoDir)
		if exists {
			verdict, verifyErr := m.hookRunner.Verify(scriptPath, originalRepoDir)
			if verifyErr != nil {
				// Verify may return a verdict alongside err (e.g. hash
				// failure); treat err as authoritative and abort before
				// switching on verdict to avoid running an unverified hook.
				undo()
				return out, fmt.Errorf("verify worktree hook: %w", verifyErr)
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
					LogPath:      logPath,
					Timeout:      timeout,
				})
				cancel()
				if runErr != nil {
					undo()
					return out, fmt.Errorf("worktree post-create hook failed: %w (log: %s)", runErr, logPath)
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

	out.worktreePath = worktreePath
	out.branch = branch
	out.warning = hookWarning
	out.undo = undo
	return out, nil
}

// ReserveCreation validates a create request, mints a session ID, and registers
// a StatusCreating record so the client has an ID to poll immediately. It does
// no external I/O — no git, no hook, no tmux.
//
// It returns the live session pointer (for ProvisionAsync; only the immutable
// sess.ID is read once m.mu is released) and a value-copy Info taken in the
// same critical section. Handlers must marshal the response from that Info,
// not from sess.ToInfo(): once the goroutine kicks off, ProvisionAsync can
// mutate the record and a later ToInfo() would race.
//
// For the worktree case opts.WorkDir is treated as the repo root and becomes a
// placeholder WorkDir that ProvisionAsync overwrites. The workDir conflict
// check is skipped there because the placeholder is deliberately shared by
// concurrent worktree sessions in the same repo; findAvailableWorktreeName
// guarantees the final paths are unique.
func (m *Manager) ReserveCreation(opts CreateOptions) (*Session, Info, error) {
	if opts.Fleet == "" {
		opts.Fleet = DefaultFleet
	}
	if opts.WorkDir == "" {
		return nil, Info{}, fmt.Errorf("work directory is required")
	}

	sessionID := uuid.New().String()

	// Layer A description. For the worktree case, opts.WorkDir is still the
	// repo root; ProvisionAsync recomputes the baseline against the final
	// worktree path if the caller did not lock the description.
	description := strings.TrimSpace(opts.Description)
	locked := true
	if description == "" {
		description = GenerateBaselineDescription(opts.WorkDir, "", false, "")
		locked = false
	}

	agentKind := opts.AgentKind
	if agentKind == "" {
		agentKind = "claude"
	}

	session := &Session{
		ID:                sessionID,
		Description:       description,
		DescriptionLocked: locked,
		WorkDir:           opts.WorkDir,
		CreatedAt:         time.Now(),
		Status:            StatusCreating,
		AgentKind:         agentKind,
		AgentSessionID:    uuid.New().String(),
		Model:             opts.Model,
		Fleet:             opts.Fleet,
		// Set IsWorktree immediately so the TUI delete modal offers the
		// worktree removal option without waiting for the next
		// captureOutputTmux poll cycle. `opts.Worktree` reflects "we will
		// create a worktree"; also check the WorkDir for cases where the
		// user pointed at an existing worktree directly.
		IsWorktree: opts.Worktree || git.IsGitWorktreeDir(opts.WorkDir),
		// For the worktree case this resolves opts.WorkDir (still the repo
		// root at this point), which yields the same repo name the final
		// worktree path will; ProvisionAsync recomputes it anyway.
		RepoName: ResolveRepoName(opts.WorkDir),
	}

	m.mu.Lock()

	// Skip the workDir conflict check for the worktree case: opts.WorkDir is
	// the repo root and multiple concurrent worktree creates legitimately
	// share it as a placeholder. The final worktree path (set by
	// ProvisionAsync) is guaranteed unique via findAvailableWorktreeName.
	if !opts.Worktree {
		if s := m.workDirConflictLocked(opts.WorkDir, ""); s != nil {
			m.mu.Unlock()
			return nil, Info{}, fmt.Errorf("session already exists for directory: %s (session: %s)", opts.WorkDir, s.Description)
		}
	}

	m.sessions[sessionID] = session
	// Snapshot both the persistable copy (for Save) and the client-facing
	// Info (for the daemon response) under the lock. Callers must marshal
	// their response from this info — a later sess.ToInfo() races with the
	// provisioning goroutine's writes to the same record.
	saved := *session
	info := session.ToInfo()
	m.mu.Unlock()

	if err := m.store.Save(saved); err != nil {
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
		return nil, Info{}, err
	}

	return session, info, nil
}

// ProvisionAsync runs the external provisioning work (git worktree add,
// post-create hook) for a session previously registered by ReserveCreation. On
// error, on-disk state is fully undone and the record is left untouched; the
// caller decides how to surface the failure.
//
// On success WorkDir is updated to the final worktree path, the baseline
// description is recomputed against it when unlocked, and the update is
// persisted. Status is left at StatusCreating so callers decide what follows.
func (m *Manager) ProvisionAsync(sess *Session, opts CreateOptions) (string, error) {
	if !opts.Worktree {
		// Nothing to provision — non-worktree sessions are usable as soon as
		// ReserveCreation returns.
		return "", nil
	}
	prov, err := m.provisionWorktree(sess.ID, opts)
	if err != nil {
		return "", err
	}

	// ResolveRepoName stats the filesystem, so settle it before taking the lock.
	// GenerateBaselineDescription below is called while the lock IS held, as a
	// bounded cost accepted there — do not read that as this file's rule.
	repoName := ResolveRepoName(prov.worktreePath)

	m.mu.Lock()
	live, ok := m.sessions[sess.ID]
	if !ok {
		// Session was deleted while provisioning was in flight; undo the
		// on-disk changes so we do not leak a worktree and branch.
		m.mu.Unlock()
		prov.undo()
		return "", fmt.Errorf("session %s no longer registered (deleted during provisioning)", sess.ID)
	}
	// Final WorkDir conflict check against the resolved worktree path.
	// findAvailableWorktreeName makes collisions extremely rare, but "no two
	// sessions manage the same directory" has to hold, and the map insert is in
	// this same critical section.
	if s := m.workDirConflictLocked(prov.worktreePath, sess.ID); s != nil {
		m.mu.Unlock()
		prov.undo()
		return "", fmt.Errorf("session already exists for directory: %s (session: %s)", prov.worktreePath, s.Description)
	}
	live.WorkDir = prov.worktreePath
	// Recomputed against the final path rather than left on ReserveCreation's
	// seed. The two agree in every reachable case today, so this changes nothing
	// on its own; it keeps the pair honest, since WorkDir is being reassigned on
	// this line and RepoName describes WorkDir.
	live.RepoName = repoName
	if !live.DescriptionLocked {
		live.Description = GenerateBaselineDescription(prov.worktreePath, "", false, "")
	}
	saved := *live
	m.mu.Unlock()

	if err := m.store.Save(saved); err != nil {
		prov.undo()
		return "", fmt.Errorf("saving session after provisioning: %w", err)
	}
	return prov.warning, nil
}

// MarkCreationFailed persists a failure verdict on a reserved session's async
// creation: Status flips to Stopped and ErrorMessage carries err. The record is
// kept so clients that poll `get` can still see what happened. Idempotent.
//
// The store.Save is fire-and-forget: memory stays authoritative and the next
// Save reconverges, but a failed persist matters diagnostically, so it is
// logged rather than dropped.
func (m *Manager) MarkCreationFailed(id string, err error) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	session.Status = StatusStopped
	if err != nil {
		session.ErrorMessage = err.Error()
	}
	saved := *session
	m.mu.Unlock()
	if saveErr := m.store.Save(saved); saveErr != nil {
		debugLog("[SESSION] MarkCreationFailed %s: persist failed: %v", id, saveErr)
	}
}

// SetCreationWarning records a non-fatal warning produced during async creation
// (e.g. post-create hook detected but not allowed). It lives on the record
// until the session is deleted, so later `get` responses can surface it.
// Idempotent — a repeat call overwrites the previous value.
//
// The store.Save is fire-and-forget, same reasoning as MarkCreationFailed.
func (m *Manager) SetCreationWarning(id string, warning string) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	session.CreationWarning = warning
	saved := *session
	m.mu.Unlock()
	if err := m.store.Save(saved); err != nil {
		debugLog("[SESSION] SetCreationWarning %s: persist failed: %v", id, err)
	}
}

// dropSession removes a session record from the in-memory map and the store,
// unconditionally. Intended for the sync-compat path in CreateWithOptions
// where provisioning failure must leave no persisted record.
func (m *Manager) dropSession(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	_ = m.store.Delete(id)
}

// CreateWithOptions creates a new session with full options, synchronously. It
// is a thin composition of ReserveCreation + ProvisionAsync + (on failure)
// dropSession, preserving the contract that no session record is persisted when
// creation fails.
//
// The second return value is retained for signature compatibility and is always
// "": a warning goes to Session.CreationWarning, which callers read back
// through Get / GetInfo. Prefer ReserveCreation + ProvisionAsync directly when
// the caller wants its session ID before external I/O completes.
func (m *Manager) CreateWithOptions(opts CreateOptions) (*Session, string, error) {
	sess, _, err := m.ReserveCreation(opts)
	if err != nil {
		return nil, "", err
	}
	warning, provErr := m.ProvisionAsync(sess, opts)
	if provErr != nil {
		// Sync compat: caller expects "nothing persisted on failure".
		m.dropSession(sess.ID)
		return nil, "", provErr
	}
	if warning != "" {
		m.SetCreationWarning(sess.ID, warning)
	}
	// Provisioning succeeded but no agent has been started. Transition
	// Status off Creating so callers that skip StartBackground (existing
	// tests, non-daemon helpers) do not see a stuck "creating" state.
	m.SetStatus(sess.ID, StatusStopped)
	return sess, "", nil
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

	// Phase 2: enrich with transcript data outside the lock (slow I/O).
	//
	// Concurrently, because the rows are independent: no lock is held, each
	// iteration writes its own element, and every adapter's Transcript() is
	// constructed per call. It matters because the TUI refetches the whole list
	// every two seconds — measured over 40 transcripts, 248ms serial against
	// 133ms across four workers. The bound also caps how many []Entry are live.
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for i := range infos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m.AttachLastMessages(&infos[i])
		}(i)
	}
	wg.Wait()

	SortInfos(infos)

	return infos
}

// AttachLastMessages fills in the two message previews the list rows and
// `session info` show, reading through the adapter that owns the session.
//
// Every failure stays silent here, deliberately and unlike `session result`:
// this decorates a row that has to render either way, and failing a whole
// `session list` because one transcript was unreadable would be worse than a
// row with an empty second line.
//
// A reader that has not declared itself cheap is skipped, and that is the
// difference between this being a read and being a fork. List calls this for
// every row, in parallel, and the TUI refreshes on a timer; the opencode
// reader answers by running `opencode export`, a subprocess whatever the
// session's size. Neither this function nor that reader is wrong on its own,
// which is exactly why the guard has to be here rather than in either.
//
// The guard sits here rather than at the callers, which is a trade: handleGet
// also serves one-shot commands — `session info`, `session output` — that
// would happily pay for a preview, and they lose it too. A kind that both
// needed previews and cost a subprocess would have to lift this check out.
func (m *Manager) AttachLastMessages(info *Info) {
	if info.AgentSessionID == "" || info.WorkDir == "" {
		return
	}
	ag := m.resolveAgent(info.AgentKind)
	if ag == nil {
		return
	}
	src := ag.Transcript()
	if src == nil {
		return
	}
	if _, ok := src.(PollableTranscriptSource); !ok {
		return
	}
	entries, err := src.ReadEntries(info.WorkDir, info.AgentSessionID, "")
	if err != nil {
		return
	}
	msgs := transcript.LastMessagesFrom(entries)
	if msgs == nil {
		return
	}
	if msgs.User != nil {
		info.LastUserMessage = transcript.TruncateMessage(msgs.User.Content, 500)
	}
	if msgs.Assistant != nil {
		info.LastAssistantMessage = transcript.TruncateMessageFromEnd(msgs.Assistant.Content, 500)
	}
}

// Get returns a session by ID. The returned pointer aliases the live map
// entry; callers must not read or write through it once the manager can
// mutate it in another goroutine. Prefer GetInfo for a race-free snapshot
// whenever the caller only needs a read.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// GetInfo returns a value-copy Info snapshot of a session, taken under the
// read lock so it is safe to inspect while other goroutines mutate the live
// record. This is the read path async callers (the daemon `get` handler,
// tests observing goroutine progress) should use — Get's aliased pointer
// races with the write side of ProvisionAsync / SetStatus / MarkDeleting.
func (m *Manager) GetInfo(id string) (Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return Info{}, false
	}
	return s.ToInfo(), true
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
// The values below were fixed by measurement against Claude Code 2.1.220,
// Codex 0.144.6 and OpenCode 1.17.18 driven through a throwaway tmux server.
var (
	// sendVerifyTimeoutBase is the fixed part of the send-verify retry budget;
	// the total also scales with the prompt (see sendVerifyBudget), since a
	// flat 5s would leave big sends with no retries at all. On timeout
	// SendPrompt returns an error before ever pressing Enter.
	sendVerifyTimeoutBase = 5 * time.Second
	// sendVerifyPerChunk and sendVerifyPerClearKey extend that budget by the
	// cost of one chunk send and one clear keypress. Every tmux verb is a
	// separate process, so process startup dominates and the payload barely
	// matters:
	//
	//	direct tmux 3.6a binary     ~1.4ms per send-keys / capture-pane
	//	exec.Command from Go        ~3ms   (what this package actually does)
	//	from a shell, via a shim    ~33ms  (the shim re-execs the real one)
	//
	// Sized from the slow figure: over-estimating only delays reporting a
	// genuinely stuck send, while under-estimating silently reduces large
	// prompts to a single attempt with no retry.
	sendVerifyPerClearKey = 35 * time.Millisecond
	// Derived rather than written as a literal so that changing
	// sendChunkDelay cannot silently under-budget the loop: a chunk costs one
	// tmux invocation plus the delay that follows it.
	sendVerifyPerChunk = sendVerifyPerClearKey + sendChunkDelay + 5*time.Millisecond
	// sendVerifySettleDelay is how long we wait between the last chunk
	// (plus nudge) and the follow-up CapturePane. Empirically ~50ms is
	// enough for tmux to reflect the literal into the pane's visible
	// buffer.
	sendVerifySettleDelay = 50 * time.Millisecond
	// sendVerifyBackoff is the pause between a failed verify and the
	// next re-send. Kept small so a genuinely-not-ready TUI recovers
	// within a few hundred ms once it is ready.
	sendVerifyBackoff = 100 * time.Millisecond
	// sendVerifyTailBytes is how many trailing bytes of the normalized prompt
	// are looked for in the capture. Long prompts get wrapped by the TUI's
	// input area, so matching only the tail avoids reflow false negatives.
	//
	// Do not raise this hoping for a stronger anchor: a longer needle is MORE
	// likely to straddle a wrapped row, not less.
	sendVerifyTailBytes = 32
	// sendClearSettleDelay is the wait after the adapter's ClearInputKeys
	// sequence before capturing the baseline — long enough for a well-behaved
	// TUI to render the empty input line, short enough to stay negligible
	// across a full retry chain. Skipped when the adapter returns no keys.
	sendClearSettleDelay = 20 * time.Millisecond
	// sendDismissSettleDelay is the wait after the adapter's DismissOverlayKeys
	// sequence before re-capturing to confirm the prompt survived it.
	//
	// It cannot be sized by analogy with sendClearSettleDelay: that delay guards
	// a step whose effect is never checked, so being short only risks a wasted
	// keypress. Here the delay IS the check — capture too early and the pane
	// still shows the pre-dismiss state, where the prompt is present by
	// definition. Measured on Claude Code 2.1.224 with the destructive case
	// (C-u): 54, 83, 93, 116 and 161ms over five runs. Paid once per send, and
	// only for prompts an adapter flagged as able to open an overlay.
	sendDismissSettleDelay = 250 * time.Millisecond

	// sendVerifyLooks is how many times one attempt re-checks the pane before
	// giving up and re-sending; sendVerifyLookDelay is the gap between looks
	// (the first uses sendVerifySettleDelay).
	//
	// Looking is cheap — a nudge plus a capture — while re-sending clears the
	// input and pushes the whole prompt again, discarding whatever the TUI had
	// drawn. Without this, an agent slower than one settle delay is reset before
	// it can finish: Codex with a 16KB prompt burned the entire budget across 16
	// attempts, then passed in 1.7s once the attempt looked instead of resending.
	//
	// The count scales with the prompt because each look sends one nudge, and on
	// OpenCode the nudge is what walks the cursor toward the end of a multi-row
	// input (see sendNudgeKey).
	sendVerifyLooksBase = 10
	sendVerifyLooksMax  = 60
	sendVerifyLookDelay = 200 * time.Millisecond

	// sendChunkMaxBytes caps one SendKeysLiteral payload. Two separate ceilings
	// force the split:
	//
	//   - tmux send-keys -l refuses arguments over 16341 bytes outright.
	//   - the agent TUIs fold a single oversized read into a "[Pasted Content N
	//     chars]" placeholder, which hides the prompt tail from capture-pane and
	//     breaks verify even though the text landed. Measured fold thresholds:
	//     Claude 801B, Codex 1001B, OpenCode none.
	//
	// 800 is the largest value under every measured threshold. One number serves
	// all three agents; make it an adapter capability when a fourth needs a
	// different ceiling.
	sendChunkMaxBytes = 800
	// sendChunkDelay separates consecutive chunk sends. With no delay,
	// Codex coalesces adjacent chunks into a single read and folds them
	// anyway — which defeats the split entirely (measured: 12x800B at 0ms
	// produced placeholders swallowing 2400-3200B; at 20ms, zero). Claude
	// does not need it and is unharmed by it.
	sendChunkDelay = 20 * time.Millisecond
	// sendNudgeKey is sent before each verify capture. Two agents need it, for
	// two different reasons:
	//
	//   - Codex repaints only on key events, so without one a capture can show
	//     stale content indefinitely (measured: still stale after 37s).
	//   - OpenCode draws only a fixed-size window of its input buffer, and that
	//     window follows the CURSOR. A prompt taller than the window leaves the
	//     tail undrawn — and undrawn means capture-pane cannot see it at all:
	//     the rest lives only inside OpenCode, never written to the terminal.
	//
	// "Down" rather than "End" because of that second case. `End` goes to the end
	// of the current visual row, which on a wrapped input never reaches the end
	// of the buffer, so the window never scrolls. Measured on a 48-column pane, a
	// 2KB prompt stayed invisible under `End` at every interval tried and became
	// visible after roughly 20 `Down` presses; `C-End`, `NPage` and `C-e` did
	// nothing.
	//
	// `Down` is NOT inert on an input holding a completion overlay: it walks the
	// overlay's selection. Measured on Claude Code 2.1.224 with a slash prefix
	// matching two commands, the second ran rather than the first, 3/3. The fix
	// belongs at the other end — DismissOverlayKeys closes the overlay before
	// Enter — because making the nudge conditional would cost the thing it
	// exists for on OpenCode.
	sendNudgeKey = "Down"
	// sendPasteBufferName is the tmux buffer the paste transport reuses.
	// A fixed name keeps the buffer stack from growing; PasteBuffer's `-d`
	// removes it right after pasting, so a prompt is never left readable
	// there.
	sendPasteBufferName = "jin-prompt"
	// sendClearWidthAssumed is the input-line width assumed when deciding how
	// many times to repeat the clear sequence, since each press clears one
	// visual row.
	//
	// Deliberately below every measured width (Claude 196, Codex 197, OpenCode
	// 70): overshooting is harmless — the clear key is a no-op on empty input —
	// while undershooting leaves residue that concatenates with the prompt at
	// Enter time.
	sendClearWidthAssumed = 60
	// sendClearMaxKeys caps the repeat count so a pathological prompt cannot
	// spin the pane for minutes. 512 repeats x 60 columns covers roughly 30KB of
	// residue; past that SendPrompt continues best-effort with a
	// possibly-incomplete clear (documented in docs/gotchas.md).
	//
	// This is the dominant cost of a large send: one tmux process per press.
	// `send-keys -N` would batch them, but only Codex honours it.
	sendClearMaxKeys = 512
)

// Respond tuning. Kept as vars so tests can shorten them, like the send knobs
// above, but read them differently: the send values are measurements, these are
// bounds. Nothing below was derived from timing a dialog, and none of them
// should be quoted as if it were.
//
// Being generous is cheap in both directions. Overshooting costs a slower error
// on a session that was never going to answer, which RespondToBlock reports
// rather than papers over; undershooting would report failure on an agent that
// had merely not repainted yet, with the keys already in the pane.
var (
	// respondClearPollDelay is the pause between two captures. Both loops use
	// it: the one waiting for a Verify step's text to render, and the one
	// waiting for the block to leave the pane. One number rather than two,
	// because nothing measured says the two waits differ.
	respondClearPollDelay = 200 * time.Millisecond
	// respondClearBudget bounds the wait for the block to disappear. The keys
	// are already out by the time this loop starts, so expiry decides only how
	// long jin looks before saying it does not know — the error says exactly
	// that, and RespondToBlock has no way to take an answer back.
	respondClearBudget = 10 * time.Second
	// respondVerifyLooks is how many times a Verify step re-checks the pane for
	// its own text before giving up.
	//
	// There is no re-send behind it, which is the difference from
	// sendVerifyLooksBase: a TUI input line is idempotent, but these keys address
	// a dialog by position, so sending them twice answers a different question.
	// Exhausting the looks abandons the sequence — the committing Enter after it
	// is not sent at all.
	respondVerifyLooks = 10
)

// ErrBlockNotCleared reports that a prompt was still on screen after its answer
// was sent.
//
// It is a sentinel rather than a phrase to match, because the CLI maps exactly
// this case to the timeout exit code and the two live in different packages. A
// reworded message would turn a documented exit 4 into a generic exit 1 with
// nothing failing.
var ErrBlockNotCleared = errors.New("the prompt was still on screen")

// NormalizeForVerify strips every rune that the TUI may inject into — or
// around — the prompt while rendering it, so a needle taken from the prompt
// still matches the captured pane.
//
// Two classes are removed:
//
//   - Whitespace. capture-pane emits a newline at each wrap position and the
//     TUIs pad with cursor-positioning spaces. Collapsing runs to a single
//     space is not enough: the wrap inserts a separator where the prompt has
//     none, so needle and haystack disagree exactly at the seam. Measured
//     failure rate for a 32-byte tail: ~16% on Claude and Codex, ~44% on
//     OpenCode. Japanese text meets the condition essentially always.
//   - Box-drawing runes (U+2500-U+257F). OpenCode draws a vertical bar at the
//     start of every wrapped row.
//
// Apply it to BOTH sides of the comparison, so a prompt that legitimately
// contains box-drawing characters stays symmetric and still matches. Exported
// for the same reason as PromptVerifiable: Agent.DetectBlock faces the same
// wrap seam, and a second normalizer written next to it would drift.
func NormalizeForVerify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if survivesNormalize(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// survivesNormalize is the single statement of what NormalizeForVerify keeps.
// PromptVerifiable is defined as "at least one rune survives", so both read
// the rule from here: adding a third stripped class must not silently stop
// the daemon's guard from covering it.
func survivesNormalize(r rune) bool { return !unicode.IsSpace(r) && !isBoxDrawing(r) }

// isBoxDrawing reports whether r is in the Box Drawing block. Note that the
// Block Elements that follow it (U+2580 and up) are NOT included: agent TUIs
// use those for solid banners, which do not appear inside the input area.
func isBoxDrawing(r rune) bool { return r >= 0x2500 && r <= 0x257F }

// PromptVerifiable reports whether SendPrompt can prove that the given prompt
// reached the pane. Verification works by finding the prompt's tail in the
// captured pane, and NormalizeForVerify discards whitespace and box-drawing
// runes — so a prompt built only from those normalizes to nothing, leaves no
// needle, and would be accepted without any evidence it landed.
//
// Callers that accept prompts from outside should reject the ones this rejects,
// rather than letting SendPrompt press Enter unverified.
func PromptVerifiable(prompt string) bool {
	// Scan for the first surviving rune rather than building the normalized
	// copy: the answer is a bool, and an ordinary prompt decides it on the
	// first character.
	for _, r := range prompt {
		if survivesNormalize(r) {
			return true
		}
	}
	return false
}

// promptTail returns the needle sendVerifyOK looks for: the prompt run
// through NormalizeForVerify, truncated to at most n bytes. Truncation
// backs off to a rune boundary so a multi-byte character is never cut in
// half — a byte-sliced tail cannot match the equally-normalized haystack
// once its leading fragment is no longer valid UTF-8.
func promptTail(prompt string, n int) string {
	s := NormalizeForVerify(prompt)
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// chunkPrompt splits s into consecutive pieces of at most max bytes, never
// cutting a rune in half. Concatenating the result reproduces s exactly.
// Returns nil for an empty string so the caller sends nothing.
//
// A rune longer than max is emitted whole rather than split: the chunk then
// exceeds max, which is still far below tmux's hard limit, and it guarantees
// forward progress for a test that shrinks the value.
func chunkPrompt(s string, max int) []string {
	if s == "" || max <= 0 {
		return nil
	}
	var out []string
	for len(s) > 0 {
		if len(s) <= max {
			out = append(out, s)
			break
		}
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			_, size := utf8.DecodeRuneInString(s)
			cut = size
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return out
}

// sendClearRepeats returns how many times the adapter's clear sequence must be
// repeated to wipe a residue of at most promptLen bytes. A retry can only ever
// face residue from what we sent, which makes the prompt an upper bound; the
// +4 covers the partial first row plus rounding.
//
// The per-row assumption comes from Claude Code, measured to drop exactly one
// visual row per press (72 characters off a 270-character input, 171 off a
// 1350-character one). Codex and OpenCode empty their input on the first press,
// so every repeat after that is a harmless no-op there, and one number keeps
// the caller free of per-adapter branching.
//
// The result is a repeat count for the whole sequence, not a keypress count.
func sendClearRepeats(promptLen int) int {
	return min(promptLen/sendClearWidthAssumed+4, sendClearMaxKeys)
}

// sendVerifyLookCount returns how many times one attempt re-checks the pane
// before re-sending. It scales with the prompt because each look sends one
// nudge, and on an agent whose input window follows the cursor those nudges
// are what walk the tail into view — a taller prompt needs more of them.
func sendVerifyLookCount(promptLen int) int {
	return min(sendVerifyLooksBase+promptLen/sendClearWidthAssumed, sendVerifyLooksMax)
}

// sendVerifyBudget returns how long the retry loop may run for a send of the
// given shape. A large prompt costs more per attempt — more chunks, more clear
// presses, more looks, each with a delay — so a flat timeout would silently
// reduce big sends to a single attempt, or cut the look loop short before the
// tail has been walked into view.
func sendVerifyBudget(chunks, clearPresses, looks int) time.Duration {
	return sendVerifyTimeoutBase +
		time.Duration(chunks)*sendVerifyPerChunk +
		time.Duration(clearPresses)*sendVerifyPerClearKey +
		time.Duration(looks)*sendVerifyLookDelay
}

// sendVerifyOK reports whether the pane's captured content shows that the
// prompt landed in the TUI's input area since the pre-send snapshot. It
// compares occurrence counts of promptTail(prompt) between before and after, so
// pane content that already carried the tail does not falsely satisfy it.
//
// A prompt that normalizes to nothing is treated as trivially accepted, because
// there is no needle to look for. That branch is only safe as long as callers
// reject those first — daemon.handleSend does, via PromptVerifiable.
func sendVerifyOK(before, after, prompt string) bool {
	return sendVerifyLanded(NormalizeForVerify(before), after,
		promptTail(prompt, sendVerifyTailBytes))
}

// sendVerifyLanded is sendVerifyOK with the invariant work hoisted out: the
// needle is fixed for the whole send and the baseline for the whole attempt,
// while only `after` changes between looks. SendPrompt normalizes `after`
// itself and calls sendVerifyAppeared, because normalizing a 32KB capture per
// needle per look would cost more than the capture it is checking.
func sendVerifyLanded(beforeNorm, after, tail string) bool {
	return sendVerifyAppeared(beforeNorm, NormalizeForVerify(after), tail)
}

// sendVerifyAppeared reports whether needle occurs more often in the pane now
// than it did at the baseline; both sides are already normalized. Comparing
// counts rather than testing presence is what stops pane content that already
// carried the needle from vouching for a prompt that never arrived.
func sendVerifyAppeared(beforeNorm, afterNorm, needle string) bool {
	if needle == "" {
		return true
	}
	nAfter := strings.Count(afterNorm, needle)
	if nAfter == 0 {
		return false
	}
	return nAfter > strings.Count(beforeNorm, needle)
}

// SendPrompt sends a prompt to a session's tmux pane. The session must be in
// idle status.
//
// tmux send-keys is fire-and-forget from tmux's point of view: it always
// reports success, even when the TUI has not finished its startup redraw and
// drops the incoming keys. SendPrompt therefore captures the pane before and
// after each attempt and checks that the tail of prompt appeared. Attempts
// repeat with backoff until the check passes or sendVerifyBudget elapses, and
// Enter is pressed only after verify succeeds, so a fully-dropped prompt is
// never committed.
//
// That guarantee does not invert. "Verify failed" does mean nothing was
// committed; "verify passed" does NOT mean the prompt will be. Verify sees only
// what the pane renders, and a rendered input line says nothing about what the
// TUI will do with the next keypress — an agent holding a completion overlay
// open consumes Enter to accept a candidate instead, rewriting the input and
// submitting nothing. Closing that overlay is the adapter's job
// (DismissOverlayKeys), and the step after it re-checks that the prompt
// survived. Beyond those two, a send that returns nil has delivered keystrokes
// and pressed Enter, not proven a turn began; callers that need the stronger
// fact poll for it (`send --wait-running`).
//
// One attempt is:
//
//	clear x sendClearRepeats -> capture before -> chunked literal sends
//	-> (nudge -> settle -> capture after -> verify) x sendVerifyLooks
//
// and once an attempt lands, before Enter:
//
//	dismiss overlay keys -> settle -> capture -> verify again
//
// The input-area clear is best-effort: an adapter returning no keys skips it
// and carries the residual-concat risk documented in docs/gotchas.md "Session
// send". Only an erroring SendKeys fails the send outright — that means the
// pane is in an unusable state.
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
	agentKind := sess.AgentKind
	m.mu.RUnlock()

	if paneID == "" {
		return fmt.Errorf("session has no tmux pane")
	}
	if m.tmuxClient == nil {
		return fmt.Errorf("tmux client not available")
	}

	// Ask the adapter once, up front, how it wants to be driven: neither
	// answer changes across the retry loop. A missing or misconfigured
	// resolver falls through to the defaults — no clearing, keystrokes —
	// because the transport layer must never refuse a send over it.
	var clearKeys, dismissKeys []string
	var placeholder string
	if ag := m.resolveAgent(agentKind); ag != nil {
		clearKeys, placeholder = ag.ClearInputKeys(), ag.PastePlaceholder(prompt)
		dismissKeys = ag.DismissOverlayKeys(prompt)
	}
	pasting := placeholder != ""

	// Chunking is the keystroke path's business; a paste hands the prompt
	// over whole. Leaving chunks nil on the paste path also keeps
	// sendVerifyBudget from charging a per-chunk cost that path never pays.
	var chunks []string
	if !pasting {
		chunks = chunkPrompt(prompt, sendChunkMaxBytes)
	}
	// Size the clear from the residue a retry can actually face. On the
	// keystroke path that is the whole prompt, typed out; on the paste path it
	// is at most one summary line, bounded by the fold threshold rather than by
	// the prompt. Sizing it from the prompt there is pure waste — a 64KB paste
	// would spend 512 tmux processes clearing a single row. Measured, that was
	// the bulk of the send: 1.12s against 114ms with the clamp, 64KB still 3/3.
	//
	// Known limit: this only covers residue a HUMAN left in the input. The
	// clamped count wipes ~500 typed characters but not ~5000 (measured on
	// OpenCode). The unclamped count was no principled defence against that
	// either — it scales with our prompt, which says nothing about what someone
	// typed before it.
	clearRepeats := sendClearRepeats(len(prompt))
	if pasting {
		clearRepeats = min(clearRepeats, sendClearRepeats(sendChunkMaxBytes))
	}
	looks := sendVerifyLookCount(len(prompt))
	// Fixed for the whole send: hoisted so the look loop does not re-normalize
	// them on every pass.
	tail := promptTail(prompt, sendVerifyTailBytes)
	foldNeedle := NormalizeForVerify(placeholder)
	budget := sendVerifyBudget(len(chunks), clearRepeats*len(clearKeys), looks)

	// The post-clear baseline the whole attempt compares against. Declared
	// out here because the overlay-dismiss step below runs after the loop
	// and re-checks against the same baseline and needles.
	var beforeNorm string
	// landedIn folds the two-needle comparison the paste path needs into one
	// call, so the look loop and the post-dismiss re-check cannot drift into
	// judging "did the prompt arrive?" by different rules.
	landedIn := func(afterNorm string) bool {
		if sendVerifyAppeared(beforeNorm, afterNorm, tail) {
			return true
		}
		return pasting && sendVerifyAppeared(beforeNorm, afterNorm, foldNeedle)
	}

	deadline := time.Now().Add(budget)
	attempts := 0
	for {
		attempts++

		// Clear residual input BEFORE the baseline capture so the "before"
		// snapshot is post-clear and sendVerifyOK's occurrence-count delta stays
		// clean. Fail fast on a SendKeys error: if a control key cannot be
		// pushed, the pane is unusable and nothing downstream will succeed.
		if len(clearKeys) > 0 {
			for i := 0; i < clearRepeats; i++ {
				for _, k := range clearKeys {
					if err := m.tmuxClient.SendKeys(paneID, k); err != nil {
						return fmt.Errorf("failed to send clear key %q: %w", k, err)
					}
				}
			}
			time.Sleep(sendClearSettleDelay)
		}

		before, err := m.tmuxClient.CapturePane(paneID, false)
		if err != nil {
			return fmt.Errorf("capture-pane before failed: %w", err)
		}
		beforeNorm = NormalizeForVerify(before)

		if pasting {
			// One atomic bracketed paste. No chunking, so the split-boundary
			// hazards this path was built around — the argv limit and a chunk
			// that starts with a dash — cannot arise at all.
			if err := m.tmuxClient.LoadBuffer(sendPasteBufferName, prompt); err != nil {
				return fmt.Errorf("failed to load paste buffer: %w", err)
			}
			if err := m.tmuxClient.PasteBuffer(paneID, sendPasteBufferName); err != nil {
				return fmt.Errorf("failed to paste prompt: %w", err)
			}
		} else {
			for i, c := range chunks {
				if i > 0 {
					time.Sleep(sendChunkDelay)
				}
				if err := m.tmuxClient.SendKeysLiteral(paneID, c); err != nil {
					return fmt.Errorf("failed to send prompt: %w", err)
				}
			}
		}

		// Look for the prompt, nudging before each look, WITHOUT re-sending.
		//
		// Re-sending is destructive: the next attempt clears the input area and
		// pushes the whole prompt again, throwing away whatever the TUI had
		// rendered. A TUI slower than one settle delay then never converges —
		// measured on Codex with a 16KB prompt, 16 failed attempts over the full
		// budget re-sending, against verified in 1.7s looking.
		//
		// The nudge is what makes looking worthwhile: Codex repaints only on key
		// events, and OpenCode's viewport does not follow the cursor on its own.
		landed := false
		for look := 0; look < looks; look++ {
			// A pasted prompt needs no nudge: the placeholder renders where
			// the input already is, and sending keys into a folded paste
			// would only risk disturbing it.
			if !pasting {
				if err := m.tmuxClient.SendKeys(paneID, sendNudgeKey); err != nil {
					return fmt.Errorf("failed to send nudge key %q: %w", sendNudgeKey, err)
				}
			}
			if look == 0 {
				time.Sleep(sendVerifySettleDelay)
			} else {
				time.Sleep(sendVerifyLookDelay)
			}

			after, err := m.tmuxClient.CapturePane(paneID, false)
			if err != nil {
				return fmt.Errorf("capture-pane after failed: %w", err)
			}
			// Normalize once, then try each needle. On the paste path the
			// prompt text is usually not on screen at all — the placeholder
			// stands in for it — but a small paste the TUI declined to fold
			// still shows its tail, so accept whichever appears rather than
			// depending on where that fold threshold sits.
			if landedIn(NormalizeForVerify(after)) {
				landed = true
				break
			}
			if time.Now().After(deadline) {
				break
			}
		}
		if landed {
			break
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"send verify: prompt did not appear in pane within %s (attempts=%d); "+
					"the TUI may not have been ready to receive input",
				budget, attempts)
		}
		time.Sleep(sendVerifyBackoff)
	}

	// Close any completion overlay the prompt opened, then prove the prompt is
	// still there.
	//
	// Verify above establishes that the text is rendered in the input area, not
	// that Enter will submit it: an agent whose completion overlay is open
	// consumes Enter to accept a candidate. Measured on Claude Code 2.1.224,
	// `list @internal/agent` was rewritten to `list @internal/agentdocs/` and
	// left unsent 3/3 — with or without the nudge, so the nudge is not the cause.
	//
	// The re-check is what makes sending a key here safe rather than hopeful:
	// only the adapter decides whether keys go out, and if the prompt is gone
	// afterwards, Enter is not pressed at all.
	if len(dismissKeys) > 0 {
		for _, k := range dismissKeys {
			if err := m.tmuxClient.SendKeys(paneID, k); err != nil {
				return fmt.Errorf("failed to send overlay-dismiss key %q: %w", k, err)
			}
		}
		time.Sleep(sendDismissSettleDelay)

		after, err := m.tmuxClient.CapturePane(paneID, false)
		if err != nil {
			return fmt.Errorf("capture-pane after overlay dismiss failed: %w", err)
		}
		if !landedIn(NormalizeForVerify(after)) {
			return fmt.Errorf(
				"send verify: the prompt left the input area after the overlay-dismiss keys %v; "+
					"Enter was not pressed, so nothing was committed", dismissKeys)
		}
	}

	if err := m.tmuxClient.SendKeys(paneID, "Enter"); err != nil {
		return fmt.Errorf("failed to send Enter: %w", err)
	}
	return nil
}

// RespondToBlock answers a blocking prompt the session's agent is showing — a
// tool-approval dialog, or a question — and returns the kind it answered.
//
// This is a different verb from SendPrompt because it drives a different thing,
// not because the gate is looser. SendPrompt types into an input line and
// proves it arrived by finding it there; on a dialog there is nothing to find.
// Measured on Claude Code 2.1.226, typed prose is neither drawn nor buffered
// (3/3), while SendPrompt's own nudge key walks the dialog's selection (3/3).
//
// The post-condition here is that the block LEFT the pane, which is why only
// kinds reporting Answerable() are driven: a form of several questions stays
// standing after one answer, so "the block is gone" could not tell a
// half-filled form from an answer that never landed. The gate and the
// post-condition are one design, not two.
//
// Status is deliberately not the gate. The hook that turns a session
// `permission` arrives about six seconds after the agent blocks, so a caller
// that answers promptly never sees that status at all (n=9). The pane is the
// authority, and asking it costs nothing: on BlockNone no key is sent.
func (m *Manager) RespondToBlock(id string, ans BlockAnswer) (BlockKind, error) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return BlockNone, fmt.Errorf("session not found: %s", id)
	}
	status := sess.Status
	paneID := sess.TmuxPaneID
	agentKind := sess.AgentKind
	m.mu.RUnlock()

	// The only statuses refused outright are the ones with no pane worth looking
	// at. Everything else — including idle — falls through to the pane, because a
	// session can sit at idle with a dialog up: recovery derives idle without
	// consulting the screen. Allowing it costs nothing, since DetectBlock then
	// reports BlockNone and this returns before sending a key.
	switch status {
	case StatusStopped, StatusCreating, StatusDeleting:
		return BlockNone, fmt.Errorf("session is %s, so there is no prompt to answer", status)
	}

	if paneID == "" {
		return BlockNone, fmt.Errorf("session has no tmux pane")
	}
	if m.tmuxClient == nil {
		return BlockNone, fmt.Errorf("tmux client not available")
	}

	// Unlike SendPrompt, a missing adapter is fatal here. SendPrompt falls
	// back to defaults because every capability it reads off the adapter has
	// a safe one and the transport must never refuse a send. There is no safe
	// default answer to a dialog: which key means "approve" is precisely the
	// agent-specific knowledge this call needs.
	ag := m.resolveAgent(agentKind)
	if ag == nil {
		return BlockNone, fmt.Errorf("no adapter for agent kind %q, so jin cannot tell "+
			"what keys its prompts take; attach the session and answer it directly", agentKind)
	}

	capture, err := m.tmuxClient.CapturePane(paneID, false)
	if err != nil {
		return BlockNone, fmt.Errorf("capture-pane failed: %w", err)
	}
	kind := ag.DetectBlock(capture)

	// Ask the adapter to plan the answer even for kinds it cannot drive: the
	// error it returns IS the message the caller gets, and only the adapter
	// knows which screen this is and therefore what to do about it. Manager
	// classifying it here would flatten several different situations into one
	// sentence.
	steps, err := ag.AnswerBlockKeys(kind, capture, ans)
	if err != nil {
		return kind, err
	}
	if !kind.Answerable() {
		// An adapter that plans keys for a kind Manager will not drive is a
		// bug in the adapter, and a silent one: the keys would look right in
		// its unit tests. Refuse rather than run them.
		return kind, fmt.Errorf("internal: adapter planned keys for %q, which jin does not drive", kind)
	}
	if len(steps) == 0 {
		return kind, fmt.Errorf("internal: adapter planned no keys for %q", kind)
	}

	// Validate the whole plan before any of it runs. A malformed step found
	// halfway through would be found with keys already in the pane, and the
	// error would then have to describe a dialog in an unknown state — so the
	// checks that can be made from the plan alone are made here, where a
	// refusal still means nothing was typed.
	for _, step := range steps {
		switch {
		case step.Key != "" && step.Literal != "":
			// KeyStep documents these as exclusive. Picking one silently
			// would drop the other, and on this path the dropped half is
			// usually the answer itself.
			return kind, fmt.Errorf("internal: adapter planned a step with both a key (%q) and text for %q",
				step.Key, kind)
		case step.Key == "" && step.Literal == "":
			return kind, fmt.Errorf("internal: adapter planned an empty key step for %q", kind)
		}
		// A Verify step whose text normalizes to nothing leaves no needle,
		// and sendVerifyAppeared accepts an empty needle unconditionally — so
		// the check would pass without evidence and the committing steps
		// after it would run on it. This is PromptVerifiable's rule applied
		// to the adapter's own plan rather than to the caller's input.
		if step.Verify && promptTail(step.Literal, sendVerifyTailBytes) == "" {
			return kind, fmt.Errorf("internal: adapter asked to verify a step with nothing to look for")
		}
	}

	for _, step := range steps {
		// A Verify step is checked against a baseline taken immediately
		// before it, so what the check reports is what THIS step drew rather
		// than something already on screen.
		var beforeNorm string
		if step.Verify {
			before, err := m.tmuxClient.CapturePane(paneID, false)
			if err != nil {
				return kind, fmt.Errorf("capture-pane before %q failed: %w", step.Literal, err)
			}
			beforeNorm = NormalizeForVerify(before)
		}

		switch {
		case step.Key != "":
			if err := m.tmuxClient.SendKeys(paneID, step.Key); err != nil {
				return kind, fmt.Errorf("failed to send key %q: %w", step.Key, err)
			}
		default:
			if err := m.tmuxClient.SendKeysLiteral(paneID, step.Literal); err != nil {
				return kind, fmt.Errorf("failed to send %q: %w", step.Literal, err)
			}
		}

		if !step.Verify {
			continue
		}
		// Reuse SendPrompt's comparison rather than writing a second one, so
		// "the text appeared" cannot come to mean two things in one package.
		needle := promptTail(step.Literal, sendVerifyTailBytes)
		landed := false
		for look := 0; look < respondVerifyLooks; look++ {
			time.Sleep(respondClearPollDelay)
			after, err := m.tmuxClient.CapturePane(paneID, false)
			if err != nil {
				return kind, fmt.Errorf("capture-pane after %q failed: %w", step.Literal, err)
			}
			if sendVerifyAppeared(beforeNorm, NormalizeForVerify(after), needle) {
				landed = true
				break
			}
		}
		if !landed {
			// Abandoning here is the point of the flag: the steps left
			// unsent are the ones that commit, so stopping means nothing
			// was answered.
			//
			// The message deliberately does NOT invite a retry. The text
			// was typed and only its appearance could not be confirmed, so
			// a second attempt's tail could verify against a field holding
			// both answers run together.
			return kind, fmt.Errorf(
				"the answer text was typed but never appeared in the pane, so the keys that " +
					"would submit it were not sent. The prompt is still waiting, and its " +
					"free-text field may already hold part of the answer — attach the session " +
					"and look rather than answering again")
		}
	}

	// The answer is only taken once the dialog is gone. Detection and
	// clearance run through the same DetectBlock for the reason SendPrompt
	// folds its two landing checks into one closure: two rules for the same
	// question drift, and this one decides whether jin reports success.
	deadline := time.Now().Add(respondClearBudget)
	for {
		after, err := m.tmuxClient.CapturePane(paneID, false)
		if err != nil {
			return kind, fmt.Errorf("capture-pane after answering failed: %w", err)
		}
		if ag.DetectBlock(after) == BlockNone {
			return kind, nil
		}
		if time.Now().After(deadline) {
			return kind, fmt.Errorf("%w after %s; the keys went out, so whether the agent "+
				"took them is unknown — attach the session and look before answering again",
				ErrBlockNotCleared, respondClearBudget)
		}
		time.Sleep(respondClearPollDelay)
	}
}

// resolveAgent returns the adapter for kind, or nil when the resolver is not
// installed or the kind is unknown.
//
// Errors are logged and swallowed rather than returned: every capability read
// off the adapter has a safe default, so a misconfigured resolver must degrade
// the send rather than refuse it.
func (m *Manager) resolveAgent(kind string) Agent {
	if m.agentResolver == nil {
		return nil
	}
	ag, err := m.agentResolver.Resolve(kind)
	if err != nil {
		debugLog("[SEND] resolveAgent: agent %q unknown: %v", kind, err)
		return nil
	}
	return ag
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
		// The popup is a process this daemon started, so it is told the same
		// three things every other one is, plus the session it was opened over.
		// Handed in from the identity this Manager was built with, never
		// rebuilt here — see EstablishHookBinary for what a second derivation
		// gets wrong. TmuxEnviron has why every key is emitted even when empty.
		Env: m.identity.TmuxEnviron(id),
	})
}

// PaneSplit splits the session's pane in its working directory and returns the
// new pane's ID. With name set the split becomes idempotent: when a pane with
// that name already exists in the session's window, the existing pane is
// returned as-is (noop), respawned with opts.Cmd (respawn), or reported as an
// error (error), per ifExists. The daemon handler validates name/ifExists/opts.
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
	// Overwritten, not merged: the working directory above is injected the same
	// way, and what a caller asked to run in the pane does not get to decide
	// which jin that pane calls back into. Reaches the respawn branch of a
	// named slot too, so a restarted slot is told the same thing a fresh one is.
	opts.Env = m.identity.TmuxEnviron(id)

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
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	session.Status = status
	session.ErrorMessage = errMsg
	// Save a copy outside the lock: Store.Save hits the filesystem and
	// marshals every field, so neither holding m.mu across it nor handing it
	// the live pointer is safe (see TryUpgradeDescription).
	saved := *session
	m.mu.Unlock()
	_ = m.store.Save(saved)
}

// workDirConflictLocked returns the session already claiming workDir, or nil.
// Sessions whose CurrentWorkDir is inside a Claude worktree have "moved away"
// from their persisted WorkDir and do not block it; excludeID exempts the
// session being edited ("" excludes nothing). Pure string checks, so safe to
// run under the lock. Caller must hold m.mu.
func (m *Manager) workDirConflictLocked(workDir, excludeID string) *Session {
	for _, s := range m.sessions {
		if s.ID != excludeID && s.WorkDir == workDir && !git.IsClaudeWorktreePath(s.CurrentWorkDir) {
			return s
		}
	}
	return nil
}

// SetWorkDir updates the work directory of a session
// Returns error if the workDir is already in use by another session
func (m *Manager) SetWorkDir(id string, workDir string) error {
	m.mu.Lock()

	// Duplicate check (prevents conflicts in async mode)
	if workDir != "" {
		if s := m.workDirConflictLocked(workDir, id); s != nil {
			m.mu.Unlock()
			return fmt.Errorf("WorkDir already in use by session %s", s.Description)
		}
	}

	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	session.WorkDir = workDir
	// Persist a copy outside the lock (see SetStatusWithError).
	saved := *session
	m.mu.Unlock()
	_ = m.store.Save(saved)
	return nil
}

// SetDescription updates a session's description. An empty (or whitespace-only)
// value clears the manual lock and regenerates the Layer A baseline from the
// session's WorkDir, so subsequent Layer C upgrades can take over again.
//
// The baseline is regenerated with the same arguments CreateWithOptions and
// TryUpgradeDescription use, keeping all three call sites' notion of "the
// baseline" byte-identical; drift would silently block Layer C after unlock.
func (m *Manager) SetDescription(id string, desc string) error {
	desc = strings.TrimSpace(desc)

	// The baseline depends only on WorkDir, and GenerateBaselineDescription
	// walks the filesystem (os.Lstat) — snapshot WorkDir first so the walk
	// runs outside m.mu.
	var baseline, baselineWorkDir string
	if desc == "" {
		m.mu.RLock()
		session, ok := m.sessions[id]
		if !ok {
			m.mu.RUnlock()
			return fmt.Errorf("session %s not found", id)
		}
		baselineWorkDir = session.WorkDir
		m.mu.RUnlock()
		baseline = GenerateBaselineDescription(baselineWorkDir, "", false, "")
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}

	if desc == "" {
		if session.WorkDir != baselineWorkDir {
			// WorkDir moved during the unlocked walk. Writing the stale
			// baseline would disagree with the one TryUpgradeDescription
			// derives from the current WorkDir, whose drift guard would then
			// block Layer C forever. This is a user-initiated clear, so
			// recompute under the lock (~21µs) rather than drop the request.
			baseline = GenerateBaselineDescription(session.WorkDir, "", false, "")
		}
		session.Description = baseline
		session.DescriptionLocked = false
		session.DescriptionLayer = DescriptionLayerBaseline
	} else {
		session.Description = desc
		session.DescriptionLocked = true
	}
	// Persist a copy outside the lock (see SetStatusWithError).
	saved := *session
	m.mu.Unlock()
	return m.store.Save(saved)
}

// TryUpgradeDescription asks the given enhancer for a Layer C description and
// applies it when two layer guards allow the write. Callers should invoke it
// from every hook event that might carry new signal; the guards keep repeated
// calls cheap.
//
// Guard 1 (restart protection) is Session.descriptionDriftedFrom: refuse to
// overwrite a Description a previous daemon process already upgraded.
//
// Guard 2 (monotonic layer): reject a candidate whose layer is not strictly
// greater than the session's current one. That lets the same enhancer run on
// SessionStart (transcript miss) and again on UserPromptSubmit (transcript hit)
// without the second call being rejected, while still stopping a same- or
// lower-layer proposal from clobbering a better value.
//
// A nil enhancer, an unknown id, or a locked description is a silent no-op.
//
// The enhancer scans the transcript end to end and the store write hits the
// filesystem, so neither runs under m.mu; only the snapshot and the commit take
// it. The session can change in that gap, so commitDescriptionUpgrade
// re-evaluates every guard against live state before writing.
func (m *Manager) TryUpgradeDescription(id string, enhancer DescriptionEnhancer) {
	if enhancer == nil {
		return
	}

	snapshot, ok := m.snapshotForUpgrade(id)
	if !ok {
		return
	}

	// Baselines must be computed with the same arguments CreateWithOptions and
	// SetDescription use. Threading CurrentBranch / IsWorktree / TmuxWindowName
	// here would make the comparison miss as soon as captureOutputTmux populates
	// those runtime fields, silently disabling Layer C on the very first poll.
	baselines := baselineDescriptions(snapshot.WorkDir)

	// Guard 1, evaluated against the snapshot purely to skip the transcript
	// scan. commitDescriptionUpgrade runs the authoritative one.
	if snapshot.descriptionDriftedFrom(baselines) {
		return
	}

	candidate, layer, ok := enhancer.TryGenerate(&snapshot)
	if !ok || candidate == "" {
		return
	}

	saved, ok := m.commitDescriptionUpgrade(id, &snapshot, baselines, candidate, layer)
	if !ok {
		return
	}
	// Save the copy rather than the live session: Store.Save marshals every
	// field, so handing it the live pointer outside the lock would race with
	// concurrent mutators. The copy can persist a Status that a concurrent Save
	// has already superseded; that is accepted, since memory stays
	// authoritative and the next Save reconverges.
	_ = m.store.Save(saved)
}

// snapshotForUpgrade returns an independent copy of the session to hand to an
// enhancer running without the lock, or ok=false when the session is unknown or
// its description is user-locked. The copy is safe because no Session field
// aliases mutable state.
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
// field, is what lets a write that landed during the unlocked window win.
func (m *Manager) commitDescriptionUpgrade(id string, snapshot *Session, baselines []string, candidate string, layer DescriptionLayer) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok || session.DescriptionLocked {
		return Session{}, false
	}

	// baselines describe snapshot.WorkDir. Once the session moves they say
	// nothing about the session in front of us, so drop the round rather than
	// compare against stale values; the next hook recomputes both. A miss only
	// costs one skipped round.
	if session.WorkDir != snapshot.WorkDir {
		return Session{}, false
	}

	// Guard 1, authoritative.
	if session.descriptionDriftedFrom(baselines) {
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
	// Lazy tmux init + recovery runs before we take the lock: recovery
	// manages its own locking in phases, and may find this very session's
	// pane still alive — the isProcessRunning check below sees its result.
	m.ensureTmuxClient()

	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}

	if isProcessRunning(session) {
		return nil // Already running
	}

	return m.startSessionTmux(session)
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

// paneMonitorInterval is how often captureOutputTmux looks at a session's pane.
const paneMonitorInterval = 10 * time.Second

// quickResumeFailWindow bounds "how long after startup does a pane death
// still count as a resume failure worth retrying with a fresh session id".
//
// It is expressed against paneMonitorInterval because it has to outlast it:
// the monitor's first look at a pane lands one whole interval after StartedAt,
// so a window equal to the interval is already shut when the only observation
// that could open it arrives. Measured at 10.034s against a 10s window, 2 of 2.
const quickResumeFailWindow = paneMonitorInterval + 5*time.Second

// spawnSnapshot is a value-typed snapshot of the session fields
// buildAgentShellCmd needs. Callers copy the fields they care about while
// holding m.mu, then release the lock before calling the builder — this
// makes buildAgentShellCmd safe to run concurrently with HandleHookEvent /
// List / Get, which mutate the source session under lock.
type spawnSnapshot struct {
	JinSessionID            string
	AgentKind               string
	AgentSessionID          string
	AgentSessionStarted     bool
	AgentSessionIDConfirmed bool
	Model                   string
	StartDir                string // pre-tmux shell workdir (may be ~-prefixed)
	ExpandedWorkDir         string // absolute, ~-expanded workdir handed to Setup()
}

// snapshotForSpawn takes the fields buildAgentShellCmd depends on. Callers
// must hold m.mu — the read must be atomic with respect to the field
// writes the daemon performs elsewhere.
func snapshotForSpawn(session *Session, startDir, expandedWorkDir string) spawnSnapshot {
	return spawnSnapshot{
		JinSessionID:            session.ID,
		AgentKind:               session.AgentKind,
		AgentSessionID:          session.AgentSessionID,
		AgentSessionStarted:     session.AgentSessionStarted,
		AgentSessionIDConfirmed: session.AgentSessionIDConfirmed,
		Model:                   session.Model,
		StartDir:                startDir,
		ExpandedWorkDir:         expandedWorkDir,
	}
}

// buildAgentShellCmd wraps the adapter's SpawnPlan in the fixed shell template
// Manager uses everywhere it spawns an agent (start and quick-fail retry).
// Centralising the assembly keeps the two call sites in lock-step on env vars,
// shell escaping, and the Setup() invariant — Setup runs here, with this
// spawn's SetupContext, immediately before SpawnCommand, on both paths alike
// because neither reaches an adapter except through this function.
//
// This is also the only place that resolves the two paths every adapter builds
// its command from (StateDir, ExecPath), and it puts them on both structs. A
// third path that reached an adapter without coming through here would not get
// a stale command — it would get an empty one: all three adapters fail open on
// absent paths, so the session would start with no --settings, no
// OPENCODE_CONFIG_DIR and no hook injection, and every adapter's own tests
// would still pass.
//
// Pure builder: reads only the immutable snapshot and performs NO Session
// writes. Callers own the "started once" invariant, and the read side too —
// the value-typed snapshot is what lets captureOutputTmux's retry path call
// this after m.mu.Unlock() without racing HandleHookEvent's writes.
func (m *Manager) buildAgentShellCmd(snap spawnSnapshot) (shellCmd string, resumed bool, err error) {
	if m.agentResolver == nil {
		return "", false, fmt.Errorf("agent resolver not configured")
	}
	ag, err := m.agentResolver.Resolve(snap.AgentKind)
	if err != nil {
		return "", false, fmt.Errorf("resolve agent %q: %w", snap.AgentKind, err)
	}

	// Passing the identity's binary to every adapter's Setup is what makes the
	// path baked into hook wiring survive the launch binary being rebuilt or
	// deleted — see EstablishHookBinary.
	if err := ag.Setup(SetupContext{
		StateDir: m.stateDir,
		ExecPath: m.identity.BinPath,
		WorkDir:  snap.ExpandedWorkDir,
	}); err != nil {
		debugLog("[AGENT] Setup returned error: %v", err)
	}

	// The same two paths Setup just received. SpawnCommand derives what it
	// emits from them rather than from anything the adapter kept, which is
	// what lets no adapter carry state from one call to the other — see
	// SpawnOptions.StateDir.
	plan := ag.SpawnCommand(SpawnOptions{
		StateDir:                m.stateDir,
		ExecPath:                m.identity.BinPath,
		JinSessionID:            snap.JinSessionID,
		AgentSessionID:          snap.AgentSessionID,
		AgentSessionStarted:     snap.AgentSessionStarted,
		AgentSessionIDConfirmed: snap.AgentSessionIDConfirmed,
		WorkDir:                 snap.ExpandedWorkDir,
		Model:                   snap.Model,
		CustomEnv:               m.configMgr.GetEnv(),
	})

	debugLog("[AGENT] Session %s spawning %s (resuming=%v, id confirmed=%v)",
		snap.JinSessionID, snap.AgentKind, plan.Resumed, snap.AgentSessionIDConfirmed)

	shellDir := workDirForShell(snap.StartDir)
	customEnv := buildEnvString(m.configMgr.GetEnv())
	envVars := "TERM=xterm-256color COLORTERM=truecolor FORCE_COLOR=1"
	// The jin that started this agent, for the same reason JIN_SESSION_ID rides
	// along: the process this launches will run `jin hook`, and that hook decides
	// which daemon to notify and whether to record what it was handed. None of it
	// carries across on its own — jinenv.Identity has the mechanism.
	//
	// Both halves were measured. Starting the daemon under the flag used to turn
	// on only half the exchange: the daemon logged the transitions it chose while
	// the hook side stayed silent, losing the half that says why — the stop_reason
	// behind a failed turn, the notification_type that separates a permission
	// prompt from an idle timer — and a hook that changed nothing left no trace at
	// all. And an agent whose pane came from a tmux server the daemon did not fork
	// reached either no daemon (3/3: hook exits 0, status never moves) or an older
	// daemon's socket still held by that server.
	//
	// Each assignment is quoted whole, which `env` reads the same as quoting only
	// the value: these are paths, and a path may contain a space. It is written
	// before customEnv so a user who names one of these in their own config still
	// wins — `env` applies assignments left to right.
	//
	// TmuxEnviron rather than Environ — its doc has why a key this prefix omits is
	// not absent from the agent but whatever the tmux server holds. The identity
	// travels inside this string rather than as tmux -e because three verbs consume
	// the string and only the string reaches all three; the prefix is needed
	// regardless, since -e has no unset form and `env -u TMUX` has nowhere to go.
	for _, kv := range m.identity.TmuxEnviron(snap.JinSessionID) {
		envVars += " " + shellEscape(kv)
	}
	if customEnv != "" {
		envVars += " " + customEnv
	}
	for k, v := range plan.ExtraEnv {
		// Keys go through the same env-name validation as UnsetEnv; the
		// value is single-quoted so any adapter output survives the outer
		// -ic 'cmd' wrapping.
		if !validEnvKeyPattern.MatchString(k) {
			return "", false, fmt.Errorf("agent %q returned invalid ExtraEnv key %q", snap.AgentKind, k)
		}
		envVars += fmt.Sprintf(" %s=%s", k, shellEscape(v))
	}
	unsetFlags := " -u TMUX -u TMUX_PANE"
	for _, k := range plan.UnsetEnv {
		if !validEnvKeyPattern.MatchString(k) {
			return "", false, fmt.Errorf("agent %q returned invalid UnsetEnv name %q", snap.AgentKind, k)
		}
		unsetFlags += " -u " + k
	}
	// plan.Command is spliced verbatim into `-ic '...'`. Per the SpawnPlan
	// doc comment, adapters emit the raw command and Manager defensively
	// escapes any single quote that slipped through — so a malformed
	// adapter can't break out of the wrapper into the parent shell.
	safeCmd := strings.ReplaceAll(plan.Command, "'", `'\''`)
	shellCmd = fmt.Sprintf("cd \"%s\" 2>/dev/null; env%s %s %s -ic '%s'",
		shellDir, unsetFlags, envVars, m.configMgr.GetShell(), safeCmd)
	return shellCmd, plan.Resumed, nil
}

// startSessionTmux starts a session in a tmux window.
func (m *Manager) startSessionTmux(session *Session) error {
	// Resume in the last known cwd (e.g. worktree) when available, so the session
	// lands where it was when it stopped. If it never moved out of WorkDir,
	// CurrentWorkDir is empty and WorkDir is used. We do NOT silently fall back
	// from a missing CurrentWorkDir to WorkDir: a session bound to a worktree
	// cannot be resumed at the project root once the worktree is gone — fail
	// loudly so the user can delete or recreate the session.
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
	// safe: startSessionTmux runs under StartBackground's m.mu.Lock(), so no
	// other goroutine can mutate the session under us.
	shellCmd, resumed, err := m.buildAgentShellCmd(snapshotForSpawn(session, startDir, expandedWorkDir))
	if err != nil {
		return err
	}
	session.spawnResumed = resumed

	// Commit the "started once" invariant: from this point a subsequent
	// resume must take the --resume branch even if SessionStart never fires
	// (crashes on start, no hook binary, etc.). Same lock context as the
	// snapshot above.
	session.AgentSessionStarted = true

	innerSessionName := tmux.InnerSessionName(session.ID)

	// Try to revive CC in existing inner tmux session (preserves user panes)
	if session.TmuxWindowName != "" && m.tmuxClient.HasSession(session.TmuxWindowName) {
		target := session.TmuxPaneID
		if err := m.tmuxClient.RespawnPane(target, shellCmd, nil); err == nil {
			session.Status = StatusRunning
			session.LastOutputTime = time.Now()
			session.StartedAt = time.Now()
			// Saved under the still-held lock rather than via snapshotAndUnlock:
			// the whole function runs under StartBackground's lock (see the
			// comment above), so there is no unlock/relock window for a
			// concurrent mutator to race with. *session is just the dereference
			// Save's by-value signature requires.
			_ = m.store.Save(*session)
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

	// Persist inner session name. Saved under the still-held lock, same
	// reasoning as the RespawnPane branch above.
	_ = m.store.Save(*session)

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
		// One Lstat per level walked up to the repo root, plus an Lstat and a
		// ReadFile for the worktree pointer — against a rev-parse fork/exec
		// already paid on this same tick. Recomputing unconditionally keeps this
		// branch free of state and follows the agent into a different repo.
		repoName := ResolveRepoName(currentPath)
		m.mu.Lock()
		session.CurrentBranch = branch
		session.IsGitRepo = true
		session.IsWorktree = isWorktree
		session.RepoName = repoName
		m.mu.Unlock()
	} else if currentPath != lastTrackedPath {
		// Only clear git info when entering a non-git directory
		m.mu.Lock()
		session.CurrentBranch = ""
		session.IsGitRepo = false
		session.IsWorktree = false
		session.RepoName = ""
		m.mu.Unlock()
	}
}

// isPersistableWorkDir reports whether path can become a session's persisted
// WorkDir: a git repo/worktree root (project root, not a subdirectory like
// .claude/workdir/) outside Claude Code's own worktree area.
//
// Stats the filesystem (git.IsGitRoot), so evaluate it before taking m.mu.
func isPersistableWorkDir(path string) bool {
	return path != "" && git.IsGitRoot(path) && !git.IsClaudeWorktreePath(path)
}

// applyCWDLocked records the agent's observed cwd on the session and promotes
// it to the persisted WorkDir when the caller determined the path is
// persistable (evaluate isPersistableWorkDir BEFORE taking the lock — it
// stats the filesystem). Returns whether WorkDir changed, i.e. whether the
// caller should persist the session. Caller must hold m.mu.
func applyCWDLocked(s *Session, cwd string, persistable bool) (workDirChanged bool) {
	s.CurrentWorkDir = cwd
	if persistable && s.WorkDir != cwd {
		s.WorkDir = cwd
		return true
	}
	return false
}

// snapshotAndUnlock takes a value copy of session and releases m.mu, so the
// copy — not the live pointer — is what a caller passes to Store.Save. Save
// marshals every field; handing it session after unlocking would let the
// marshal race with a concurrent mutator. Caller must hold m.mu on entry and
// must not read or write through session again until re-locking.
func (m *Manager) snapshotAndUnlock(session *Session) Session {
	saved := *session
	m.mu.Unlock()
	return saved
}

// paneDeathOutcome is what a dead agent pane means for the session behind it.
type paneDeathOutcome int

const (
	// paneDeathAlreadyStopped: someone already recorded a stop for this
	// session, so the dead pane is the aftermath of their work, not news.
	paneDeathAlreadyStopped paneDeathOutcome = iota
	// paneDeathQuickResumeRetry: the pane died soon after starting, having
	// asked for a resume, and exited non-zero; retry with a fresh agent session.
	paneDeathQuickResumeRetry
	// paneDeathRecordStop: an ordinary death; record the session as stopped.
	paneDeathRecordStop
)

// classifyPaneDeath decides what the monitor should do about a pane it has just
// found dead, given the status that pane's process exited with. Caller must
// hold m.mu.
//
// The already-stopped case has to be tested first, and that is why this is a
// three-way decision rather than the retry test alone: the monitor's status
// read happens a probe earlier than the pane check, so a kill landing in that
// gap arrives looking exactly like a crash — and inside quickResumeFailWindow
// that misreading would respawn an agent the user just asked to stop.
//
// The retry answers one question — did the resume this spawn asked for fail —
// and two things have to hold before a young death is read as that answer.
// spawnResumed, because a fresh spawn resumed nothing. And deadStatus, because
// Codex and opencode emit no session-end event: a user who quits inside the
// pane leaves a record a crash is indistinguishable from by anything else here.
// Quitting exits 0; a resume against an id the agent does not know exits
// non-zero (all three CLIs, measured — see "Codex adapter" in docs/gotchas.md).
// A process killed by a signal reads as a clean exit; tmux.PaneDeath says why.
func classifyPaneDeath(s *Session, now time.Time, deadStatus int) paneDeathOutcome {
	switch {
	case s.Status == StatusStopped:
		return paneDeathAlreadyStopped
	case s.spawnResumed && deadStatus != 0 && now.Sub(s.StartedAt) < quickResumeFailWindow:
		return paneDeathQuickResumeRetry
	default:
		return paneDeathRecordStop
	}
}

// handlePaneDeath resolves a pane the monitor has just found dead: exit quietly
// when someone else already recorded the stop, retry a resume that failed on
// startup, or record the stop itself. Reports whether the monitor should exit —
// false means the agent is back up and worth watching.
//
// Split out of captureOutputTmux's loop so the decision can be driven without
// waiting out the poll interval.
func (m *Manager) handlePaneDeath(session *Session, target, sessionName string, deadStatus int) (stop bool) {
	m.mu.Lock()
	// Exit without saving if session was already deleted
	if _, exists := m.sessions[session.ID]; !exists {
		m.mu.Unlock()
		debugLog("[TMUX] Session %s pane died but session already deleted, skipping save", sessionName)
		return true
	}

	outcome := classifyPaneDeath(session, time.Now(), deadStatus)
	if outcome == paneDeathAlreadyStopped {
		m.mu.Unlock()
		debugLog("[TMUX] Session %s pane dead with a stop already recorded, monitor exiting", sessionName)
		return true
	}

	// If the agent's --resume fails immediately (within quickResumeFailWindow
	// of startup), auto-restart with a fresh session ID by going back
	// through the adapter's SpawnCommand (this way agents without a
	// --resume concept still get sensible retry semantics).
	if outcome == paneDeathQuickResumeRetry {
		newSessionID := uuid.New().String()
		debugLog("[TMUX] Session %s pane died quickly (resume likely failed), retrying with a fresh agent session (dropping %s for %s)",
			session.Description, session.AgentSessionID, newSessionID)
		session.AgentSessionStarted = false
		session.AgentSessionID = newSessionID
		// No agent has reported this one yet, and the record is about to be
		// saved: leaving it set would let the next spawn resume an id nothing
		// has ever written.
		session.AgentSessionIDConfirmed = false
		session.spawnResumed = false
		// Snapshot every field buildAgentShellCmd needs BEFORE
		// releasing m.mu. Without this the retry runs the builder
		// with lock-free reads of session.WorkDir /
		// AgentSessionID / AgentSessionStarted, racing writes from
		// HandleHookEvent.
		retrySnap := snapshotForSpawn(session, session.WorkDir, expandTilde(session.WorkDir))
		// A Kill can land across the respawn below, and the check that spotted
		// none a moment ago cannot speak for the whole window.
		killSeqBefore := session.killSeq
		_ = m.store.Save(m.snapshotAndUnlock(session))

		respawned := false
		if shellCmd, _, buildErr := m.buildAgentShellCmd(retrySnap); buildErr != nil {
			debugLog("[TMUX] Session %s: cannot build retry cmd: %v", sessionName, buildErr)
		} else if err := m.tmuxClient.RespawnPane(target, shellCmd, nil); err != nil {
			debugLog("[TMUX] Session %s respawn failed after quick death", sessionName)
		} else {
			respawned = true
		}

		m.mu.Lock()
		if _, exists := m.sessions[session.ID]; !exists {
			m.mu.Unlock()
			return true
		}
		if respawned {
			// The retry may only publish an agent nobody stopped meanwhile:
			// a kill that landed during the respawn means the user asked for
			// this session to be down. The window name is deliberately
			// withheld from the undo — this path leaves the session's tmux
			// fields as they are, so tearing the inner session down here
			// would strand them pointing at nothing.
			if session.killSeq != killSeqBefore {
				tc := m.tmuxClient
				m.mu.Unlock()
				debugLog("[TMUX] Session %s killed during resume retry, stopping the agent it brought back", sessionName)
				stopAgentPane(tc, target, "")
				return true
			}
			session.Status = StatusRunning
			session.AgentSessionStarted = true
			session.StartedAt = time.Now()
			session.LastOutputTime = time.Now()
			_ = m.store.Save(m.snapshotAndUnlock(session))
			debugLog("[TMUX] Session %s restarted with fresh agent session (id: %s)", sessionName, newSessionID)
			return false
		}
		// Respawn did not happen; fall through to record the stop.
	}

	session.Status = StatusStopped
	session.LastActiveAt = time.Now()
	// Keep TmuxWindowName: window survives (remain-on-exit), only CC pane is dead.
	// RespawnPane can revive CC while preserving user panes in the same window.
	_ = m.store.Save(m.snapshotAndUnlock(session))
	debugLog("[TMUX] Session %s pane died, marked as stopped (window preserved)", sessionName)
	return true
}

// pollPaneDeath asks tmux about the pane and, when it is dead, resolves what
// that means. Split out so which status reaches classifyPaneDeath is a seam a
// test can drive: waiting out paneMonitorInterval is the only other way in.
func (m *Manager) pollPaneDeath(session *Session, target, sessionName string) (dead, stop bool) {
	dead, deadStatus := m.tmuxClient.PaneDeath(target)
	if !dead {
		return false, false
	}
	return true, m.handlePaneDeath(session, target, sessionName, deadStatus)
}

// captureOutputTmux polls a session's tmux pane every paneMonitorInterval: it
// detects pane death (retrying a resume that failed on startup), tracks the agent's
// working directory and git branch, and falls back to "idle" when no hook
// arrives after a fresh start. One goroutine per monitored session; it exits
// when the session stops or is deleted.
func (m *Manager) captureOutputTmux(session *Session) {
	ticker := time.NewTicker(paneMonitorInterval)
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
		if dead, stop := m.pollPaneDeath(session, target, sessionName); dead {
			if stop {
				return
			}
			continue
		}

		// Track current working directory and git branch
		if currentPath, err := m.tmuxClient.GetPaneCurrentPath(target); err == nil {
			currentPath = strings.TrimSpace(currentPath)
			if currentPath != "" {
				// isPersistableWorkDir stats the filesystem, so settle it
				// before taking the lock.
				persistable := isPersistableWorkDir(currentPath)
				m.mu.Lock()
				workDirChanged := applyCWDLocked(session, currentPath, persistable)
				saved := m.snapshotAndUnlock(session)
				if workDirChanged {
					_ = m.store.Save(saved)
					debugLog("[CWD] Session %s WorkDir updated to %s", sessionName, currentPath)
				}

				// updateGitBranch only touches CurrentBranch / IsGitRepo /
				// IsWorktree, all json:"-" — it never affects what the Save
				// above persisted, so running it after Save (rather than
				// before taking the copy) is safe.
				m.updateGitBranch(session, currentPath, lastTrackedPath)
				lastTrackedPath = currentPath
			}
		}

		// Fallback: a session that has been "running" since a fresh start with no
		// hook for hookIdleTimeout is assumed idle and waiting for input, which
		// covers agents that fire neither Stop nor idle_prompt during startup.
		// StartedAt is json:"-", so the !IsZero() guard keeps this from firing for
		// daemon-recovered sessions while their task is still running.
		m.mu.RLock()
		fbStatus := session.Status
		fbLastOutput := session.LastOutputTime
		fbStartedAt := session.StartedAt
		m.mu.RUnlock()

		const hookIdleTimeout = 30 * time.Second
		if fbStatus == StatusRunning && !fbStartedAt.IsZero() && time.Since(fbLastOutput) > hookIdleTimeout {
			m.mu.Lock()
			saved, changed := m.markIdleFallbackLocked(session.ID)
			m.mu.Unlock()
			// Only save when this goroutine actually made the transition: the
			// guard above can miss (session deleted, or another goroutine
			// already moved Status off Running) between the RLock snapshot and
			// this Lock. Saving unconditionally would resurrect a just-deleted
			// session's file.
			if changed {
				_ = m.store.Save(saved)
				debugLog("[POLL] Session %s: running -> idle (no hook received for %s, fallback)", saved.Description, hookIdleTimeout)
			}
		}
	}
}

// markIdleFallbackLocked applies captureOutputTmux's idle-fallback transition
// (Running with no hook for hookIdleTimeout -> Idle) if the session still
// qualifies, and returns a copy to persist plus whether it happened. It
// re-checks existence and Status against live state rather than trusting the
// caller's RLock snapshot: applying unconditionally would resurrect a
// just-deleted session's file. Caller must hold m.mu.
func (m *Manager) markIdleFallbackLocked(id string) (Session, bool) {
	session, exists := m.sessions[id]
	if !exists || session.Status != StatusRunning {
		return Session{}, false
	}
	session.Status = StatusIdle
	session.LastOutputTime = time.Now()
	return *session, true
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
//  2. update the adapter session id if the adapter has re-keyed it — and only
//     if the reported id passes both gates (see rejectAgentSessionID)
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
		// Both values are caller-chosen and neither has been gated yet — this
		// branch is reached before any of that. It is also the branch a payload
		// naming a session that does not exist always takes.
		debugLog("[HOOK] Unknown session: jin=%s agent=%s",
			debug.Untrusted(jinSessionID, maxAgentSessionIDLen), debug.Untrusted(agentSessionID, maxAgentSessionIDLen))
		return
	}

	m.mu.RLock()
	kind := session.AgentKind
	desc := session.Description
	m.mu.RUnlock()
	if m.agentResolver == nil {
		debugLog("[HOOK] Session %s: no agent resolver configured", desc)
		return
	}
	ag, err := m.agentResolver.Resolve(kind)
	if err != nil {
		debugLog("[HOOK] Session %s: cannot resolve agent %q: %v", desc, kind, err)
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

	// isPersistableWorkDir stats the filesystem, so settle it before taking the
	// lock. cwd comes from the hook payload, so this is a pure function of the
	// event.
	cwdPersistable := isPersistableWorkDir(cwd)

	// Settled here because rejectAgentSessionID calls into the adapter and this
	// function holds m.mu without a deferred unlock. An adapter whose predicate
	// blocked or panicked under the lock would not cost one event, it would wedge
	// the daemon — and adapters are an extension surface, so "it is a cheap pure
	// function today" is a property of the three that ship, not of the interface.
	//
	// Computed unconditionally rather than only for an id that differs from the
	// record: the comparison needs the lock, and taking it to decide whether to
	// do work outside it is the wrong way round.
	rekeyRefusal := rejectAgentSessionID(ag, agentSessionID)

	m.mu.Lock()
	oldStatus := session.Status
	sessionID := session.ID
	sessionName := session.Description

	// Update AgentSessionID if it changed (an adapter may re-key it, e.g. CC
	// assigns its own UUID when we started with an empty one). This write is the
	// one place a value from outside jind-ai becomes a session's identity, so it
	// is gated: the id arrives in a hook payload, and anything that can reach the
	// daemon socket or run `jin hook` with JIN_SESSION_ID set can choose it, for
	// any session, not only its own.
	//
	// A rejection drops the WRITE and nothing else. The event keeps its status
	// verdict, its CWD tracking and its SessionStart bookkeeping. Dropping the
	// whole event instead would hand the same payload a second power — send one
	// with a malformed id and status tracking stops.
	//
	// The agentSessionID != "" test is kept even though safeAgentSessionID
	// refuses "" on its own: it separates "the agent said nothing about its id"
	// from "the agent reported an id we would not take", and only the second is
	// worth a log line.
	if agentSessionID != "" {
		switch {
		case rekeyRefusal != "":
			if session.AgentSessionID != agentSessionID {
				debugLog("[HOOK] Session %s: refusing the reported agent session id (%s): %s",
					sessionName, rekeyRefusal, debug.Untrusted(agentSessionID, maxAgentSessionIDLen))
			}
		default:
			if session.AgentSessionID != agentSessionID {
				debugLog("[HOOK] Updating AgentSessionID for %s: %s -> %s", sessionName, session.AgentSessionID, agentSessionID)
				session.AgentSessionID = agentSessionID
			}
			// The agent named this id itself, which is the whole of what
			// AgentSessionIDConfirmed asserts — including when it names the one
			// jind-ai already holds, as an agent taking its id on the command
			// line does.
			session.AgentSessionIDConfirmed = true
		}
	}

	// Update CWD from the agent's actual working directory.
	// Only update persisted WorkDir when the new path is a git root
	// (project root or worktree root) to prevent drift to subdirectories.
	cwdChanged := false
	if cwd != "" {
		cwdChanged = applyCWDLocked(session, cwd, cwdPersistable)
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

	// The same observation applied to a second field: an agent that just
	// announced it started is not stopped, so SessionStart clears a stale stop —
	// and clears nothing else. (docs/gotchas.md, under Hook, has why a stale stop
	// is there to clear and what it costs while it lasts.)
	//
	// The narrowness is the point. SessionStart is not only fired at startup:
	// Claude Code raises it for resume, /clear and /compact too, and the generated
	// hooks file carries no matcher, so every one of them arrives here. A verdict
	// that set a status unconditionally would drop a session to idle in the middle
	// of a turn the moment an auto-compaction ran, opening SendPrompt's idle gate
	// on a working agent. "The process is alive" contradicts exactly one status,
	// so exactly one is touched.
	//
	// It is Manager's rather than an adapter's because the premise is process
	// liveness, which no vocabulary owns — and because Interpret runs before m.mu
	// is taken, so a verdict reasoning about the status may be acting on one that
	// is already gone. Reading and writing Status in one critical section is what
	// makes the rule safe.
	//
	// No plugin status_changed event fires for the correction: dispatch below is
	// gated on updOK, and a correction of a stale record is not something that
	// just happened.
	if eventName == "SessionStart" && session.Status == StatusStopped {
		session.Status = StatusIdle
	}

	// SessionEnd on an already-stopped session: no verdict fields should be
	// applied (they would mutate LastOutputTime / LastActiveAt in memory but
	// only persist on cwdChanged, which drops the change on daemon restart).
	// Take the early return before assigning anything from upd — mirrors the
	// pre-refactor SessionEnd branch that also short-circuited here.
	if updOK && upd.Status == StatusStopped && oldStatus == StatusStopped {
		saved := m.snapshotAndUnlock(session)
		if cwdChanged {
			_ = m.store.Save(saved)
			debugLog("[HOOK] Session %s: CWD updated to %s (SessionEnd, already stopped)", sessionName, cwd)
		}
		return
	}

	// A liveness verdict reports that the agent is alive, not that a turn began,
	// so it does not apply to a session sitting idle. It still applies everywhere
	// else: recovering a session from permission once the user answers, and
	// contradicting a stale stop, both run through it.
	//
	// The whole verdict is withheld, not just its Status. ClearError means "the
	// agent took a new turn", which is exactly the claim being rejected — and
	// since nothing downstream saves a record whose status did not move, clearing
	// the message here would drop it from memory while the session file kept it.
	//
	// Enforced here under m.mu rather than in the adapter, for the reason the
	// SessionStart correction above gives.
	//
	// What this gives up, deliberately: a turn whose UserPromptSubmit hook never
	// arrives is no longer rescued by the tool hooks that follow it, and reads
	// idle while it runs. Nothing here distinguishes which writer produced the
	// idle, so a recovery verdict's idle loses the rescue too. It stays unguarded
	// on purpose; the alternative is a threshold fitted to a handful of
	// observations. The measurements are in docs/gotchas.md ("Hook").
	suppressed := updOK && upd.Liveness && session.Status == StatusIdle

	// Fold in the adapter's status verdict. A missing verdict (updOK=false)
	// still lets us persist CWD / SessionStart changes, but leaves Status
	// alone. ErrorMessage uses the tri-state documented on StatusUpdate:
	// non-empty means set, ClearError means clear, both zero means leave.
	if updOK && !suppressed {
		session.Status = upd.Status
		if upd.ErrorMessage != "" {
			session.ErrorMessage = upd.ErrorMessage
		} else if upd.ClearError {
			session.ErrorMessage = ""
		}
		if upd.Status == StatusStopped {
			session.LastActiveAt = time.Now()
		}
	}

	// Separately, and whatever came of the verdict: a hook we could make sense
	// of is evidence the agent is alive, which is what the "no hook for 30s"
	// fallback in captureOutputTmux reads. That holds for the events we track
	// without a verdict, and for a verdict withheld above — the hook still
	// arrived.
	if updOK || eventName == "CwdChanged" || eventName == "SessionStart" {
		session.LastOutputTime = time.Now()
	}

	// saved is the single point-in-time snapshot the post-unlock code reads from
	// — for Store.Save and for the fields the plugin event below needs, since
	// reading session.* after Unlock would race with concurrent mutators.
	// updateGitBranch below only touches json:"-" fields, so saved not reflecting
	// its result does not affect what gets persisted.
	pluginDisp := m.pluginDisp
	saved := m.snapshotAndUnlock(session)

	// CwdChanged: immediately check git branch outside the lock
	if eventName == "CwdChanged" && cwd != "" {
		m.updateGitBranch(session, cwd, "")
	}

	// Leave a trace of the transition that did not happen. A status that
	// silently declines to move is exactly the kind of thing the next person
	// has to guess at from the outside, and this log is the difference between
	// measuring that and reasoning about it.
	if suppressed {
		debugLog("[HOOK] Session %s: stayed %s (hook: %s reports liveness, not a turn)", sessionName, saved.Status, eventName)
	}

	// Persist status/CWD/session-started changes
	if oldStatus != saved.Status || cwdChanged || sessionStarted {
		_ = m.store.Save(saved)
		if oldStatus != saved.Status {
			debugLog("[HOOK] Session %s: %s -> %s (hook: %s)", sessionName, oldStatus, saved.Status, eventName)
		}
		if cwdChanged {
			debugLog("[HOOK] Session %s: CWD updated to %s", sessionName, cwd)
		}
	}

	if pluginDisp != nil && updOK && oldStatus != saved.Status {
		pluginDisp.Publish(plugin.Event{
			Name:       manifest.EventStatusChanged,
			SessionID:  sessionID,
			Status:     string(saved.Status),
			PrevStatus: string(oldStatus),
			AgentKind:  kind,
			WorkDir:    saved.WorkDir,
			TmuxPaneID: saved.TmuxPaneID,
			NotifyKind: string(upd.Notify),
		})
	}

	// Layer C: opportunistically upgrade the description, on three events that
	// each expose a different signal source:
	//
	//   - SessionStart is the earliest hook; the transcript is still empty but the
	//     agent may already have written a session-name file (Claude Code 2.x
	//     populates ~/.claude/sessions/<PID>.json by then) — LayerAgentName.
	//   - UserPromptSubmit races Claude Code's transcript flush by ~10ms, so it
	//     sometimes still sees an empty jsonl but is the fastest chance at a
	//     LayerTranscript win.
	//   - Stop fires after the assistant response completes, by which point the
	//     transcript is guaranteed flushed. The reliable path to LayerTranscript.
	//
	// TryUpgradeDescription self-limits via the monotonic-layer guard, so calling
	// it on all three produces at most one write per layer per session.
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

const (
	// paneTerminatePoll / paneTerminateTries bound how long Kill waits for a
	// pane's process to go away after the hangup before falling back to kill-pane.
	// A pane observably dies within a few milliseconds of its direct child
	// exiting, so ten 50ms probes is generous for a slow machine while staying
	// short enough that a kill request never feels stalled.
	paneTerminatePoll  = 50 * time.Millisecond
	paneTerminateTries = 10
)

// waitPaneDead polls target until tmux reports the pane's process gone,
// bounded by paneTerminateTries. Reports whether it settled.
func waitPaneDead(tc tmux.Runner, target string) bool {
	for i := 0; i < paneTerminateTries; i++ {
		if i > 0 {
			time.Sleep(paneTerminatePoll)
		}
		if tc.IsPaneDead(target) {
			return true
		}
	}
	return false
}

// stopAgentPane stops the agent running in a session's tmux pane and reports
// whether the session's tmux references survived. Keeping them is what
// preserves the rest of the window: the inner tmux session stays up, every
// other pane in it keeps running, and the dead pane holds the agent's slot in
// the layout so a later start revives it in place via RespawnPane.
//
// True means TmuxPaneID / TmuxWindowName still address something real and the
// caller must keep them. False means the window is gone and both must be cleared.
//
// Takes no lock and touches no Session: the caller snapshots the fields under
// m.mu, runs this, and re-validates before applying the result.
func stopAgentPane(tc tmux.Runner, paneID, windowName string) (keepTmuxRefs bool) {
	switch {
	case tc == nil:
		// No way to observe tmux, so no grounds to declare the references
		// dead either. Leave them for a client that can check.
		return true
	case paneID != "":
		// A pane that already exited needs nothing done to it, and must not
		// be signalled: tmux keeps reporting the pid the pane started with
		// long after the process is gone (verified on 3.6a), and the OS is
		// free to have handed that number to something else entirely.
		if tc.IsPaneDead(paneID) {
			return true
		}
		if err := tc.TerminatePaneProcess(paneID); err == nil && waitPaneDead(tc, paneID) {
			return true
		}
		// The signal never landed (unreadable pid) or the process sat through it.
		// A kill that leaves the agent running is not a kill, so destroy the pane
		// outright. The window goes with it: kill-pane spares a window that still
		// has other panes, and the caller is about to forget its name, which would
		// leave the inner session with no owner to reclaim it.
		_ = tc.KillPane(paneID)
		if windowName != "" {
			_ = tc.KillSession(windowName)
		}
		return false
	case windowName != "":
		// No pane to aim at (pre-pane-ID record, or a start that failed
		// before GetPaneID): the inner session is the only handle there is.
		_ = tc.KillSession(windowName)
		return false
	}
	return false
}

// Kill stops a session's agent. The session's inner tmux window survives
// whenever the agent's pane can be stopped without destroying it, so kill is
// "stop the agent", not "tear the session's tmux state down" — that is delete's
// job. It matches what already happened when an agent died on its own, so a
// user-requested stop and a crash leave a session in the same shape.
//
// The tmux work runs between two lock sections rather than inside one: it
// signals a process and then waits on it, and holding m.mu across that would
// stall every List, hook and delete. The second section re-validates before
// writing, the same way applyRecovery does.
func (m *Manager) Kill(id string) error {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	if session.Status == StatusDeleting {
		// A delete is already in flight and subsumes this stop; it owns the
		// record's status until it finishes.
		m.mu.Unlock()
		return nil
	}
	tc := m.tmuxClient
	paneID := session.TmuxPaneID
	windowName := session.TmuxWindowName
	startedAt := session.StartedAt

	// Record the stop before dropping the lock, not after the tmux work: for the
	// length of that work the agent is on its way out, and a session that still
	// reads as running is one StartBackground would treat as needing no start at
	// all — a restart request silently doing nothing. killSeq goes up with it, so
	// anyone else who dropped m.mu can see that a kill landed.
	session.killSeq++
	killSeq := session.killSeq
	session.Status = StatusStopped
	m.mu.Unlock()

	keepTmuxRefs := stopAgentPane(tc, paneID, windowName)

	m.mu.Lock()
	session, ok = m.sessions[id]
	if !ok {
		// Deleted while we were stopping it; the delete owns the record now.
		m.mu.Unlock()
		return nil
	}
	// A start or delete that ran while the lock was down owns the record's
	// state, and finishing our write over theirs would either leave a running
	// session nobody is watching or reopen a delete's CAS. StartedAt is the
	// marker for a start: it reuses the same pane and inner session, so the
	// two names alone cannot tell a revived session from the one we stopped.
	if session.Status == StatusDeleting || session.killSeq != killSeq ||
		session.StartedAt != startedAt || session.TmuxPaneID != paneID || session.TmuxWindowName != windowName {
		desc := session.Description
		m.mu.Unlock()
		debugLog("[TMUX] Session %s changed hands during kill, leaving the newer state alone", desc)
		return nil
	}
	if !keepTmuxRefs {
		session.TmuxPaneID = ""
		session.TmuxWindowName = ""
	}

	// Re-assert the stop rather than trusting phase 1's: an agent being
	// terminated tends to fire one last hook on its way out, and
	// HandleHookEvent writes whatever status that hook maps to without
	// knowing a kill is in flight. Landing in this window, it would otherwise
	// leave a killed session reading as idle.
	session.Status = StatusStopped
	// Update LastActiveAt for persistence
	if !session.LastOutputTime.IsZero() {
		session.LastActiveAt = session.LastOutputTime
	} else {
		session.LastActiveAt = time.Now()
	}

	// Persist LastActiveAt
	_ = m.store.Save(m.snapshotAndUnlock(session))

	return nil
}

// DeleteRequest carries the resolved intent for a delete after PreCheckDelete
// has run its synchronous checks, so the async goroutine has everything it
// needs without re-taking the manager lock. Fields other than
// ID/RemoveWorktree/ForceRemoveWorktree are an opaque snapshot to callers.
type DeleteRequest struct {
	ID                  string
	RemoveWorktree      bool
	ForceRemoveWorktree bool
	// workDir is the resolved effective worktree path, computed under
	// PreCheckDelete via ResolveWorktreeDir. Empty when RemoveWorktree is
	// false or the session has no work directory.
	workDir string
	// tmuxWindowName snapshots the window under MarkDeleting's write lock so
	// the goroutine can KillSession without re-reading the record. Taken
	// there (not in PreCheckDelete) so a Kill/Start racing the pre-check
	// window does not hand DeleteFinalize a stale name.
	tmuxWindowName string
	// previousStatus is the Status the session held immediately before
	// MarkDeleting flipped it to StatusDeleting. MarkDeletionFailed uses
	// this to restore the pre-delete state on finalize failure — falling
	// back to Stopped would silently degrade a still-running session
	// (idle/thinking/permission) into "attach-broken" territory.
	previousStatus Status
}

// PreCheckDelete runs the synchronous checks a delete request must pass before
// the daemon can accept it and defer the rest to a background goroutine:
// session existence, worktree resolution, and (when the caller asked to remove
// the worktree without force) a dirty-tree probe.
//
// On success it returns a DeleteRequest carrying the resolved worktree path and
// tmux window name, ready for MarkDeleting + DeleteFinalize. On failure no
// state has been touched and the caller reports the error synchronously.
//
// The dirty probe runs synchronously on purpose: it costs a `git status
// --porcelain` on a checkout the user is asking to delete, and reporting dirty
// synchronously is what preserves the confirm-force UX in the TUI and the CLI.
func (m *Manager) PreCheckDelete(id string, removeWorktree, forceRemoveWorktree bool) (DeleteRequest, error) {
	// Defense-in-depth: the CLI validates the same combination, but non-CLI
	// callers (TUI, integration tests, future clients) reach Manager directly.
	if forceRemoveWorktree && !removeWorktree {
		return DeleteRequest{}, fmt.Errorf("forceRemoveWorktree requires removeWorktree")
	}

	m.mu.RLock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return DeleteRequest{}, fmt.Errorf("session %s not found", id)
	}
	// Reject in-flight duplicates up front. Without this, a second
	// PreCheckDelete would still resolve workDir and run `git status` on a
	// checkout the first request is already rm -rf'ing. MarkDeleting is where the
	// CAS actually lands; this is the pre-check version.
	if session.Status == StatusDeleting {
		m.mu.RUnlock()
		return DeleteRequest{}, ErrDeleteInFlight
	}
	req := DeleteRequest{
		ID:                  id,
		RemoveWorktree:      removeWorktree,
		ForceRemoveWorktree: forceRemoveWorktree,
		// previousStatus and tmuxWindowName are intentionally left zero
		// here — MarkDeleting snapshots them under the write lock so a
		// Status change or a Kill/Start racing this pre-check cannot make
		// them stale.
	}
	currentWorkDir := session.CurrentWorkDir
	persistedWorkDir := session.WorkDir
	m.mu.RUnlock()

	if !removeWorktree {
		return req, nil
	}

	// Resolve outside the lock: ResolveWorktreeDir performs os.Lstat probes.
	workDir := git.ResolveWorktreeDir(currentWorkDir, persistedWorkDir)
	req.workDir = workDir
	if workDir == "" {
		return req, nil
	}

	// Directory already gone (manual `rm -rf`, prior partial delete): skip the
	// worktree and dirty probes and let DeleteFinalize's removeGitWorktree
	// short-circuit on its own os.IsNotExist branch. A synchronous ErrNotWorktree
	// here would regress callers that reasonably expect delete-with-missing-
	// worktree to still drop the session.
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		return req, nil
	}

	if !git.IsGitWorktreeDir(workDir) {
		return DeleteRequest{}, ErrNotWorktree
	}

	if !forceRemoveWorktree {
		dirty, err := m.gitClient.IsDirty(workDir)
		if err != nil {
			// git failure ≠ dirty; fall through and let the actual
			// `git worktree remove` in DeleteFinalize surface the real
			// problem. Log so operators can trace an unexpected failure.
			debugLog("[DELETE] pre-check IsDirty(%s) failed: %v", workDir, err)
		} else if dirty {
			return DeleteRequest{}, ErrWorktreeDirty
		}
	}

	return req, nil
}

// ErrDeleteInFlight is returned by MarkDeleting when the session is already
// StatusDeleting — a prior accepted delete is still running its goroutine.
// The daemon handler surfaces this to reject the duplicate request instead
// of spawning a second DeleteFinalize goroutine that would race the first
// on `removeGitWorktree` / `KillSession` / `store.Delete`.
var ErrDeleteInFlight = errors.New("delete already in progress for this session")

// MarkDeleting flips a session's Status to StatusDeleting under the write lock
// and captures the pre-flip Status into req.previousStatus in the same critical
// section. It acts as a compare-and-set: ErrDeleteInFlight when the session is
// already StatusDeleting, so concurrent delete requests serialize on the flip.
//
// The atomicity matters: PreCheckDelete runs under a separate RLock and cannot
// own previousStatus reliably — a Status change between pre-check and flip would
// let a subsequent MarkDeletionFailed restore a stale value, in the extreme
// StatusDeleting itself, leaving the record permanently stuck.
//
// req.tmuxWindowName is refreshed here for the same reason.
func (m *Manager) MarkDeleting(req *DeleteRequest) error {
	m.mu.Lock()
	session, ok := m.sessions[req.ID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", req.ID)
	}
	if session.Status == StatusDeleting {
		m.mu.Unlock()
		return ErrDeleteInFlight
	}
	req.previousStatus = session.Status
	req.tmuxWindowName = session.TmuxWindowName
	session.Status = StatusDeleting
	saved := *session
	m.mu.Unlock()
	if err := m.store.Save(saved); err != nil {
		debugLog("[SESSION] MarkDeleting %s: persist failed: %v", req.ID, err)
	}
	return nil
}

// MarkDeletionFailed rolls back a MarkDeleting flip when DeleteFinalize errors:
// Status returns to the value req.previousStatus captured before the flip, so a
// pane-alive session stays attach-usable, and ErrorMessage records err. The
// record is preserved so the client sees the failure through `get`. Idempotent.
//
// The store.Save is fire-and-forget, same reasoning as MarkCreationFailed.
func (m *Manager) MarkDeletionFailed(req DeleteRequest, err error) {
	m.mu.Lock()
	session, ok := m.sessions[req.ID]
	if !ok {
		m.mu.Unlock()
		return
	}
	// Only rewrite Status when we are the one still holding the Deleting
	// flip — some other path (e.g. hook event, recovery) may have advanced
	// it while we were finalizing, and we do not want to clobber that.
	if session.Status == StatusDeleting {
		session.Status = req.previousStatus
	}
	if err != nil {
		session.ErrorMessage = err.Error()
	}
	saved := *session
	m.mu.Unlock()
	if saveErr := m.store.Save(saved); saveErr != nil {
		debugLog("[SESSION] MarkDeletionFailed %s: persist failed: %v", req.ID, saveErr)
	}
}

// DeleteFinalize runs the destructive tail of delete (worktree removal, tmux
// kill, store delete, map drop) using the resolved DeleteRequest from
// PreCheckDelete. It runs entirely outside the request/response window, so any
// failure is reported through MarkDeletionFailed rather than a return error
// that no one is waiting for.
func (m *Manager) DeleteFinalize(req DeleteRequest) error {
	if req.RemoveWorktree && req.workDir != "" {
		if err := m.removeGitWorktree(req.workDir, req.ForceRemoveWorktree); err != nil {
			return err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.tmuxClient != nil && req.tmuxWindowName != "" {
		_ = m.tmuxClient.KillSession(req.tmuxWindowName)
	}

	if err := m.store.Delete(req.ID); err != nil {
		return err
	}

	delete(m.sessions, req.ID)
	return nil
}

// Delete removes a session completely, synchronously. It is a thin composition
// of PreCheckDelete + MarkDeleting + DeleteFinalize, preserved for tests and
// helpers that want a linear return-when-done contract.
//
// Worktree removal failures are fatal to the delete and the record is kept so
// the caller can retry after fixing the cause. The daemon's `delete` handler
// uses the async form instead, so the wait moves off the request path.
func (m *Manager) Delete(id string, removeWorktree, forceRemoveWorktree bool) error {
	req, err := m.PreCheckDelete(id, removeWorktree, forceRemoveWorktree)
	if err != nil {
		return err
	}
	if err := m.MarkDeleting(&req); err != nil {
		return err
	}
	if err := m.DeleteFinalize(req); err != nil {
		m.MarkDeletionFailed(req, err)
		return err
	}
	return nil
}

// removeGitWorktree removes a git worktree at the given path. Returns
// ErrWorktreeDirty if it has uncommitted changes and force is false, and
// ErrNotWorktree if workDir is not a git worktree. Any other failure is wrapped
// with the worktree path and the way out, since this error is what the CLI and
// TUI show verbatim.
//
// The static wrapper text must not contain the ErrWorktreeDirty /
// ErrNotWorktree messages: the daemon client restores those sentinels from the
// error string across IPC, and a collision would misreport an unrelated failure.
func (m *Manager) removeGitWorktree(workDir string, force bool) error {
	err := m.gitClient.RemoveWorktree(workDir, force)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, git.ErrDirty):
		return ErrWorktreeDirty
	case errors.Is(err, git.ErrNotWorktree):
		return ErrNotWorktree
	default:
		return fmt.Errorf("removing git worktree at %s: %w (session kept; delete without worktree removal to drop it)", workDir, err)
	}
}

// Claude Code-specific setup helpers (hooks-settings.json generation, trust
// dialog suppression) live under internal/agent/claude/. The adapter's
// Setup() is invoked from startSessionTmux, so no CC-specific code remains
// in this file.
