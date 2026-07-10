package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/paths"
)

func TestGetSocketPath(t *testing.T) {
	t.Run("flag takes precedence", func(t *testing.T) {
		orig := socketPathFlag
		socketPathFlag = "/tmp/flag.sock"
		defer func() { socketPathFlag = orig }()
		t.Setenv("JIN_SOCKET", "/tmp/env.sock")

		if got := getSocketPath(); got != "/tmp/flag.sock" {
			t.Errorf("getSocketPath() = %q, want %q", got, "/tmp/flag.sock")
		}
	})

	t.Run("env used when flag unset", func(t *testing.T) {
		orig := socketPathFlag
		socketPathFlag = ""
		defer func() { socketPathFlag = orig }()
		t.Setenv("JIN_SOCKET", "/tmp/env.sock")

		if got := getSocketPath(); got != "/tmp/env.sock" {
			t.Errorf("getSocketPath() = %q, want %q", got, "/tmp/env.sock")
		}
	})

	t.Run("falls back to paths.Socket when neither set", func(t *testing.T) {
		orig := socketPathFlag
		socketPathFlag = ""
		defer func() { socketPathFlag = orig }()
		t.Setenv("JIN_SOCKET", "")

		if got := getSocketPath(); got != paths.Socket() {
			t.Errorf("getSocketPath() = %q, want %q", got, paths.Socket())
		}
	})
}

func TestRenderDaemonStatusJSON(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		result := daemonStatusResult{Running: true}
		var buf bytes.Buffer
		if err := renderDaemonStatusJSON(&buf, result); err != nil {
			t.Fatalf("renderDaemonStatusJSON() error = %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed["running"] != true {
			t.Errorf("expected running=true, got %v", parsed["running"])
		}
	})

	t.Run("not running", func(t *testing.T) {
		result := daemonStatusResult{Running: false}
		var buf bytes.Buffer
		if err := renderDaemonStatusJSON(&buf, result); err != nil {
			t.Fatalf("renderDaemonStatusJSON() error = %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed["running"] != false {
			t.Errorf("expected running=false, got %v", parsed["running"])
		}
	})
}
