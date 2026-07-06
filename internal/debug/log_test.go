package debug

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStateHome forces internal/paths to resolve State() to a deterministic
// directory by setting XDG_STATE_HOME for the duration of the test.
func withStateHome(t *testing.T, dir string) string {
	t.Helper()
	orig, had := os.LookupEnv("XDG_STATE_HOME")
	os.Setenv("XDG_STATE_HOME", dir)
	t.Cleanup(func() {
		if had {
			os.Setenv("XDG_STATE_HOME", orig)
		} else {
			os.Unsetenv("XDG_STATE_HOME")
		}
	})
	stateDir := filepath.Join(dir, "jindaiko")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}
	return stateDir
}

func TestNewLogger_Disabled(t *testing.T) {
	// When JIN_DEBUG is not "1", the logger should be a no-op.
	origDebug := os.Getenv("JIN_DEBUG")
	os.Setenv("JIN_DEBUG", "0")
	defer os.Setenv("JIN_DEBUG", origDebug)

	// Reset enabled for this test
	origEnabled := enabled
	enabled = false
	defer func() { enabled = origEnabled }()

	stateDir := withStateHome(t, t.TempDir())
	filename := "test-disabled.log"

	log := NewLogger(filename)
	log("this message should not appear")

	logPath := filepath.Join(stateDir, filename)
	if _, err := os.Stat(logPath); err == nil {
		t.Error("logger created a file even though debug is disabled")
	}
}

func TestNewLogger_Enabled(t *testing.T) {
	origEnabled := enabled
	enabled = true
	defer func() { enabled = origEnabled }()

	stateDir := withStateHome(t, t.TempDir())

	log := NewLogger("test-enabled.log")
	log("hello %s %d", "world", 42)

	logPath := filepath.Join(stateDir, "test-enabled.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "hello world 42") {
		t.Errorf("log file content %q does not contain expected message", content)
	}
	// Verify timestamp format [HH:MM:SS.mmm] is present
	if !strings.Contains(content, "[") || !strings.Contains(content, "]") {
		t.Errorf("log file content %q does not contain timestamp brackets", content)
	}
}

func TestNewLogger_NoopWhenDisabled(t *testing.T) {
	origEnabled := enabled
	enabled = false
	defer func() { enabled = origEnabled }()

	log := NewLogger("noop.log")

	// Should not panic
	log("this should be a no-op")
}
