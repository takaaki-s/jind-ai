package session

import (
	"fmt"

	"github.com/takaaki-s/jind-ai/internal/tmux"
)

// mockCall records a method invocation on mockTmuxRunner.
type mockCall struct {
	method string
	args   []string
}

// mockTmuxRunner is a test double for tmux.Runner.
// Configure the maps before calling Manager methods, then inspect calls afterwards.
type mockTmuxRunner struct {
	sessions  map[string]bool   // session existence (HasSession return value)
	deadPanes map[string]bool   // pane dead status (IsPaneDead return value)
	paneIDs   map[string]string // session name -> pane ID (GetPaneID return value)
	panePaths map[string]string // target -> current path (GetPaneCurrentPath return value)
	captured  map[string]string // target -> content (CapturePane return value)

	calls []mockCall // recorded calls for assertion
}

func newMockTmuxRunner() *mockTmuxRunner {
	return &mockTmuxRunner{
		sessions:  make(map[string]bool),
		deadPanes: make(map[string]bool),
		paneIDs:   make(map[string]string),
		panePaths: make(map[string]string),
		captured:  make(map[string]string),
	}
}

func (m *mockTmuxRunner) record(method string, args ...string) {
	m.calls = append(m.calls, mockCall{method: method, args: args})
}

func (m *mockTmuxRunner) HasSession(name string) bool {
	m.record("HasSession", name)
	return m.sessions[name]
}

func (m *mockTmuxRunner) KillSession(name string) error {
	m.record("KillSession", name)
	delete(m.sessions, name)
	return nil
}

func (m *mockTmuxRunner) NewSessionWithCmdInDir(name string, width, height int, dir, cmd string) error {
	m.record("NewSessionWithCmdInDir", name, dir, cmd)
	m.sessions[name] = true
	return nil
}

func (m *mockTmuxRunner) RespawnPane(target, cmd string) error {
	m.record("RespawnPane", target, cmd)
	return nil
}

func (m *mockTmuxRunner) GetPaneID(sessionName string) (string, error) {
	m.record("GetPaneID", sessionName)
	if id, ok := m.paneIDs[sessionName]; ok {
		return id, nil
	}
	return "", fmt.Errorf("no pane ID for session %s", sessionName)
}

func (m *mockTmuxRunner) IsPaneDead(target string) bool {
	m.record("IsPaneDead", target)
	return m.deadPanes[target]
}

func (m *mockTmuxRunner) TagManagedPane(paneID string) error {
	m.record("TagManagedPane", paneID)
	return nil
}

func (m *mockTmuxRunner) SetupAutoCleanDeadPanes() error {
	m.record("SetupAutoCleanDeadPanes")
	return nil
}

func (m *mockTmuxRunner) KillPane(paneID string) error {
	m.record("KillPane", paneID)
	return nil
}

func (m *mockTmuxRunner) GetPaneCurrentPath(target string) (string, error) {
	m.record("GetPaneCurrentPath", target)
	if p, ok := m.panePaths[target]; ok {
		return p, nil
	}
	return "", fmt.Errorf("no pane path for target %s", target)
}

func (m *mockTmuxRunner) SendKeys(target, keys string) error {
	m.record("SendKeys", target, keys)
	return nil
}

func (m *mockTmuxRunner) SendKeysLiteral(target, text string) error {
	m.record("SendKeysLiteral", target, text)
	return nil
}

func (m *mockTmuxRunner) DisplayPopup(opts tmux.DisplayPopupOptions) error {
	m.record("DisplayPopup", opts.Target, opts.Cmd, opts.Dir)
	return nil
}

func (m *mockTmuxRunner) SplitWindow(target string, horizontal bool, percent int, shellCmd string) error {
	m.record("SplitWindow", target, shellCmd)
	return nil
}

func (m *mockTmuxRunner) CapturePane(target string, ansi bool) (string, error) {
	m.record("CapturePane", target)
	return m.captured[target], nil
}

// hasCalledWith returns true if the mock recorded a call to the given method
// where the first argument matches arg.
func (m *mockTmuxRunner) hasCalledWith(method, arg string) bool {
	for _, c := range m.calls {
		if c.method == method && len(c.args) > 0 && c.args[0] == arg {
			return true
		}
	}
	return false
}
