package cmd

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

const pluginTestManifest = `schema_version: 1
name: notifier
version: 0.1.0
description: end-to-end plugin CLI fixture
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: ./notify.sh
on:
  - status_changed
`

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
	if err := os.WriteFile(filepath.Join(dir, "jind-ai-plugin.yaml"), []byte(pluginTestManifest), 0o644); err != nil {
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
	if !strings.Contains(listOut, "end-to-end plugin CLI fixture") {
		t.Errorf("expected manifest description in list, got %q", listOut)
	}
	if !strings.Contains(listOut, "DESCRIPTION") {
		t.Errorf("expected DESCRIPTION column header in list, got %q", listOut)
	}

	upOut, err := runPluginCmd(t, "update", "--yes", "notifier")
	if err != nil {
		t.Fatalf("update: err=%v, out=%q", err, upOut)
	}
	if !strings.Contains(upOut, "already up to date") {
		t.Errorf("expected 'already up to date', got %q", upOut)
	}
}

// startFakePluginDaemon listens on a unix socket standing in for the daemon,
// records every plugin-run request it receives, and answers Success. The
// socket is wired into the CLI through JIN_SOCKET (see getSocketPath), and
// the returned accessor snapshots the recorded requests under the lock.
func startFakePluginDaemon(t *testing.T) func() []daemon.PluginRunRequest {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("JIN_SOCKET", sock)

	var mu sync.Mutex
	var reqs []daemon.PluginRunRequest
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req daemon.Request
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				if req.Action == "plugin-run" {
					var pr daemon.PluginRunRequest
					if json.Unmarshal(req.Data, &pr) == nil {
						mu.Lock()
						reqs = append(reqs, pr)
						mu.Unlock()
					}
				}
				_ = json.NewEncoder(c).Encode(daemon.Response{ProtocolVersion: daemon.ProtocolVersion, Success: true})
			}(conn)
		}
	}()
	return func() []daemon.PluginRunRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]daemon.PluginRunRequest(nil), reqs...)
	}
}

// The action argument is optional: omitted it must reach the daemon as the
// empty string (default action) and keep the historical success message;
// given explicitly it must ride the request and appear as name:action.
func TestRunPluginRun_ActionArg(t *testing.T) {
	getReqs := startFakePluginDaemon(t)

	out, err := runPluginCmd(t, "run", "notifier")
	if err != nil {
		t.Fatalf("run without action: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Started plugin notifier (global)") {
		t.Errorf("default-action output = %q, want 'Started plugin notifier (global)'", out)
	}

	out, err = runPluginCmd(t, "run", "notifier", "send-dm")
	if err != nil {
		t.Fatalf("run with action: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Started plugin notifier:send-dm (global)") {
		t.Errorf("explicit-action output = %q, want 'Started plugin notifier:send-dm (global)'", out)
	}

	reqs := getReqs()
	if len(reqs) != 2 {
		t.Fatalf("daemon received %d plugin-run requests, want 2", len(reqs))
	}
	if reqs[0].Plugin != "notifier" || reqs[0].Action != "" {
		t.Errorf("default run request = %+v, want Plugin=notifier Action=\"\"", reqs[0])
	}
	if reqs[1].Plugin != "notifier" || reqs[1].Action != "send-dm" {
		t.Errorf("explicit run request = %+v, want Plugin=notifier Action=send-dm", reqs[1])
	}
}

// cobra owns the arity check: zero and three arguments must fail before
// runPluginRun ever contacts the daemon.
func TestRunPluginRun_ArgCount(t *testing.T) {
	getReqs := startFakePluginDaemon(t)

	if out, err := runPluginCmd(t, "run"); err == nil {
		t.Errorf("run with 0 args should fail, out=%q", out)
	}
	if out, err := runPluginCmd(t, "run", "a", "b", "c"); err == nil {
		t.Errorf("run with 3 args should fail, out=%q", out)
	}
	if got := len(getReqs()); got != 0 {
		t.Errorf("daemon received %d requests, want 0 (arity errors are client-side)", got)
	}
}

const multiActionTestManifest = `schema_version: 2
name: multi
version: 0.1.0
description: completion fixture
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
actions:
  - id: notify
    entrypoint: ./notify.sh
  - id: send-dm
    entrypoint: ./send-dm.sh
`

// installPluginFixture places a manifest under the plugins dir and registers
// it in the lock file, mirroring an installed plugin without going through
// the git clone path. Callers must have pointed XDG_* at temp dirs first.
func installPluginFixture(t *testing.T, name, manifestBody string) {
	t.Helper()
	dir := filepath.Join(paths.Plugins(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.Filename), []byte(manifestBody), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.LoadLock(getStateDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Set(name, plugin.LockEntry{Source: "test", InstalledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// Two-stage completion: position 0 completes installed plugin names,
// position 1 completes that plugin's action IDs (prefix-filtered), and
// anything later — or an unknown plugin — completes nothing.
func TestCompletePluginRunArgs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	installPluginFixture(t, "multi", multiActionTestManifest)

	names, _ := completePluginRunArgs(pluginRunCmd, nil, "mu")
	if len(names) != 1 || names[0] != "multi" {
		t.Errorf("stage-0 completion = %v, want [multi]", names)
	}

	ids, _ := completePluginRunArgs(pluginRunCmd, []string{"multi"}, "")
	if len(ids) != 2 || ids[0] != "notify" || ids[1] != "send-dm" {
		t.Errorf("stage-1 completion = %v, want [notify send-dm] in declaration order", ids)
	}

	filtered, _ := completePluginRunArgs(pluginRunCmd, []string{"multi"}, "se")
	if len(filtered) != 1 || filtered[0] != "send-dm" {
		t.Errorf("stage-1 prefix completion = %v, want [send-dm]", filtered)
	}

	if extra, _ := completePluginRunArgs(pluginRunCmd, []string{"multi", "notify"}, ""); extra != nil {
		t.Errorf("stage-2 completion = %v, want nil", extra)
	}

	if unknown, _ := completePluginRunArgs(pluginRunCmd, []string{"nope"}, ""); len(unknown) != 0 {
		t.Errorf("unknown plugin completion = %v, want empty", unknown)
	}
}

// Listener actions are event-only endpoints: they must not appear in shell
// completion (so users are not nudged toward invoking them by tab-completion)
// even though direct invocation stays legal for debugging.
func TestCompletePluginRunArgs_OmitsListenerActions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	installPluginFixture(t, "notifier", `schema_version: 2
name: notifier
version: 0.1.0
description: listener completion fixture
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
actions:
  - id: list
    entrypoint: ./list.sh
  - id: listen
    entrypoint: ./listen.sh
    on: [status_changed]
    listener: true
`)

	ids, _ := completePluginRunArgs(pluginRunCmd, []string{"notifier"}, "")
	if len(ids) != 1 || ids[0] != "list" {
		t.Errorf("completion = %v, want [list] (listener omitted)", ids)
	}
	if filtered, _ := completePluginRunArgs(pluginRunCmd, []string{"notifier"}, "lis"); len(filtered) != 1 || filtered[0] != "list" {
		t.Errorf("prefix completion for %q = %v, want [list] (listener omitted even when prefix matches)", "lis", filtered)
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
