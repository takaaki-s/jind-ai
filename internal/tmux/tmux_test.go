package tmux

import (
	"fmt"
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

func TestBuildSplitArgs(t *testing.T) {
	tests := []struct {
		name string
		opts SplitOptions
		want []string
	}{
		{
			name: "defaults split down",
			opts: SplitOptions{},
			want: []string{"split-window", "-t", "%1", "-P", "-F", "#{pane_id}", "-v"},
		},
		{
			name: "up adds -b",
			opts: SplitOptions{Direction: "up"},
			want: []string{"split-window", "-t", "%1", "-P", "-F", "#{pane_id}", "-v", "-b"},
		},
		{
			name: "right is horizontal",
			opts: SplitOptions{Direction: "right"},
			want: []string{"split-window", "-t", "%1", "-P", "-F", "#{pane_id}", "-h"},
		},
		{
			name: "left is horizontal with -b",
			opts: SplitOptions{Direction: "left"},
			want: []string{"split-window", "-t", "%1", "-P", "-F", "#{pane_id}", "-h", "-b"},
		},
		{
			name: "all options",
			opts: SplitOptions{Direction: "right", Size: "30%", Full: true, NoFocus: true, Dir: "/work", Cmd: "htop"},
			want: []string{"split-window", "-t", "%1", "-P", "-F", "#{pane_id}", "-h", "-f", "-d", "-l", "30%", "-c", "/work", "htop"},
		},
		{
			name: "line size passes through",
			opts: SplitOptions{Size: "15"},
			want: []string{"split-window", "-t", "%1", "-P", "-F", "#{pane_id}", "-v", "-l", "15"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSplitArgs("%1", tt.opts)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildSplitArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    SplitOptions
		wantErr bool
	}{
		{"empty is valid", SplitOptions{}, false},
		{"down", SplitOptions{Direction: "down"}, false},
		{"up", SplitOptions{Direction: "up"}, false},
		{"left", SplitOptions{Direction: "left"}, false},
		{"right", SplitOptions{Direction: "right"}, false},
		{"invalid direction", SplitOptions{Direction: "sideways"}, true},
		{"percent size", SplitOptions{Size: "30%"}, false},
		{"line size", SplitOptions{Size: "15"}, false},
		{"zero percent", SplitOptions{Size: "0%"}, true},
		{"hundred percent", SplitOptions{Size: "100%"}, true},
		{"ninety-nine percent", SplitOptions{Size: "99%"}, false},
		{"zero lines", SplitOptions{Size: "0"}, true},
		{"negative", SplitOptions{Size: "-5"}, true},
		{"garbage", SplitOptions{Size: "abc"}, true},
		{"bare percent sign", SplitOptions{Size: "%"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePaneName(t *testing.T) {
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"simple", "demo", false},
		{"with separators", "ok-name.1_x", false},
		{"single char", "a", false},
		{"max length 64", string(long), false},
		{"too long 65", string(long) + "a", true},
		{"empty", "", true},
		{"space", "a b", true},
		{"leading dash", "-x", true},
		{"shell metachars", "a;rm", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePaneName(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePaneName(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestValidateIfExists(t *testing.T) {
	for _, v := range []string{"", "noop", "respawn", "error"} {
		if err := ValidateIfExists(v); err != nil {
			t.Errorf("ValidateIfExists(%q) = %v, want nil", v, err)
		}
	}
	if err := ValidateIfExists("maybe"); err == nil {
		t.Error("ValidateIfExists(\"maybe\") = nil, want error")
	}
}

func TestMatchPaneByName(t *testing.T) {
	out := "%1 \n%2 demo\n%3 other"
	tests := []struct {
		name string
		want string
	}{
		{"demo", "%2"},
		{"other", "%3"},
		{"missing", ""},
	}
	for _, tt := range tests {
		if got := matchPaneByName(out, tt.name); got != tt.want {
			t.Errorf("matchPaneByName(out, %q) = %q, want %q", tt.name, got, tt.want)
		}
	}
	if got := matchPaneByName("", "demo"); got != "" {
		t.Errorf("matchPaneByName(empty) = %q, want empty", got)
	}
}

// fakeSlotOps is a minimal PaneSlotOps double for EnsureNamedPane tests.
type fakeSlotOps struct {
	named        map[string]string // name -> existing pane ID
	splitID      string            // pane ID SplitPane returns
	setOptionErr error             // injected SetPaneOption failure

	splitCalls   int
	respawnCalls []string // "target cmd"
	killCalls    []string
	setCalls     []string // "target option value"
}

func (f *fakeSlotOps) FindPaneByName(target, name string) (string, error) {
	return f.named[name], nil
}

func (f *fakeSlotOps) SplitPane(target string, opts SplitOptions) (string, error) {
	f.splitCalls++
	return f.splitID, nil
}

func (f *fakeSlotOps) SetPaneOption(target, option, value string) error {
	f.setCalls = append(f.setCalls, target+" "+option+" "+value)
	return f.setOptionErr
}

func (f *fakeSlotOps) RespawnPane(target, cmd string) error {
	f.respawnCalls = append(f.respawnCalls, target+" "+cmd)
	return nil
}

func (f *fakeSlotOps) KillPane(target string) error {
	f.killCalls = append(f.killCalls, target)
	return nil
}

func TestEnsureNamedPane(t *testing.T) {
	tests := []struct {
		name         string
		slotName     string
		ifExists     string
		existing     map[string]string
		wantPane     string
		wantErr      bool
		wantSplits   int
		wantRespawns int
	}{
		{
			name:       "empty name is a plain split",
			slotName:   "",
			wantPane:   "%99",
			wantSplits: 1,
		},
		{
			name:       "named pane not found splits and tags",
			slotName:   "demo",
			wantPane:   "%99",
			wantSplits: 1,
		},
		{
			name:     "existing pane noop by default",
			slotName: "demo",
			existing: map[string]string{"demo": "%50"},
			wantPane: "%50",
		},
		{
			name:     "existing pane explicit noop",
			slotName: "demo",
			ifExists: IfExistsNoop,
			existing: map[string]string{"demo": "%50"},
			wantPane: "%50",
		},
		{
			name:         "existing pane respawn",
			slotName:     "demo",
			ifExists:     IfExistsRespawn,
			existing:     map[string]string{"demo": "%50"},
			wantPane:     "%50",
			wantRespawns: 1,
		},
		{
			name:     "existing pane error policy",
			slotName: "demo",
			ifExists: IfExistsError,
			existing: map[string]string{"demo": "%50"},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeSlotOps{named: tt.existing, splitID: "%99"}
			got, err := EnsureNamedPane(ops, "%1", tt.slotName, tt.ifExists, SplitOptions{Cmd: "top"})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantPane {
				t.Errorf("pane ID = %q, want %q", got, tt.wantPane)
			}
			if ops.splitCalls != tt.wantSplits {
				t.Errorf("SplitPane calls = %d, want %d", ops.splitCalls, tt.wantSplits)
			}
			if len(ops.respawnCalls) != tt.wantRespawns {
				t.Errorf("RespawnPane calls = %d, want %d", len(ops.respawnCalls), tt.wantRespawns)
			}
		})
	}
}

func TestEnsureNamedPane_TagsNewPane(t *testing.T) {
	ops := &fakeSlotOps{splitID: "%99"}
	if _, err := EnsureNamedPane(ops, "%1", "demo", "", SplitOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops.setCalls) != 1 || ops.setCalls[0] != "%99 "+PaneNameOption+" demo" {
		t.Errorf("SetPaneOption calls = %v, want the new pane tagged with the slot name", ops.setCalls)
	}
}

func TestEnsureNamedPane_NamingFailureKillsPane(t *testing.T) {
	ops := &fakeSlotOps{splitID: "%99", setOptionErr: fmt.Errorf("boom")}
	_, err := EnsureNamedPane(ops, "%1", "demo", "", SplitOptions{})
	if err == nil {
		t.Fatal("expected error when naming fails")
	}
	if len(ops.killCalls) != 1 || ops.killCalls[0] != "%99" {
		t.Errorf("KillPane calls = %v, want the orphaned pane killed", ops.killCalls)
	}
}

func TestEnsureNamedPane_PlainSplitSkipsTagging(t *testing.T) {
	ops := &fakeSlotOps{splitID: "%99"}
	if _, err := EnsureNamedPane(ops, "%1", "", "", SplitOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops.setCalls) != 0 {
		t.Errorf("SetPaneOption calls = %v, want none for an unnamed split", ops.setCalls)
	}
}
