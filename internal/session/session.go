package session

import (
	"sort"
	"time"
)

// DefaultFleet is the fleet name used when no fleet is specified.
const DefaultFleet = "default"

// Status represents the session status
type Status string

const (
	// StatusCreating covers both worktree/session provisioning and the agent's
	// own startup window. The union is intentional: both are "not yet usable"
	// and the UI treats them identically.
	StatusCreating   Status = "creating"
	StatusStopped    Status = "stopped"    // Process stopped
	StatusRunning    Status = "running"    // Running (details unknown)
	StatusIdle       Status = "idle"       // Waiting for input (Stop hook)
	StatusThinking   Status = "thinking"   // Processing (UserPromptSubmit hook)
	StatusPermission Status = "permission" // Waiting for permission (Notification hook)
	// StatusDeleting marks a session whose deletion has been accepted but not
	// finalized — the daemon removes the worktree asynchronously so `git worktree
	// remove` does not hold up the client. It ends either with the record
	// dropped, or back at Stopped with ErrorMessage set.
	StatusDeleting Status = "deleting"
)

// Session represents an agent session managed by jind-ai. The concrete agent
// (Claude Code, Codex CLI, ...) is identified by AgentKind and driven through
// the interfaces in agent_types.go.
type Session struct {
	ID                string    `json:"id"`
	Description       string    `json:"description"`
	DescriptionLocked bool      `json:"description_locked,omitempty"`
	WorkDir           string    `json:"work_dir"`
	CreatedAt         time.Time `json:"created_at"`
	Status            Status    `json:"status"`

	// Last active time (persisted)
	LastActiveAt time.Time `json:"last_active_at,omitzero"`

	// Error info
	ErrorMessage string `json:"error_message,omitempty"` // Error message

	// CreationWarning carries a non-fatal message produced during async
	// provisioning (e.g. "post-create hook detected but not allowed"). Unlike
	// ErrorMessage it does not fail the creation, and it is never cleared
	// automatically: the warning was true at creation time. The record is
	// dropped when the session is deleted.
	CreationWarning string `json:"creation_warning,omitempty"`

	// AgentKind identifies the adapter (registry key) that owns this session.
	// Always non-empty in persisted form; the store migration backfills legacy
	// records with "claude".
	AgentKind string `json:"agent_kind"`
	// AgentSessionID is the adapter-side persistent identifier (Claude Code's
	// --session-id / --resume UUID, for example). Kept alongside AgentKind so
	// the same field can serve every adapter.
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// AgentSessionStarted is true once the agent has been launched at least
	// once with AgentSessionID; adapters use it to switch between "start" and
	// "resume" command lines.
	AgentSessionStarted bool `json:"agent_session_started,omitempty"`
	// AgentSessionIDConfirmed is true once the agent itself named
	// AgentSessionID in a hook payload. Until then the value is the UUID
	// jind-ai minted when the record was created, which only an agent taking
	// its id on the command line (Claude Code's --session-id) can be resumed
	// with — Codex and opencode mint their own and have never heard of it.
	//
	// Set by HandleHookEvent and cleared by the quick-fail retry, never at
	// spawn. Written without omitempty so a missing key means "record predates
	// the field" rather than "false" — migrateSessionJSON reads that
	// distinction.
	AgentSessionIDConfirmed bool `json:"agent_session_id_confirmed"`
	// Model is the agent model this session was created with, in the agent
	// CLI's own spelling (see SpawnOptions.Model). Persisted because every
	// respawn — quick-fail retry, revive, daemon-restart recovery — rebuilds
	// the command from this record: dropping it would move a running
	// conversation onto a different model without saying so.
	Model string `json:"model,omitempty"`

	// Attention is the completion receipt, orthogonal to Status (see
	// attention.go). Zero means "nothing to acknowledge", which is also what a
	// record written before the field existed decodes to — that is why there is
	// no attention migration.
	Attention Attention `json:"attention,omitzero"`

	// Fleet grouping
	Fleet string `json:"fleet"` // Fleet name for session grouping

	// tmux integration
	TmuxWindowName string `json:"tmux_window_name,omitempty"` // tmux window name for this session
	TmuxPaneID     string `json:"tmux_pane_id,omitempty"`     // CC pane ID (e.g., "%42") for capture-pane

	// Runtime fields (not persisted)
	LastOutputTime   time.Time        `json:"-"` // Last PTY output received (for idle stability detection)
	StartedAt        time.Time        `json:"-"` // Process start time (prevents false error detection right after startup)
	SSHAuthSock      string           `json:"-"` // SSH_AUTH_SOCK (for git operations, not persisted)
	DescriptionLayer DescriptionLayer `json:"-"` // Runtime-only enhancer layer; see DescriptionLayer docs + TryUpgradeDescription's restart guard
	PersistedStatus  Status           `json:"-"` // Status read from disk at load time, before the in-memory normalization to Stopped; consumed once by recovery
	// killSeq counts the stops Kill has recorded, so code that dropped m.mu can
	// tell whether a Kill landed while it was away. Nothing else answers that any
	// more: a kill now leaves the tmux window standing so the session can be
	// revived in place, and a session reloaded from disk looks Stopped either way.
	//
	// Kill is the only writer. A stop observed from elsewhere — a hook, the
	// monitor finding the pane dead — is a session that stopped by itself.
	killSeq uint64

	// spawnResumed carries SpawnPlan.Resumed from the last spawn; classifyPaneDeath
	// is what reads it.
	spawnResumed bool

	// Tracked runtime fields (CurrentWorkDir is persisted so worktree/subdir
	// context survives daemon restarts and enables resume in the last known dir).
	CurrentWorkDir string `json:"current_work_dir,omitempty"` // Current working directory (tmux pane_current_path)
	CurrentBranch  string `json:"-"`                          // Current git branch
	IsGitRepo      bool   `json:"-"`                          // Whether CurrentWorkDir is inside a git repository
	IsWorktree     bool   `json:"-"`                          // Whether CurrentWorkDir is a git worktree (not the main repo)
	// RepoName is the human-facing repository name (ResolveRepoName): for a
	// worktree the main repo's name, not the worktree directory's. Runtime-only
	// like CurrentBranch, but also seeded at create, provision and load time so
	// stopped and freshly-restored sessions still have one.
	RepoName string `json:"-"`
}

