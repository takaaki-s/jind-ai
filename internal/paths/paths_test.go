package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withEnv(t *testing.T, key, value string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	if value == "" {
		os.Unsetenv(key)
	} else {
		os.Setenv(key, value)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, orig)
		} else {
			os.Unsetenv(key)
		}
	})
}

func TestConfig_UsesXDGWhenSet(t *testing.T) {
	withEnv(t, "XDG_CONFIG_HOME", "/tmp/cc-cfg")

	got := Config()
	want := filepath.Join("/tmp/cc-cfg", "jind-ai")
	if got != want {
		t.Errorf("Config() = %q, want %q", got, want)
	}
}

func TestConfig_FallsBackToHomeDotConfig(t *testing.T) {
	withEnv(t, "XDG_CONFIG_HOME", "")
	withEnv(t, "HOME", "/tmp/cc-home")

	got := Config()
	want := filepath.Join("/tmp/cc-home", ".config", "jind-ai")
	if got != want {
		t.Errorf("Config() = %q, want %q", got, want)
	}
}

func TestState_UsesXDGWhenSet(t *testing.T) {
	withEnv(t, "XDG_STATE_HOME", "/tmp/cc-state")

	got := State()
	want := filepath.Join("/tmp/cc-state", "jind-ai")
	if got != want {
		t.Errorf("State() = %q, want %q", got, want)
	}
}

func TestState_FallsBackToHomeLocalState(t *testing.T) {
	withEnv(t, "XDG_STATE_HOME", "")
	withEnv(t, "HOME", "/tmp/cc-home")

	got := State()
	want := filepath.Join("/tmp/cc-home", ".local", "state", "jind-ai")
	if got != want {
		t.Errorf("State() = %q, want %q", got, want)
	}
}

func TestData_UsesXDGWhenSet(t *testing.T) {
	withEnv(t, "XDG_DATA_HOME", "/tmp/cc-data")

	got := Data()
	want := filepath.Join("/tmp/cc-data", "jind-ai")
	if got != want {
		t.Errorf("Data() = %q, want %q", got, want)
	}
}

func TestData_FallsBackToHomeLocalShare(t *testing.T) {
	withEnv(t, "XDG_DATA_HOME", "")
	withEnv(t, "HOME", "/tmp/cc-home")

	got := Data()
	want := filepath.Join("/tmp/cc-home", ".local", "share", "jind-ai")
	if got != want {
		t.Errorf("Data() = %q, want %q", got, want)
	}
}

func TestPlugins_IsUnderData(t *testing.T) {
	withEnv(t, "XDG_DATA_HOME", "/tmp/cc-data")

	got := Plugins()
	want := filepath.Join("/tmp/cc-data", "jind-ai", "plugins")
	if got != want {
		t.Errorf("Plugins() = %q, want %q", got, want)
	}
}

func TestSessions_IsUnderState(t *testing.T) {
	withEnv(t, "XDG_STATE_HOME", "/tmp/cc-state")

	got := Sessions()
	want := filepath.Join("/tmp/cc-state", "jind-ai", "sessions")
	if got != want {
		t.Errorf("Sessions() = %q, want %q", got, want)
	}
}

func TestRuntime_UsesXDGWhenSet(t *testing.T) {
	withEnv(t, "XDG_RUNTIME_DIR", "/run/user/1000")

	got := runtime()
	want := filepath.Join("/run/user/1000", "jind-ai")
	if got != want {
		t.Errorf("runtime() = %q, want %q", got, want)
	}
}

func TestRuntime_FallsBackToTempDirWithUID(t *testing.T) {
	withEnv(t, "XDG_RUNTIME_DIR", "")

	got := runtime()
	suffix := fmt.Sprintf("jind-ai-%d", os.Getuid())
	want := filepath.Join(os.TempDir(), suffix)
	if got != want {
		t.Errorf("runtime() = %q, want %q", got, want)
	}
}

func TestSocket_IsRuntimePlusDaemonSock(t *testing.T) {
	withEnv(t, "XDG_RUNTIME_DIR", "/run/user/1000")

	got := Socket()
	if !strings.HasSuffix(got, filepath.Join("jind-ai", "daemon.sock")) {
		t.Errorf("Socket() = %q, expected suffix jind-ai/daemon.sock", got)
	}
}

func TestRemoteDefaultSocket_IsFixed(t *testing.T) {
	const want = "~/.local/state/jind-ai/daemon.sock"
	if got := RemoteDefaultSocket(); got != want {
		t.Errorf("RemoteDefaultSocket() = %q, want %q", got, want)
	}
}

func TestRemoteDefaultSocketRel_IsFixed(t *testing.T) {
	const want = ".local/state/jind-ai/daemon.sock"
	if got := RemoteDefaultSocketRel(); got != want {
		t.Errorf("RemoteDefaultSocketRel() = %q, want %q", got, want)
	}
}

func TestRemoteStateDirRel_IsFixed(t *testing.T) {
	const want = ".local/state/jind-ai"
	if got := RemoteStateDirRel(); got != want {
		t.Errorf("RemoteStateDirRel() = %q, want %q", got, want)
	}
}

func TestStateOrEmpty_SuccessWithXDG(t *testing.T) {
	withEnv(t, "XDG_STATE_HOME", "/tmp/cc-state")

	dir, ok := StateOrEmpty()
	if !ok {
		t.Fatal("StateOrEmpty() ok=false, want true")
	}
	want := filepath.Join("/tmp/cc-state", "jind-ai")
	if dir != want {
		t.Errorf("StateOrEmpty() dir = %q, want %q", dir, want)
	}
}

// TestStateOrEmpty_FailsSilentlyWhenHomeUnresolvable documents that the
// non-panicking variant returns ok=false instead of panicking so best-effort
// callers (e.g. debug.NewLogger) can degrade to a no-op.
func TestStateOrEmpty_FailsSilentlyWhenHomeUnresolvable(t *testing.T) {
	withEnv(t, "XDG_STATE_HOME", "")
	withEnv(t, "HOME", "")
	withEnv(t, "USERPROFILE", "")

	dir, ok := StateOrEmpty()
	if ok {
		t.Errorf("StateOrEmpty() ok=true, want false (dir=%q)", dir)
	}
	if dir != "" {
		t.Errorf("StateOrEmpty() dir = %q, want \"\"", dir)
	}
}

// TestMustHome_PanicsWhenHomeUnresolvable documents the contract: helpers that
// rely on $HOME (with no XDG_* override) panic rather than silently returning
// a relative path. Set both HOME and USERPROFILE to empty so os.UserHomeDir
// fails on every supported OS.
func TestMustHome_PanicsWhenHomeUnresolvable(t *testing.T) {
	withEnv(t, "HOME", "")
	withEnv(t, "USERPROFILE", "")
	withEnv(t, "XDG_CONFIG_HOME", "")

	defer func() {
		if recover() == nil {
			t.Fatal("expected Config() to panic when home cannot be resolved")
		}
	}()
	_ = Config()
}
