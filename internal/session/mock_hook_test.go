package session

import (
	"context"
	"path/filepath"

	"github.com/takaaki-s/jindaiko/internal/worktreehook"
)

// mockHookRunner is a test double for worktreehook.Runner. Configure the maps
// before invoking Manager, then inspect calls afterwards. Mirrors the
// mockTmuxRunner pattern in mock_tmux_test.go (same package, so mockCall and
// hasCalledWith-style helpers are shared conceptually but each mock keeps its
// own call log to keep assertions scoped).
type mockHookRunner struct {
	discoverExists map[string]bool                 // key: repoRoot
	scriptPathFor  map[string]string               // key: repoRoot; default: <repoRoot>/.jin/worktree-post-create.sh
	verdictFor     map[string]worktreehook.Verdict // key: scriptPath
	verifyErrFor   map[string]error                // key: scriptPath
	runErr         error
	logPath        string

	calls []mockCall
}

func newMockHookRunner() *mockHookRunner {
	return &mockHookRunner{
		discoverExists: make(map[string]bool),
		scriptPathFor:  make(map[string]string),
		verdictFor:     make(map[string]worktreehook.Verdict),
		verifyErrFor:   make(map[string]error),
	}
}

func (m *mockHookRunner) record(method string, args ...string) {
	m.calls = append(m.calls, mockCall{method: method, args: args})
}

func (m *mockHookRunner) Discover(repoRoot string) (string, bool) {
	m.record("Discover", repoRoot)
	exists := m.discoverExists[repoRoot]
	if !exists {
		return "", false
	}
	if p, ok := m.scriptPathFor[repoRoot]; ok {
		return p, true
	}
	return filepath.Join(repoRoot, ".jin", "worktree-post-create.sh"), true
}

func (m *mockHookRunner) Verify(scriptPath, repoRoot string) (worktreehook.Verdict, error) {
	m.record("Verify", scriptPath, repoRoot)
	if err, ok := m.verifyErrFor[scriptPath]; ok && err != nil {
		return m.verdictFor[scriptPath], err
	}
	return m.verdictFor[scriptPath], nil
}

func (m *mockHookRunner) Run(ctx context.Context, opts worktreehook.RunOptions) error {
	m.record("Run", opts.ScriptPath, opts.WorktreePath, opts.RepoRoot, opts.SessionID)
	return m.runErr
}

func (m *mockHookRunner) HookLogPath(stateDir, sessionID string) string {
	m.record("HookLogPath", stateDir, sessionID)
	if m.logPath != "" {
		return m.logPath
	}
	return filepath.Join(stateDir, "hook-logs", sessionID+".log")
}

// hasCalledWith returns true if the mock recorded a call to method whose first
// argument equals arg. Matches mockTmuxRunner.hasCalledWith semantics so
// assertions read the same across both mocks.
func (m *mockHookRunner) hasCalledWith(method, arg string) bool {
	for _, c := range m.calls {
		if c.method == method && len(c.args) > 0 && c.args[0] == arg {
			return true
		}
	}
	return false
}

// callCount returns how many times the given method was invoked.
func (m *mockHookRunner) callCount(method string) int {
	n := 0
	for _, c := range m.calls {
		if c.method == method {
			n++
		}
	}
	return n
}
