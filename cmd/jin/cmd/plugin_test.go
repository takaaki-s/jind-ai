package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pluginTestManifest = "name: notifier\napi_version: 1\nrun: ./notify.sh\non:\n  - status_changed\n"

// runPluginGit runs a git subcommand in dir with a hermetic environment (no
// global/system config, no credential prompts) so the fixture behaves the same
// on any host.
func runPluginGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// initPluginRepo creates a git repo holding a valid manifest and returns its
// path, usable as a file:// clone source.
func initPluginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runPluginGit(t, dir, "init", "-b", "main")
	runPluginGit(t, dir, "config", "user.email", "test@example.com")
	runPluginGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "jin-plugin.yaml"), []byte(pluginTestManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	runPluginGit(t, dir, "add", ".")
	runPluginGit(t, dir, "commit", "-m", "initial")
	return dir
}

// runPluginCmd invokes the plugin subcommand tree with captured I/O. It resets
// flags and the --json global before each run so cobra's retained state does
// not leak between tests.
func runPluginCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	_ = pluginInstallCmd.Flags().Set("yes", "false")
	_ = pluginInstallCmd.Flags().Set("link", "")
	_ = pluginUpdateCmd.Flags().Set("yes", "false")
	jsonOutput = false

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetIn(strings.NewReader(""))
	rootCmd.SetArgs(append([]string{"plugin"}, args...))
	err := rootCmd.Execute()
	return buf.String(), err
}

func TestPluginInstallListUpdate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	source := "file://" + initPluginRepo(t)

	out, err := runPluginCmd(t, "install", "--yes", source)
	if err != nil {
		t.Fatalf("install: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Installed notifier @ ") {
		t.Errorf("expected install confirmation, got %q", out)
	}

	listOut, err := runPluginCmd(t, "list")
	if err != nil {
		t.Fatalf("list: err=%v, out=%q", err, listOut)
	}
	if !strings.Contains(listOut, "notifier") || !strings.Contains(listOut, "enabled") {
		t.Errorf("expected notifier enabled in list, got %q", listOut)
	}
	if !strings.Contains(listOut, source) {
		t.Errorf("expected source %q in list, got %q", source, listOut)
	}

	upOut, err := runPluginCmd(t, "update", "--yes", "notifier")
	if err != nil {
		t.Fatalf("update: err=%v, out=%q", err, upOut)
	}
	if !strings.Contains(upOut, "already up to date") {
		t.Errorf("expected 'already up to date', got %q", upOut)
	}
}

// A --session flag that was never passed is a global action (no resolution, no
// daemon contact — hence the nil client); an explicitly empty --session must
// instead fail through the usual selector validation, never silently global.
func TestResolvePluginRunSession(t *testing.T) {
	id, desc, err := resolvePluginRunSession(nil, false, "")
	if id != "" || desc != "" || err != nil {
		t.Errorf("unchanged flag = (%q, %q, %v), want empty target and nil error", id, desc, err)
	}

	if _, _, err := resolvePluginRunSession(nil, true, ""); err == nil || !strings.Contains(err.Error(), "selector is required") {
		t.Errorf("explicit empty selector error = %v, want 'selector is required'", err)
	}
}