// Info returns session information for display
type Info struct {
	ID                string    `json:"id"`
	Description       string    `json:"description"`
	DescriptionLocked bool      `json:"description_locked,omitempty"`
	WorkDir           string    `json:"work_dir"`
	Status            Status    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	LastActiveAt      time.Time `json:"last_active_at,omitzero"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreationWarning   string    `json:"creation_warning,omitempty"` // Non-fatal warning from async provisioning (see Session.CreationWarning)
	AgentKind         string    `json:"agent_kind,omitempty"`       // Adapter identifier ("claude" etc.)
	AgentSessionID    string    `json:"agent_session_id,omitempty"` // Adapter-side persistent session id (transcript lookup, resume)
	Model             string    `json:"model,omitempty"`            // Agent model in the CLI's own spelling (see Session.Model)
	TmuxWindowName    string    `json:"tmux_window_name,omitempty"` // tmux window name
	Fleet             string    `json:"fleet"`                      // Fleet name for session grouping

	// Attention projects the completion receipt with `unseen` derived. Omitted
	// entirely at zero, so a consumer that finds no object may read it as
	// none/seen.
	Attention AttentionInfo `json:"attention,omitzero"`

	// Tracked fields (dynamic, from daemon polling)
	CurrentWorkDir string `json:"current_work_dir,omitempty"` // Current working directory
	CurrentBranch  string `json:"current_branch,omitempty"`   // Current git branch
	RepoName       string `json:"repo_name,omitempty"`        // Repository name; the main repo's for a worktree (see Session.RepoName)
	IsWorktree     bool   `json:"is_worktree,omitempty"`      // Whether WorkDir is a git worktree

	// Last messages from transcript
	LastUserMessage      string `json:"last_user_message,omitempty"`      // Last user message content (truncated)
	LastAssistantMessage string `json:"last_assistant_message,omitempty"` // Last assistant message content (truncated)
}

// SortInfos sorts a slice of Info by Fleet (lexicographically, DefaultFleet last),
// then by CreatedAt (oldest first). This is the canonical sort order used
// throughout the application. This function sorts the slice in-place.
func SortInfos(infos []Info) {
	sort.SliceStable(infos, func(i, j int) bool {
		fi, fj := infos[i].Fleet, infos[j].Fleet
		if fi != fj {
			// DefaultFleet always sorts last
			if fi == DefaultFleet {
				return false
			}
			if fj == DefaultFleet {
				return true
			}
			return fi < fj
		}
		return infos[i].CreatedAt.Before(infos[j].CreatedAt)
	})
}

// ToInfo converts Session to Info
func (s *Session) ToInfo() Info {
	return Info{
		ID:                s.ID,
		Description:       s.Description,
		DescriptionLocked: s.DescriptionLocked,
		WorkDir:           s.WorkDir,
		Status:            s.Status,
		CreatedAt:         s.CreatedAt,
		LastActiveAt:      s.LastActiveAt,
		ErrorMessage:      s.ErrorMessage,
		CreationWarning:   s.CreationWarning,
		AgentKind:         s.AgentKind,
		AgentSessionID:    s.AgentSessionID,
		Model:             s.Model,
		TmuxWindowName:    s.TmuxWindowName,
		Fleet:             s.Fleet,
		Attention:         s.Attention.toInfo(),
		CurrentWorkDir:    s.CurrentWorkDir,
		CurrentBranch:     s.CurrentBranch,
		RepoName:          s.RepoName,
		IsWorktree:        s.IsWorktree,
	}
}
