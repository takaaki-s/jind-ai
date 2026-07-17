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

	// splitPaneIDs overrides the pane ID SplitPane returns for a given
	// target; unset targets get "%99". namedPanes maps a slot name to the
	// pane ID FindPaneByName reports ("" / unset = not found).
	splitPaneIDs map[string]string
	namedPanes   map[string]string

	// capturedSequence overrides captured for tests that need CapturePane
	// to return different values on successive calls (send-verify retry
	// scenarios). If set, entries are consumed in order and the final
	// entry is repeated once exhausted. Empty/nil falls back to captured.
	capturedSequence map[string][]string
	capturedIdx      map[string]int

	// captureErr, if set for a target, makes CapturePane return that
	// error instead of any recorded content. Consumed on every call.
	captureErr map[string]error

	// captureErrAfter, if set for a target, is returned as the error on
	// every CapturePane call after the first (i.e. the "after" capture in
	// a SendPrompt attempt succeeds only on the initial "before" call,
	// then fails). Lets tests exercise the "after"-side error path in
	// isolation without failing the "before" capture first.
	captureErrAfter map[string]error

	// captureCallCount tracks how many times CapturePane was invoked per
	// target, so captureErrAfter can distinguish first vs. subsequent
	// calls without relying on capturedSequence consumption.
	captureCallCount map[string]int

	// sendKeysLiteralErr injects an error for SendKeysLiteral on a given
	// target. Used by SendPrompt tests to simulate a tmux write failure
	// during the prompt-injection phase.
	sendKeysLiteralErr map[string]error

	calls []mockCall // recorded calls for assertion
}

func newMockTmuxRunner() *mockTmuxRunner {
	return &mockTmuxRunner{
		sessions:           make(map[string]bool),
		deadPanes:          make(map[string]bool),
		paneIDs:            make(map[string]string),
		panePaths:          make(map[string]string),
		captured:           make(map[string]string),
		splitPaneIDs:       make(map[string]string),
		namedPanes:         make(map[string]string),
		capturedSequence:   make(map[string][]string),
		capturedIdx:        make(map[string]int),
		captureErr:         make(map[string]error),
		captureErrAfter:    make(map[string]error),
		captureCallCount:   make(map[string]int),
		sendKeysLiteralErr: make(map[string]error),
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
	if err, ok := m.sendKeysLiteralErr[target]; ok && err != nil {
		return err
	}
	return nil
}

func (m *mockTmuxRunner) DisplayPopup(opts tmux.DisplayPopupOptions) error {
	m.record("DisplayPopup", opts.Target, opts.Cmd, opts.Dir)
	return nil
}

func (m *mockTmuxRunner) SplitPane(target string, opts tmux.SplitOptions) (string, error) {
	m.record("SplitPane", target, opts.Cmd, opts.Direction, opts.Size, opts.Dir)
	if id, ok := m.splitPaneIDs[target]; ok {
		return id, nil
	}
	return "%99", nil
}

func (m *mockTmuxRunner) FindPaneByName(target, name string) (string, error) {
	m.record("FindPaneByName", target, name)
	return m.namedPanes[name], nil
}

func (m *mockTmuxRunner) SetPaneOption(target, option, value string) error {
	m.record("SetPaneOption", target, option, value)
	return nil
}

func (m *mockTmuxRunner) CapturePane(target string, ansi bool) (string, error) {
	m.record("CapturePane", target)
	m.captureCallCount[target]++
	if err, ok := m.captureErr[target]; ok && err != nil {
		return "", err
	}
	if err, ok := m.captureErrAfter[target]; ok && err != nil && m.captureCallCount[target] > 1 {
		return "", err
	}
	if seq, ok := m.capturedSequence[target]; ok && len(seq) > 0 {
		idx := m.capturedIdx[target]
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		val := seq[idx]
		if idx+1 < len(seq) {
			m.capturedIdx[target] = idx + 1
		}
		return val, nil
	}
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
