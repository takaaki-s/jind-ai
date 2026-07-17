package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests exercise the validation branches of the pane-* handlers without
// spinning a full Server, in the style of handle_new_test.go. Every case here
// returns before touching s.manager, so the zero-value Server{} is safe.

func TestHandlePanePopup_MissingID(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PanePopupRequest{Cmd: "echo hi"})
	resp := s.handlePanePopup(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "id is required") {
		t.Errorf("Error = %q, want to contain 'id is required'", resp.Error)
	}
}

func TestHandlePanePopup_MissingCmd(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PanePopupRequest{ID: "sess-1"})
	resp := s.handlePanePopup(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "cmd is required") {
		t.Errorf("Error = %q, want to contain 'cmd is required'", resp.Error)
	}
}

func TestHandlePanePopup_InvalidJSON(t *testing.T) {
	s := &Server{}
	resp := s.handlePanePopup(json.RawMessage(`{`))

	if resp.Success {
		t.Fatal("expected Success=false")
	}
}

func TestHandlePaneSplit_MissingID(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSplitRequest{Cmd: "top"})
	resp := s.handlePaneSplit(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "id is required") {
		t.Errorf("Error = %q, want to contain 'id is required'", resp.Error)
	}
}

func TestHandlePaneSplit_InvalidJSON(t *testing.T) {
	s := &Server{}
	resp := s.handlePaneSplit(json.RawMessage(`{`))

	if resp.Success {
		t.Fatal("expected Success=false")
	}
}

func TestHandlePaneSplit_InvalidDirection(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSplitRequest{ID: "sess-1", Direction: "sideways"})
	resp := s.handlePaneSplit(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "invalid direction") {
		t.Errorf("Error = %q, want to contain 'invalid direction'", resp.Error)
	}
}

func TestHandlePaneSplit_InvalidSize(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSplitRequest{ID: "sess-1", Size: "abc"})
	resp := s.handlePaneSplit(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "invalid size") {
		t.Errorf("Error = %q, want to contain 'invalid size'", resp.Error)
	}
}

func TestHandlePaneSplit_InvalidIfExists(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSplitRequest{ID: "sess-1", Name: "demo", IfExists: "maybe"})
	resp := s.handlePaneSplit(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "invalid if-exists") {
		t.Errorf("Error = %q, want to contain 'invalid if-exists'", resp.Error)
	}
}

func TestHandlePaneSplit_IfExistsWithoutName(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSplitRequest{ID: "sess-1", IfExists: "respawn"})
	resp := s.handlePaneSplit(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "--if-exists requires --name") {
		t.Errorf("Error = %q, want to contain '--if-exists requires --name'", resp.Error)
	}
}

func TestHandlePaneSplit_InvalidName(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSplitRequest{ID: "sess-1", Name: "has space"})
	resp := s.handlePaneSplit(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "invalid pane name") {
		t.Errorf("Error = %q, want to contain 'invalid pane name'", resp.Error)
	}
}

func TestHandlePaneClose_MissingID(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneCloseRequest{Name: "demo"})
	resp := s.handlePaneClose(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "id is required") {
		t.Errorf("Error = %q, want to contain 'id is required'", resp.Error)
	}
}

func TestHandlePaneClose_MissingName(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneCloseRequest{ID: "sess-1"})
	resp := s.handlePaneClose(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "name is required") {
		t.Errorf("Error = %q, want to contain 'name is required'", resp.Error)
	}
}

func TestHandlePaneClose_InvalidJSON(t *testing.T) {
	s := &Server{}
	resp := s.handlePaneClose(json.RawMessage(`{`))

	if resp.Success {
		t.Fatal("expected Success=false")
	}
}

func TestHandlePaneCapture_MissingID(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneCaptureRequest{})
	resp := s.handlePaneCapture(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "id is required") {
		t.Errorf("Error = %q, want to contain 'id is required'", resp.Error)
	}
}

func TestHandlePaneCapture_InvalidJSON(t *testing.T) {
	s := &Server{}
	resp := s.handlePaneCapture(json.RawMessage(`{`))

	if resp.Success {
		t.Fatal("expected Success=false")
	}
}

func TestHandlePaneSendKeys_MissingID(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSendKeysRequest{Keys: "hello"})
	resp := s.handlePaneSendKeys(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "id is required") {
		t.Errorf("Error = %q, want to contain 'id is required'", resp.Error)
	}
}

func TestHandlePaneSendKeys_MissingKeys(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(PaneSendKeysRequest{ID: "sess-1"})
	resp := s.handlePaneSendKeys(data)

	if resp.Success {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Error, "keys is required") {
		t.Errorf("Error = %q, want to contain 'keys is required'", resp.Error)
	}
}

func TestHandlePaneSendKeys_InvalidJSON(t *testing.T) {
	s := &Server{}
	resp := s.handlePaneSendKeys(json.RawMessage(`{`))

	if resp.Success {
		t.Fatal("expected Success=false")
	}
}
