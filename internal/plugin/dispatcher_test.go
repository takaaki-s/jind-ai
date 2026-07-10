package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/config"
)

// installTestPlugin writes a plugin directory with the given manifest and
// registers it in the lock file, mirroring what `jin plugin install` does.
func installTestPlugin(t *testing.T, pluginsDir, stateDir, name, manifest string) {
	t.Helper()
	dir := filepath.Join(pluginsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFilename), []byte(manifest), 0o644); err != nil {
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
	d := NewDispatcher(reg, pluginsDir, stateDir, "/tmp/test.sock", 500*time.Millisecond)
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

const dumpManifest = `name: dumper
api_version: 1
on: ["status_changed:idle"]
run: echo "$JIN_PLUGIN_DEPTH" >> out.txt
`

func idleEvent() Event {
	return Event{Name: EventStatusChanged, SessionID: "sess-1", Status: "idle", PrevStatus: "thinking"}
}

func TestPublishFiresMatchingPlugin(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpManifest)

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
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpManifest)

	d.Publish(Event{Name: EventStatusChanged, SessionID: "sess-1", Status: "thinking"})

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(pluginsDir, "dumper", "out.txt")); err == nil {
		t.Fatal("plugin ran for non-matching event")
	}
}

func TestPublishDebouncesSameEvent(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpManifest)

	d.Publish(idleEvent())
	d.Publish(idleEvent())

	out := filepath.Join(pluginsDir, "dumper", "out.txt")
	if got := waitForLines(t, out, 1); got != 1 {
		t.Errorf("plugin ran %d times within debounce window, want 1", got)
	}
}

func TestPublishSkipsDisabledPlugin(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{Disabled: []string{"dumper"}})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpManifest)

	d.Publish(idleEvent())

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(pluginsDir, "dumper", "out.txt")); err == nil {
		t.Fatal("disabled plugin ran")
	}
}

func TestPublishSkipsIncompatibleAndWarnsOnce(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", strings.Replace(dumpManifest, "api_version: 1", "api_version: 999", 1))

	d.Publish(idleEvent())
	d.Publish(Event{Name: EventStatusChanged, SessionID: "sess-2", Status: "idle"})

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
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpManifest)

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

const callerDumpManifest = `name: callerdump
api_version: 1
on: []
run: echo "sock=${JIN_CALLER_TMUX_SOCKET-unset} pane=${JIN_CALLER_TMUX_PANE-unset} sid=$JIN_SESSION_ID" >> out.txt
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
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpManifest)

	err := d.RunAction("dumper", idleEvent(), 1, ActionContext{})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Errorf("RunAction(depth=1) = %v, want depth limit error", err)
	}
}

func TestRunActionErrors(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{Disabled: []string{"off"}})
	installTestPlugin(t, pluginsDir, stateDir, "off", strings.Replace(dumpManifest, "name: dumper", "name: off", 1))
	installTestPlugin(t, pluginsDir, stateDir, "old", strings.Replace(strings.Replace(dumpManifest, "name: dumper", "name: old", 1), "api_version: 1", "api_version: 999", 1))

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
