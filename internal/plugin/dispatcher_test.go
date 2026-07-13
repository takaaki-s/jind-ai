package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// installTestPlugin writes a plugin directory with the given manifest and
// registers it in the lock file, mirroring what `jin plugin install` does.
func installTestPlugin(t *testing.T, pluginsDir, stateDir, name, body string) {
	t.Helper()
	dir := filepath.Join(pluginsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.Filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Set(name, LockEntry{Source: "test", InstalledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func newTestDispatcher(t *testing.T, cfg config.PluginsConfig) (*EventDispatcher, string, string) {
	t.Helper()
	pluginsDir := t.TempDir()
	stateDir := t.TempDir()
	reg := NewRegistry(pluginsDir, stateDir, cfg)
	d := NewDispatcher(reg, pluginsDir, stateDir, "/tmp/test.sock", 500*time.Millisecond, nil)
	return d, pluginsDir, stateDir
}

// waitForFile polls until path exists or the deadline passes.
func waitForFile(t *testing.T, path string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitForLines polls until path contains want non-empty lines. It keeps
// polling a little after the count is reached to catch overshoot.
func waitForLines(t *testing.T, path string, want int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countLines(path) >= want {
			time.Sleep(200 * time.Millisecond)
			return countLines(path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return countLines(path)
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// dumpManifestFor renders a runtime manifest that prepares the plugin dir so
// the dispatcher will exec its entrypoint. The entrypoint is a bash script
// (chmod +x set below via the plugin dir setup) that writes JIN_PLUGIN_DEPTH
// to out.txt.
const dumpManifest = `schema_version: 1
name: dumper
version: 0.1.0
description: dumps depth
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: /bin/bash
on:
  - status_changed:idle
`

// Because the runtime dispatcher execs the entrypoint via `bash -c`, we can
// point entrypoint at a shell fragment when the fixture wants one. Below,
// `entrypoint: bash -c 'echo ...'` uses that fact.

const dumpEntrypointRuntime = `schema_version: 1
name: dumper
version: 0.1.0
description: dumps depth
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: bash -c 'echo "$JIN_PLUGIN_DEPTH" >> out.txt'
on:
  - status_changed:idle
`

func idleEvent() Event {
	return Event{Name: manifest.EventStatusChanged, SessionID: "sess-1", Status: "idle", PrevStatus: "thinking"}
}

func TestPublishFiresMatchingPlugin(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	d.Publish(idleEvent())

	out := filepath.Join(pluginsDir, "dumper", "out.txt")
	if !waitForFile(t, out) {
		t.Fatal("plugin did not run for matching event")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Errorf("JIN_PLUGIN_DEPTH = %q, want 1", got)
	}
}

func TestPublishSkipsNonMatchingEvent(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	d.Publish(Event{Name: manifest.EventStatusChanged, SessionID: "sess-1", Status: "thinking"})

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(pluginsDir, "dumper", "out.txt")); err == nil {
		t.Fatal("plugin ran for non-matching event")
	}
}

func TestPublishDebouncesSameEvent(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	d.Publish(idleEvent())
	d.Publish(idleEvent())

	out := filepath.Join(pluginsDir, "dumper", "out.txt")
	if got := waitForLines(t, out, 1); got != 1 {
		t.Errorf("plugin ran %d times within debounce window, want 1", got)
	}
}

func TestPublishSkipsDisabledPlugin(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{Disabled: []string{"dumper"}})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	d.Publish(idleEvent())

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(pluginsDir, "dumper", "out.txt")); err == nil {
		t.Fatal("disabled plugin ran")
	}
}

func TestPublishSkipsIncompatibleAndWarnsOnce(t *testing.T) {
	restore := setJinVersionForTest(t, "0.5.0")
	defer restore()

	incompat := strings.Replace(dumpEntrypointRuntime, `jin: ">=0.0.0"`, `jin: ">=99.0.0"`, 1)

	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", incompat)

	d.Publish(idleEvent())
	d.Publish(Event{Name: manifest.EventStatusChanged, SessionID: "sess-2", Status: "idle"})

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(pluginsDir, "dumper", "out.txt")); err == nil {
		t.Fatal("incompatible plugin ran")
	}
	d.mu.Lock()
	warned := len(d.warned)
	d.mu.Unlock()
	if warned != 1 {
		t.Errorf("warned entries = %d, want exactly 1 (warn-once per plugin+reason)", warned)
	}
}

func TestRunActionBypassesMatcherAndDebounce(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	ev := Event{Name: "action", SessionID: "sess-1", Status: "idle"}
	if err := d.RunAction("dumper", ev, 0, ActionContext{}); err != nil {
		t.Fatal(err)
	}
	if err := d.RunAction("dumper", ev, 0, ActionContext{}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(pluginsDir, "dumper", "out.txt")
	if got := waitForLines(t, out, 2); got != 2 {
		t.Errorf("RunAction ran %d times, want 2 (no debounce)", got)
	}
}

const callerDumpManifest = `schema_version: 1
name: callerdump
version: 0.1.0
description: dumps caller context
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: bash -c 'echo "sock=${JIN_CALLER_TMUX_SOCKET-unset} pane=${JIN_CALLER_TMUX_PANE-unset} sid=$JIN_SESSION_ID" >> out.txt'
on: []
`

// A global action (empty session fields) must still run, carrying the caller's
// tmux context as env vars; an event-driven-style run without caller context
// must leave those vars entirely unset (not empty) so plugins can ${VAR:-...}.
func TestRunActionGlobalWithCallerContext(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "callerdump", callerDumpManifest)

	global := Event{Name: "action"}
	actx := ActionContext{TmuxSocket: "/tmp/tmux-1000/default", TmuxPane: "%3"}
	if err := d.RunAction("callerdump", global, 0, actx); err != nil {
		t.Fatal(err)
	}
	if err := d.RunAction("callerdump", global, 0, ActionContext{}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(pluginsDir, "callerdump", "out.txt")
	if got := waitForLines(t, out, 2); got != 2 {
		t.Fatalf("RunAction ran %d times, want 2", got)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "sock=/tmp/tmux-1000/default pane=%3 sid=") {
		t.Errorf("caller-context run output = %q, want caller vars set and empty session id", got)
	}
	if !strings.Contains(got, "sock=unset pane=unset sid=") {
		t.Errorf("no-context run output = %q, want JIN_CALLER_TMUX_* unset", got)
	}
}

func TestPassDebouncePrunesExpiredEntries(t *testing.T) {
	d, _, _ := newTestDispatcher(t, config.PluginsConfig{})

	// Fill past the prune threshold with entries whose window has long expired.
	d.mu.Lock()
	for i := 0; i < debouncePruneThreshold; i++ {
		d.lastFired[fmt.Sprintf("stale-%d", i)] = time.Now().Add(-time.Hour)
	}
	d.mu.Unlock()

	if !d.passDebounce("dumper", idleEvent()) {
		t.Fatal("fresh event should pass debounce")
	}

	d.mu.Lock()
	size := len(d.lastFired)
	d.mu.Unlock()
	if size != 1 {
		t.Errorf("lastFired size after prune = %d, want 1 (stale entries swept)", size)
	}
}

func TestRunActionRejectsDepthLimit(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	err := d.RunAction("dumper", idleEvent(), 1, ActionContext{})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Errorf("RunAction(depth=1) = %v, want depth limit error", err)
	}
}

func TestRunActionErrors(t *testing.T) {
	restore := setJinVersionForTest(t, "0.5.0")
	defer restore()

	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{Disabled: []string{"off"}})
	off := strings.Replace(dumpEntrypointRuntime, "name: dumper", "name: off", 1)
	old := strings.Replace(strings.Replace(dumpEntrypointRuntime, "name: dumper", "name: old", 1),
		`jin: ">=0.0.0"`, `jin: ">=99.0.0"`, 1)
	installTestPlugin(t, pluginsDir, stateDir, "off", off)
	installTestPlugin(t, pluginsDir, stateDir, "old", old)

	if err := d.RunAction("missing", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing plugin: %v, want not installed", err)
	}
	if err := d.RunAction("off", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled plugin: %v, want disabled", err)
	}
	if err := d.RunAction("old", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "jin plugin update") {
		t.Errorf("incompatible plugin: %v, want update hint", err)
	}
}

func TestNewDispatcher_NilResolver_UsesDefault(t *testing.T) {
	pluginsDir := t.TempDir()
	stateDir := t.TempDir()
	reg := NewRegistry(pluginsDir, stateDir, config.PluginsConfig{})

	d := NewDispatcher(reg, pluginsDir, stateDir, "/tmp/test.sock", 500*time.Millisecond, nil)

	w, h := d.popupResolver("any-plugin", nil)
	if w != "" || h != "" {
		t.Errorf("default popupResolver returned %q/%q, want empty/empty", w, h)
	}
}

func TestDispatcher_CallsPopupResolver_WithManifestPopup(t *testing.T) {
	pluginsDir := t.TempDir()
	stateDir := t.TempDir()

	var gotName string
	var gotPopup *manifest.PopupConfig
	resolver := func(name string, popup *manifest.PopupConfig) (string, string) {
		gotName = name
		gotPopup = popup
		return "42%", "24%"
	}

	reg := NewRegistry(pluginsDir, stateDir, config.PluginsConfig{})
	d := NewDispatcher(reg, pluginsDir, stateDir, "/tmp/test.sock", 500*time.Millisecond, resolver)

	envDump := filepath.Join(pluginsDir, "envcap", "env.txt")
	body := fmt.Sprintf(`schema_version: 1
name: envcap
version: 0.1.0
description: capture popup env
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: bash -c 'env | grep JIN_PLUGIN_POPUP > %s'
on:
  - status_changed:idle
popup:
  width: 40
  height: 20
`, envDump)
	installTestPlugin(t, pluginsDir, stateDir, "envcap", body)

	d.Publish(idleEvent())

	if !waitForFile(t, envDump) {
		t.Fatal("plugin did not run")
	}
	data, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "envcap" {
		t.Errorf("resolver got name=%q, want envcap", gotName)
	}
	if gotPopup == nil || gotPopup.Width != 40 || gotPopup.Height != 20 {
		t.Errorf("resolver got popup=%+v, want {40, 20}", gotPopup)
	}
	env := string(data)
	if !strings.Contains(env, "JIN_PLUGIN_POPUP_WIDTH=42%") {
		t.Errorf("plugin env missing JIN_PLUGIN_POPUP_WIDTH=42%%; env:\n%s", env)
	}
	if !strings.Contains(env, "JIN_PLUGIN_POPUP_HEIGHT=24%") {
		t.Errorf("plugin env missing JIN_PLUGIN_POPUP_HEIGHT=24%%; env:\n%s", env)
	}
}

// Silence unused variable warning for the pure-entrypoint dumpManifest
// variant kept for reference; the runtime variant is used above.
var _ = dumpManifest
