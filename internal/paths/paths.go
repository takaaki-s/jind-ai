// Package paths resolves jind-ai's data directories according to the
// XDG Base Directory Specification.
//
// Defaults (when the corresponding XDG_* env var is not set):
//
//	config:  $HOME/.config/jind-ai
//	state:   $HOME/.local/state/jind-ai
//	data:    $HOME/.local/share/jind-ai
//	runtime: os.TempDir()/jind-ai-<uid>
//
// The remote-host default socket path is fixed at ~/.local/state/jind-ai/daemon.sock
// because $XDG_RUNTIME_DIR cannot be reliably resolved across SSH.
//
// If the user's home directory cannot be resolved (and no relevant XDG_* env var
// is set), the helpers panic — the daemon and CLI cannot operate sanely without
// a home directory, and a relative path would silently write to the caller's CWD.
// Best-effort consumers that should degrade gracefully (e.g. debug loggers) can
// use the *OrEmpty variants instead.
//
// The runtime directory is intentionally not exposed directly; it is only
// reachable through Socket() so the 0700 permission requirement of
// $XDG_RUNTIME_DIR is enforced in one place (the daemon).
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const appName = "jind-ai"

// Config returns the directory for user configuration files
// ($XDG_CONFIG_HOME/jind-ai, default ~/.config/jind-ai).
func Config() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, appName)
	}
	return filepath.Join(mustHome(), ".config", appName)
}

// State returns the directory for persistent state files
// ($XDG_STATE_HOME/jind-ai, default ~/.local/state/jind-ai).
func State() string {
	dir, ok := stateOrEmpty()
	if !ok {
		panic("jind-ai/paths: cannot resolve state dir: $XDG_STATE_HOME unset and $HOME unresolvable")
	}
	return dir
}

// StateOrEmpty returns the same value as State along with ok=true on success.
// On failure (XDG_STATE_HOME unset and home directory cannot be resolved) it
// returns ("", false) instead of panicking. Use for best-effort consumers like
// debug loggers that should silently no-op when home is missing.
func StateOrEmpty() (string, bool) {
	return stateOrEmpty()
}

func stateOrEmpty() (string, bool) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, appName), true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".local", "state", appName), true
}

// Sessions returns the directory holding per-session JSON files.
func Sessions() string {
	return filepath.Join(State(), "sessions")
}

// Data returns the directory for user-installed data files
// ($XDG_DATA_HOME/jind-ai, default ~/.local/share/jind-ai).
func Data() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, appName)
	}
	return filepath.Join(mustHome(), ".local", "share", appName)
}

// Plugins returns the directory holding installed plugins.
func Plugins() string {
	return filepath.Join(Data(), "plugins")
}

// runtime returns the directory for ephemeral runtime files
// ($XDG_RUNTIME_DIR/jind-ai, fallback os.TempDir()/jind-ai-<uid>).
//
// Not exported: callers should obtain Socket() instead. XDG_RUNTIME_DIR
// requires 0700 access — sealing this behind Socket avoids accidental
// world-readable directory creations.
func runtime() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, appName)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", appName, os.Getuid()))
}

// Socket returns the default local daemon socket path.
func Socket() string {
	return filepath.Join(runtime(), "daemon.sock")
}

// RemoteStateDirRel returns the path of the remote-side state directory
// relative to the remote $HOME (no leading "~/"). Used as the common prefix
// for remote artifacts that have to share a layout (socket, ssh-agent symlink).
func RemoteStateDirRel() string {
	return ".local/state/" + appName
}

// RemoteDefaultSocket returns the default socket path used by slave daemons
// on remote hosts. The leading "~" is expanded by the remote shell.
func RemoteDefaultSocket() string {
	return "~/" + RemoteDefaultSocketRel()
}

// RemoteDefaultSocketRel returns the default remote socket path relative to
// the remote $HOME (no leading "~/"). Use this when appending to a remote-side
// $HOME resolved via SSH.
func RemoteDefaultSocketRel() string {
	return RemoteStateDirRel() + "/daemon.sock"
}

// mustHome resolves the user's home directory or panics. Used by helpers that
// must return an absolute path; a relative path would silently write to the
// caller's CWD.
func mustHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		panic(fmt.Sprintf("jind-ai/paths: cannot resolve home directory: %v", err))
	}
	return home
}
