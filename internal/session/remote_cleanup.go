package session

import (
	"context"
	"fmt"
	"path/filepath"
)

// RemoteCleanupOwnership is assembled only from the target's persisted Task
// and Session journals. None of these values are accepted from the wire.
type RemoteCleanupOwnership struct {
	SessionID      string
	IdempotencyKey string
	RepositoryPath string
	WorktreeName   string
	Branch         string
}

type RemoteCleanupResult struct {
	Session   bool
	Worktree  bool
	Branch    bool
	Uncertain bool
}

// CleanupRemoteOwned removes exactly one inactive, jind-ai-created worktree
// session after independently proving its repository, worktree, and branch.
func (m *Manager) CleanupRemoteOwned(ownership RemoteCleanupOwnership) (RemoteCleanupResult, error) {
	var result RemoteCleanupResult
	if ownership.SessionID == "" || ownership.IdempotencyKey == "" || ownership.WorktreeName == "" || ownership.Branch == "" ||
		!filepath.IsAbs(ownership.RepositoryPath) {
		return result, fmt.Errorf("remote cleanup ownership is incomplete")
	}

	m.mu.RLock()
	sess, ok := m.sessions[ownership.SessionID]
	if !ok {
		m.mu.RUnlock()
		return result, fmt.Errorf("owned session is unavailable")
	}
	snapshot := *sess
	tmuxClient := m.tmuxClient
	m.mu.RUnlock()
	if snapshot.IsAdopted() {
		return result, fmt.Errorf("owned session is adopted")
	}
	if snapshot.Status != StatusStopped {
		return result, fmt.Errorf("remote cleanup requires a stopped session")
	}
	if activeCleanupPane(tmuxClient, snapshot.TmuxWindowName, snapshot.TmuxPaneID) {
		return result, fmt.Errorf("remote cleanup requires an inactive session pane")
	}
	worktreePath := filepath.Clean(snapshot.ReviewBase.WorktreePath)
	if snapshot.ReviewBase.WorktreePath == "" || !filepath.IsAbs(worktreePath) ||
		filepath.Clean(snapshot.WorkDir) != worktreePath || filepath.Base(worktreePath) != ownership.WorktreeName {
		return result, fmt.Errorf("remote cleanup worktree ownership mismatch")
	}

	head, exists, err := m.gitClient.LocalBranchHead(ownership.RepositoryPath, ownership.Branch)
	if err != nil || !exists {
		if err == nil {
			err = fmt.Errorf("owned branch is unavailable")
		}
		return result, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewProbeTimeout)
	defer cancel()
	observed, err := m.gitClient.InspectCleanupWorktree(ctx, worktreePath, head)
	if err != nil {
		return result, err
	}
	if filepath.Clean(observed.RepositoryPath) != filepath.Clean(ownership.RepositoryPath) ||
		filepath.Clean(observed.WorktreePath) != worktreePath || observed.Branch != ownership.Branch || observed.HeadCommit != head {
		return result, fmt.Errorf("remote cleanup git ownership mismatch")
	}
	clean, err := m.gitClient.ReviewWorktreeClean(ctx, worktreePath)
	if err != nil {
		return result, err
	}
	if !clean {
		return result, ErrWorktreeDirty
	}

	m.mu.Lock()
	live, ok := m.sessions[ownership.SessionID]
	if !ok || live.Status != StatusStopped || live.WorkDir != snapshot.WorkDir || live.ReviewBase != snapshot.ReviewBase ||
		live.TmuxWindowName != snapshot.TmuxWindowName || live.TmuxPaneID != snapshot.TmuxPaneID {
		m.mu.Unlock()
		return result, fmt.Errorf("remote cleanup ownership changed during verification")
	}
	live.Status = StatusDeleting
	live.ReviewCleanupKey = ownership.IdempotencyKey
	saved := *live
	m.mu.Unlock()
	if err := m.store.Save(saved); err != nil {
		m.releaseRemoteCleanupClaim(ownership.SessionID, ownership.IdempotencyKey, false)
		return result, err
	}
	if activeCleanupPane(tmuxClient, snapshot.TmuxWindowName, snapshot.TmuxPaneID) {
		m.releaseRemoteCleanupClaim(ownership.SessionID, ownership.IdempotencyKey, true)
		return result, fmt.Errorf("remote cleanup session pane became active")
	}
	if err := m.gitClient.RemoveWorktree(worktreePath, false); err != nil {
		result.Uncertain = true
		return result, err
	}
	result.Worktree = true
	if err := m.gitClient.DeleteExactBranch(ownership.RepositoryPath, ownership.Branch, head); err != nil {
		return result, err
	}
	result.Branch = true

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.Delete(ownership.SessionID); err != nil {
		return result, err
	}
	delete(m.sessions, ownership.SessionID)
	result.Session = true
	return result, nil
}

func (m *Manager) releaseRemoteCleanupClaim(sessionID, key string, persist bool) {
	m.mu.Lock()
	live, ok := m.sessions[sessionID]
	if !ok || live.Status != StatusDeleting || live.ReviewCleanupKey != key {
		m.mu.Unlock()
		return
	}
	live.Status = StatusStopped
	live.ReviewCleanupKey = ""
	saved := *live
	m.mu.Unlock()
	if persist {
		_ = m.store.Save(saved)
	}
}
