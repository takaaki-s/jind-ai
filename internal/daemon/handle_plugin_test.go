package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests exercise the validation branches of handlePluginRun without
// spinning a full Server, in the style of handle_pane_test.go. The handler
// checks Plugin and the nil dispatcher before touching s.manager, so a
// zero-value Server{} covers every case here.

func TestHandlePluginRun_MissingPlugin(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PluginRunRequest{SessionID: "sess-1"})
	resp := s.handlePluginRun(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "plugin is required") {
		t.Errorf("Error = %q, want to contain 'plugin is required'", resp.Error)
	}
}

// An empty SessionID is a valid global action, not a validation error: the
// request must sail past the field checks and reach the dispatcher gate. On a
// zero-value Server that gate reports "plugins are not enabled", which proves
// the old "session_id is required" rejection is gone without needing a live
// dispatcher.
func TestHandlePluginRun_GlobalActionPassesValidation(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PluginRunRequest{Plugin: "notifier"})
	resp := s.handlePluginRun(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "plugins are not enabled") {
		t.Errorf("Error = %q, want to contain 'plugins are not enabled' (not a session_id validation error)", resp.Error)
	}
}

func TestHandlePluginRun_InvalidJSON(t *testing.T) {
	s := &Server{}
	resp := s.handlePluginRun(json.RawMessage(`{`))

	if resp.Success {
		t.Fatal("expected Success=false")
	}
}

// A zero-value Server has a nil pluginDisp; the handler must report that before
// dereferencing the (also nil) manager, so both required fields being set still
// yields a clean error rather than a panic.
func TestHandlePluginRun_NoDispatcher(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PluginRunRequest{Plugin: "notifier", SessionID: "sess-1"})
	resp := s.handlePluginRun(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "plugins are not enabled") {
		t.Errorf("Error = %q, want to contain 'plugins are not enabled'", resp.Error)
	}
}
