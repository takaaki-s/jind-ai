package tmux

import (
	"os"
	"reflect"
	"testing"
)

func TestWindowName(t *testing.T) {
	tests := []struct {
		sessionID string
		want      string
	}{
		{"abc123", WindowPrefix + "abc123"},
		{"", WindowPrefix},
		{"some-long-session-id-with-dashes", WindowPrefix + "some-long-session-id-with-dashes"},
	}
	for _, tt := range tests {
		got := WindowName(tt.sessionID)
		if got != tt.want {
			t.Errorf("WindowName(%q) = %q, want %q", tt.sessionID, got, tt.want)
		}
	}
}

func TestInnerSessionName(t *testing.T) {
	tests := []struct {
		sessionID string
		want      string
	}{
		{"abc123", SessionPrefix + "abc123"},
		{"", SessionPrefix},
		{"uuid-1234-5678", SessionPrefix + "uuid-1234-5678"},
	}
	for _, tt := range tests {
		got := InnerSessionName(tt.sessionID)
		if got != tt.want {
			t.Errorf("InnerSessionName(%q) = %q, want %q", tt.sessionID, got, tt.want)
		}
	}
}

func TestWindowTarget(t *testing.T) {
	tests := []struct {
		windowName string
		pane       int
		want       string
	}{
		{"sess-abc123", 0, SessionName + ":sess-abc123.0"},
		{UIWindowName, 1, SessionName + ":ui.1"},
		{"mywindow", 2, SessionName + ":mywindow.2"},
	}
	for _, tt := range tests {
		got := WindowTarget(tt.windowName, tt.pane)
		if got != tt.want {
			t.Errorf("WindowTarget(%q, %d) = %q, want %q", tt.windowName, tt.pane, got, tt.want)
		}
	}
}

func TestUITarget(t *testing.T) {
	tests := []struct {
		pane int
		want string
	}{
		{0, SessionName + ":" + UIWindowName + ".0"},
		{1, SessionName + ":" + UIWindowName + ".1"},
		{5, SessionName + ":" + UIWindowName + ".5"},
	}
	for _, tt := range tests {
		got := UITarget(tt.pane)
		if got != tt.want {
			t.Errorf("UITarget(%d) = %q, want %q", tt.pane, got, tt.want)
		}
	}
}

func TestBaseArgs(t *testing.T) {
	t.Run("without config file", func(t *testing.T) {
		c := &Client{
			tmuxPath:   "/usr/bin/tmux",
			socketName: "test-socket",
		}
		got := c.baseArgs()
		want := []string{"-L", "test-socket"}
		if len(got) != len(want) {
			t.Fatalf("baseArgs() returned %d elements, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("baseArgs()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("with config file", func(t *testing.T) {
		c := &Client{
			tmuxPath:   "/usr/bin/tmux",
			socketName: "mgr-socket",
			configFile: "/dev/null",
		}
		got := c.baseArgs()
		want := []string{"-L", "mgr-socket", "-f", "/dev/null"}
		if len(got) != len(want) {
			t.Fatalf("baseArgs() returned %d elements, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("baseArgs()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("with default socket name", func(t *testing.T) {
		c := &Client{
			tmuxPath:   "/usr/bin/tmux",
			socketName: SocketName,
		}
		got := c.baseArgs()
		if got[0] != "-L" || got[1] != SocketName {
			t.Errorf("baseArgs() = %v, want [-L %s]", got, SocketName)
		}
	})

	t.Run("with socket path", func(t *testing.T) {
		c := &Client{
			tmuxPath:   "/usr/bin/tmux",
			socketPath: "/tmp/tmux-1000/default",
		}
		got := c.baseArgs()
		want := []string{"-S", "/tmp/tmux-1000/default"}
		if len(got) != len(want) {
			t.Fatalf("baseArgs() returned %d elements, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("baseArgs()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("socket path takes precedence over socket name", func(t *testing.T) {
		c := &Client{
			tmuxPath:   "/usr/bin/tmux",
			socketName: "should-be-ignored",
			socketPath: "/tmp/tmux-1000/default",
		}
		got := c.baseArgs()
		want := []string{"-S", "/tmp/tmux-1000/default"}
		if len(got) != len(want) {
			t.Fatalf("baseArgs() returned %d elements, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("baseArgs()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

func TestNewClientWithSocket_NoTmux(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Cleanup(func() {
		os.Setenv("PATH", origPath)
	})

	// Set PATH to an empty directory so tmux cannot be found
	os.Setenv("PATH", t.TempDir())

	_, err := NewClientWithSocket("test-socket")
	if err == nil {
		t.Fatal("NewClientWithSocket() should return error when tmux is not in PATH")
	}
}

func TestHasTmux(t *testing.T) {
	// Test with normal PATH -- tmux should be available in CI/dev environments.
	// We don't assert true because tmux might not be installed, but we can
	// at least verify the function doesn't panic.
	_ = HasTmux()

	// Test with empty PATH -- tmux should not be found
	origPath := os.Getenv("PATH")
	t.Cleanup(func() {
		os.Setenv("PATH", origPath)
	})

	os.Setenv("PATH", t.TempDir())
	if HasTmux() {
		t.Error("HasTmux() = true with empty PATH, want false")
	}
}

func TestParseEnvironmentOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"multiple lines", "FOO=bar\nBAZ=qux\nHELLO=world", map[string]string{"FOO": "bar", "BAZ": "qux", "HELLO": "world"}},
		{"unset lines skipped", "FOO=bar\n-UNSET_VAR\nBAZ=qux", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"malformed lines skipped", "FOO=bar\nno_equals_here\nBAZ=qux\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"empty string", "", map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEnvironmentOutput(tt.in)
			if got == nil {
				t.Fatal("parseEnvironmentOutput returned nil, want non-nil map")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseEnvironmentOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSocketPathFromEnv(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/tmp/tmux-1000/default,12345,0", "/tmp/tmux-1000/default"},
		{"/tmp/tmux-1000/default", "/tmp/tmux-1000/default"},
	}
	for _, tt := range tests {
		if got := SocketPathFromEnv(tt.in); got != tt.want {
			t.Errorf("SocketPathFromEnv(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
