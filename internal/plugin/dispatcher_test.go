package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	d := NewDispatcher(reg, pluginsDir, stateDir, testIdentity(), 500*time.Millisecond, nil)
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

// dumpEntrypointRuntime is a fixture manifest whose entrypoint is itself a
// bash fragment. The runtime execs the entrypoint via `bash -c`, so any
// shell-parseable string works — here it appends JIN_PLUGIN_DEPTH to
// out.txt, giving tests a cheap way to observe both that the plugin ran
// and what depth it saw.
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
	if err := d.RunAction("dumper", "", ev, 0, ActionContext{}); err != nil {
		t.Fatal(err)
	}
	if err := d.RunAction("dumper", "", ev, 0, ActionContext{}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(pluginsDir, "dumper", "out.txt")
	if got := waitForLines(t, out, 2); got != 2 {
		t.Errorf("RunAction ran %d times, want 2 (no debounce)", got)
	}
}

const identityDumpManifest = `schema_version: 1
name: identdump
version: 0.1.0
description: dumps the identity it was handed
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: bash -c 'echo "sock=$JIN_SOCKET bin=$JIN_BIN debug=${JIN_DEBUG-unset}" >> out.txt'
on:
  - status_changed:idle
`

// TestPublishHandsThePluginTheIdentityItWasBuiltWith pins the whole chain a
// plugin's callback depends on: the identity given to NewDispatcher is the one
// that reaches the plugin's environment.
//
// Every value is compared exactly rather than for presence. JIN_BIN is why:
// os.Executable() of the dispatching process — what this used to render — is
// also a non-empty path, and session.EstablishHookBinary has what goes wrong
// when a child gets it.
func TestPublishHandsThePluginTheIdentityItWasBuiltWith(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "identdump", identityDumpManifest)

	d.Publish(idleEvent())

	out := filepath.Join(pluginsDir, "identdump", "out.txt")
	if !waitForFile(t, out) {
		t.Fatal("plugin did not run")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity()
	debug := "unset"
	if id.Debug {
		debug = "1"
	}
	want := fmt.Sprintf("sock=%s bin=%s debug=%s", id.SocketPath, id.BinPath, debug)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("plugin saw %q, want %q", got, want)
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
	if err := d.RunAction("callerdump", "", global, 0, actx); err != nil {
		t.Fatal(err)
	}
	if err := d.RunAction("callerdump", "", global, 0, ActionContext{}); err != nil {
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

	if !d.passDebounce("dumper", "default", idleEvent()) {
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

	err := d.RunAction("dumper", "", idleEvent(), 1, ActionContext{})
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

	if err := d.RunAction("missing", "", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing plugin: %v, want not installed", err)
	}
	if err := d.RunAction("off", "", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled plugin: %v, want disabled", err)
	}
	if err := d.RunAction("old", "", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "jin plugin update") {
		t.Errorf("incompatible plugin: %v, want update hint", err)
	}
}

func TestRunHandoff_RequiresCapabilityAndReturnsProviderJSON(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	manifestBody := v2Manifest("fake-pr", "fake handoff provider", `  - id: create
    entrypoint: |
      cat > request.json
      printf '{"status":"succeeded","id":"42"}'
    handoff: true
  - id: ordinary
    entrypoint: 'true'
`)
	installTestPlugin(t, pluginsDir, stateDir, "fake-pr", manifestBody)
	if _, err := d.ResolveHandoff("fake-pr", "ordinary"); err == nil || !strings.Contains(err.Error(), "not a handoff provider") {
		t.Fatalf("ordinary action error = %v", err)
	}
	if err := d.RunAction("fake-pr", "create", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "session pr-handoff") {
		t.Fatalf("ordinary run of handoff action = %v", err)
	}
	actionID, err := d.ResolveHandoff("fake-pr", "create")
	if err != nil || actionID != "create" {
		t.Fatalf("resolve = %q, %v", actionID, err)
	}
	payload := []byte(`{"schema_version":1,"kind":"pull-request"}`)
	out, err := d.RunHandoff("fake-pr", "create", "stable-key", idleEvent(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"status":"succeeded"`) {
		t.Fatalf("provider output = %q", out)
	}
	request, err := os.ReadFile(filepath.Join(pluginsDir, "fake-pr", "request.json"))
	if err != nil || string(request) != string(payload) {
		t.Fatalf("provider request = %q, %v", request, err)
	}
}

func TestRunMergeHandoff_RequiresCapabilityAndSupportsTwoPhases(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	manifestBody := v2Manifest("fake-merge", "fake merge provider", `  - id: merge
    entrypoint: |
      body=$(cat)
      printf '%s' "$body" > request.json
      case "$body" in
        *'"operation":"preflight"'*) printf '{"status":"ready"}' ;;
        *) printf '{"status":"succeeded"}' ;;
      esac
    merge_handoff: true
  - id: ordinary
    entrypoint: 'true'
`)
	installTestPlugin(t, pluginsDir, stateDir, "fake-merge", manifestBody)
	if _, err := d.ResolveMergeHandoff("fake-merge", "ordinary"); err == nil || !strings.Contains(err.Error(), "not a merge handoff provider") {
		t.Fatalf("ordinary action error = %v", err)
	}
	if err := d.RunAction("fake-merge", "merge", idleEvent(), 0, ActionContext{}); err == nil || !strings.Contains(err.Error(), "session merge-handoff") {
		t.Fatalf("ordinary run error = %v", err)
	}
	actionID, err := d.ResolveMergeHandoff("fake-merge", "merge")
	if err != nil || actionID != "merge" {
		t.Fatalf("resolve = %q, %v", actionID, err)
	}
	preflightPayload := []byte(`{"operation":"preflight"}`)
	out, err := d.RunMergeHandoff("fake-merge", "merge", "stable-key", idleEvent(), preflightPayload)
	if err != nil || !strings.Contains(string(out), `"status":"ready"`) {
		t.Fatalf("preflight output=%q err=%v", out, err)
	}
	mergePayload := []byte(`{"operation":"merge"}`)
	out, err = d.RunMergeHandoff("fake-merge", "merge", "stable-key", idleEvent(), mergePayload)
	if err != nil || !strings.Contains(string(out), `"status":"succeeded"`) {
		t.Fatalf("merge output=%q err=%v", out, err)
	}
}

func TestNewDispatcher_NilResolver_UsesDefault(t *testing.T) {
	pluginsDir := t.TempDir()
	stateDir := t.TempDir()
	reg := NewRegistry(pluginsDir, stateDir, config.PluginsConfig{})

	d := NewDispatcher(reg, pluginsDir, stateDir, testIdentity(), 500*time.Millisecond, nil)

	w, h := d.popupResolver("any-plugin", "any-action", nil)
	if w != "" || h != "" {
		t.Errorf("default popupResolver returned %q/%q, want empty/empty", w, h)
	}
}

func TestDispatcher_CallsPopupResolver_WithManifestPopup(t *testing.T) {
	pluginsDir := t.TempDir()
	stateDir := t.TempDir()

	var gotName, gotActionID string
	var gotPopup *manifest.PopupConfig
	resolver := func(name, actionID string, popup *manifest.PopupConfig) (string, string) {
		gotName = name
		gotActionID = actionID
		gotPopup = popup
		return "42%", "24%"
	}

	reg := NewRegistry(pluginsDir, stateDir, config.PluginsConfig{})
	d := NewDispatcher(reg, pluginsDir, stateDir, testIdentity(), 500*time.Millisecond, resolver)

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

	// Wait for both env lines, not just for the file: the entrypoint's `>`
	// redirection creates envDump before `env | grep` writes a byte into it,
	// so waitForFile alone can hand us an empty file and fail the assertions
	// below with an empty env dump.
	if got := waitForLines(t, envDump, 2); got != 2 {
		t.Fatalf("plugin wrote %d env lines, want 2 (WIDTH and HEIGHT)", got)
	}
	data, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "envcap" {
		t.Errorf("resolver got name=%q, want envcap", gotName)
	}
	if gotActionID != "default" {
		t.Errorf("resolver got actionID=%q, want default (v1 normalize)", gotActionID)
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

// v2Manifest renders a schema_version 2 fixture: the header (semver,
// permissive jin range, no-op build) is shared by every v2 fixture in this
// file so each test declares only its name and actions block (indented YAML
// starting at "  - id: ...").
func v2Manifest(name, description, actions string) string {
	return fmt.Sprintf(`schema_version: 2
name: %s
version: 0.1.0
description: %s
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
actions:
%s`, name, description, actions)
}

// twoActionManifest is a v2 fixture with two actions on disjoint matchers:
// on-idle appends to idle.txt for status_changed:idle, on-thinking appends to
// thinking.txt for status_changed:thinking. Each line carries $JIN_ACTION_ID
// so tests can also assert which action identity the run saw.
var twoActionManifest = v2Manifest("multi", "two independent actions", `  - id: on-idle
    entrypoint: bash -c 'echo "$JIN_ACTION_ID" >> idle.txt'
    on:
      - status_changed:idle
  - id: on-thinking
    entrypoint: bash -c 'echo "$JIN_ACTION_ID" >> thinking.txt'
    on:
      - status_changed:thinking
`)

// Two actions with disjoint matchers must fire independently: an idle event
// runs only on-idle, a thinking event only on-thinking.
func TestPublishRoutesEventsPerAction(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "multi", twoActionManifest)

	d.Publish(idleEvent())

	idleOut := filepath.Join(pluginsDir, "multi", "idle.txt")
	thinkingOut := filepath.Join(pluginsDir, "multi", "thinking.txt")
	if !waitForFile(t, idleOut) {
		t.Fatal("on-idle action did not run for idle event")
	}
	data, err := os.ReadFile(idleOut)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "on-idle" {
		t.Errorf("JIN_ACTION_ID = %q, want on-idle", got)
	}
	if _, err := os.Stat(thinkingOut); err == nil {
		t.Fatal("on-thinking action ran for idle event")
	}

	d.Publish(Event{Name: manifest.EventStatusChanged, SessionID: "sess-1", Status: "thinking", PrevStatus: "idle"})

	if !waitForFile(t, thinkingOut) {
		t.Fatal("on-thinking action did not run for thinking event")
	}
	if got := waitForLines(t, idleOut, 1); got != 1 {
		t.Errorf("on-idle ran %d times, want 1 (thinking event must not re-fire it)", got)
	}
}

// bothActionsManifest is a v2 fixture whose two actions both match
// status_changed:idle, writing to distinct files.
var bothActionsManifest = v2Manifest("both", "two actions on the same matcher", `  - id: first
    entrypoint: bash -c 'echo ran >> first.txt'
    on:
      - status_changed:idle
  - id: second
    entrypoint: bash -c 'echo ran >> second.txt'
    on:
      - status_changed:idle
`)

func TestPublishFiresAllMatchingActions(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "both", bothActionsManifest)

	d.Publish(idleEvent())

	firstOut := filepath.Join(pluginsDir, "both", "first.txt")
	secondOut := filepath.Join(pluginsDir, "both", "second.txt")
	if !waitForFile(t, firstOut) {
		t.Fatal("first action did not run")
	}
	if !waitForFile(t, secondOut) {
		t.Fatal("second action did not run (debounce must be per-action)")
	}

	// A second publish inside the window must debounce each action
	// independently: both stay at exactly one run.
	d.Publish(idleEvent())

	if got := waitForLines(t, firstOut, 1); got != 1 {
		t.Errorf("first action ran %d times within debounce window, want 1", got)
	}
	if got := waitForLines(t, secondOut, 1); got != 1 {
		t.Errorf("second action ran %d times within debounce window, want 1", got)
	}
}

// Debouncing one action must not swallow a different action of the same
// plugin for the same (session, event) pair.
func TestPassDebounceIsPerAction(t *testing.T) {
	d, _, _ := newTestDispatcher(t, config.PluginsConfig{})

	if !d.passDebounce("multi", "first", idleEvent()) {
		t.Fatal("first action should pass debounce")
	}
	if d.passDebounce("multi", "first", idleEvent()) {
		t.Fatal("first action should be debounced on the second hit")
	}
	if !d.passDebounce("multi", "second", idleEvent()) {
		t.Fatal("second action should pass debounce independently of first")
	}
}

// actionDumpManifest exposes JIN_ACTION_ID for on-demand runs: both actions
// append their received id to out.txt.
var actionDumpManifest = v2Manifest("actiondump", "dumps action id", `  - id: primary
    entrypoint: bash -c 'echo "$JIN_ACTION_ID" >> out.txt'
  - id: secondary
    entrypoint: bash -c 'echo "$JIN_ACTION_ID" >> out.txt'
`)

// RunAction with an empty id must run the default action (actions[0]); an
// explicit id must run exactly that action. Both runs must see their own id
// in JIN_ACTION_ID.
func TestRunActionSelectsActionAndInjectsID(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "actiondump", actionDumpManifest)

	ev := Event{Name: "action"}
	if err := d.RunAction("actiondump", "", ev, 0, ActionContext{}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(pluginsDir, "actiondump", "out.txt")
	if got := waitForLines(t, out, 1); got != 1 {
		t.Fatalf("default run: %d lines, want 1", got)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "primary" {
		t.Errorf("default run JIN_ACTION_ID = %q, want primary (actions[0])", got)
	}

	if err := d.RunAction("actiondump", "secondary", ev, 0, ActionContext{}); err != nil {
		t.Fatal(err)
	}
	if got := waitForLines(t, out, 2); got != 2 {
		t.Fatalf("explicit run: %d lines, want 2", got)
	}
	data, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "secondary") {
		t.Errorf("explicit run output = %q, want to contain secondary", string(data))
	}
}

func TestRunActionRejectsUnknownAction(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "actiondump", actionDumpManifest)

	err := d.RunAction("actiondump", "nope", Event{Name: "action"}, 0, ActionContext{})
	if err == nil || !strings.Contains(err.Error(), `no action "nope"`) {
		t.Fatalf("unknown action error = %v, want 'no action \"nope\"'", err)
	}
	if !strings.Contains(err.Error(), "primary, secondary") {
		t.Errorf("error %q should list available actions", err.Error())
	}
}

// --- refusal logging ---

// capturePluginLog swaps the package logger for a recorder and returns a reader
// for what it collected. pluginLog is a no-op in a test binary (internal/debug
// refuses to write from one), so replacing it is the only way to see what a
// refusal would have recorded — and the refusals that matter most reach no one
// else: a plugin key binding fires `jin plugin run` through tmux's
// `run-shell -b` with its output discarded.
//
// It is set on one dispatcher rather than on the package, so a test cannot see
// another test's runs, and it goes in through setLog rather than by assigning
// the field: this dispatcher's own goroutines read that field, and an
// unsynchronised write was measured racing with them. The recorder locks for
// the same reason on the way out.
func capturePluginLog(t *testing.T, d *EventDispatcher) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	// Restored on the way out: subtests here share a dispatcher, and a run still
	// in flight from an earlier one would otherwise write into a later one's
	// recorder. No case does that today; the ordering is what makes it safe, so
	// it should not be left to hold by luck.
	prev := d.setLog(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	t.Cleanup(func() { d.setLog(prev) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

// refusalsFor returns the recorded lines reporting that a run for the given
// plugin did not start.
func refusalsFor(lines []string, plugin string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, "not started") && strings.Contains(l, plugin) {
			out = append(out, l)
		}
	}
	return out
}

func TestRunAction_RecordsEveryRefusal(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	tests := []struct {
		name       string
		plugin     string
		action     string
		depth      int
		wantInLine string
	}{
		{"depth limit", "dumper", "default", 1, "depth limit reached"},
		{"not installed", "absent", "", 0, "not installed"},
		{"unknown action", "dumper", "nope", 0, "no action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := capturePluginLog(t, d)
			if err := d.RunAction(tt.plugin, tt.action, idleEvent(), tt.depth, ActionContext{}); err == nil {
				t.Fatal("RunAction succeeded, want a refusal")
			}
			got := refusalsFor(recorded(), tt.plugin)
			if len(got) != 1 {
				t.Fatalf("recorded %d refusals for %s, want 1: %q", len(got), tt.plugin, got)
			}
			if !strings.Contains(got[0], tt.wantInLine) {
				t.Errorf("logged %q, want it to mention %q", got[0], tt.wantInLine)
			}
			// The action id is the part only the caller knows: a depth refusal
			// happens before any action is resolved, so the error beside it
			// cannot name one.
			if tt.action != "" && !strings.Contains(got[0], tt.action) {
				t.Errorf("logged %q, want it to name the requested action %q", got[0], tt.action)
			}
		})
	}
}

// TestRunAction_AcceptedRunLogsNothing is the other half: a line per accepted
// run would make the log useless for finding the refused ones.
func TestRunAction_AcceptedRunLogsNothing(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	recorded := capturePluginLog(t, d)
	if err := d.RunAction("dumper", "", idleEvent(), 0, ActionContext{}); err != nil {
		t.Fatalf("RunAction: %v", err)
	}
	// The run is async, and it writes inside the plugin dir this test's TempDir
	// owns. Waiting for it is not only about seeing the result: returning first
	// leaves a process writing into a directory t.Cleanup is removing.
	if !waitForFile(t, filepath.Join(pluginsDir, "dumper", "out.txt")) {
		t.Fatal("plugin did not run")
	}
	if got := refusalsFor(recorded(), "dumper"); len(got) != 0 {
		t.Errorf("recorded %q, want no refusal for an accepted run", got)
	}
}

// TestRunAction_RefusalIsBoundAndQuoted covers the sanitising the convention
// requires of any value the local process did not choose: `jin plugin run` puts
// its arguments here unchecked, and a newline in one would otherwise forge
// whole entries in a log read as jind-ai's own.
func TestRunAction_RefusalIsBoundAndQuoted(t *testing.T) {
	d, _, _ := newTestDispatcher(t, config.PluginsConfig{})

	// Both logged values are formatted with %s, so neither is escaped by fmt and
	// both depend on untrusted. The plugin name reaches the line through an
	// error built the same way ("plugin %s is not installed"); the action id
	// goes in directly. A payload in either has to come out inert, so each gets
	// a turn.
	forged := "x\n[2026-01-01 00:00:00.000] plugin run not started (requested action \"forged\")"
	payload := forged + strings.Repeat("A", 4096)
	for _, tt := range []struct{ name, plugin, action string }{
		{"in the plugin name", payload, "act"},
		{"in the action id", "absent", payload},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorded := capturePluginLog(t, d)
			if err := d.RunAction(tt.plugin, tt.action, idleEvent(), 0, ActionContext{}); err == nil {
				t.Fatal("RunAction succeeded, want a refusal")
			}
			assertInertLogLine(t, recorded())
		})
	}
}

// assertInert checks one recorded line cannot forge entries or fill the file —
// the two properties debug.Untrusted is there for.
//
// maxLen is the caller's own ceiling rather than a multiple of logFieldMax:
// deriving it from the constant would make the assertion move with the value it
// is meant to guard, so a bound that grew sixteenfold would still pass. This log
// is appended to and never rotated, so a per-line size is the property worth
// pinning.
func assertInert(t *testing.T, line string, maxLen int) {
	t.Helper()
	if strings.Contains(line, "\n") {
		t.Errorf("logged line contains a raw newline, so one payload can forge entries: %q", line)
	}
	if len(line) > maxLen {
		t.Errorf("logged line is %d bytes, want at most %d (logFieldMax is %d); one payload can fill the file",
			len(line), maxLen, logFieldMax)
	}
}

// assertInertLogLine checks the one recorded refusal line. Matching is on the
// message's own prefix rather than on the caller's values: those are exactly the
// ones being truncated.
func assertInertLogLine(t *testing.T, lines []string) {
	t.Helper()
	var got string
	for _, l := range lines {
		if strings.HasPrefix(l, "plugin run not started") {
			if got != "" {
				t.Fatalf("recorded more than one line: %q", lines)
			}
			got = l
		}
	}
	if got == "" {
		t.Fatalf("recorded no refusal line: %q", lines)
	}
	// The headroom is for what quoting does to the bound: strconv.Quote renders
	// one control byte as four characters, so two fields cut at 256 bytes reach
	// about 2KB in the worst case against roughly 560 for the ASCII payload in
	// the caller. The claim is boundedness, not a tight fit.
	assertInert(t, got, 4096)
}

// TestNewDispatcher_WiresTheProductionLogger pins the one line the tests above
// cannot: they all install a recorder, so a dispatcher built with a logger of
// its own would satisfy every one of them while the daemon recorded nothing.
//
// The check is by value, not by pointer identity. Comparing
// reflect.Value.Pointer would compare code addresses, and every logger
// debug.NewLogger returns shares one — so a dispatcher wired to a logger
// writing to the wrong file would have passed, while the CHANGELOG and
// docs/gotchas.md tell readers to look in plugin-debug.log.
//
// Swapping the package variable is safe here because NewDispatcher is its only
// reader and this package's tests do not run in parallel; the swap is undone
// before the test returns.
func TestNewDispatcher_WiresTheProductionLogger(t *testing.T) {
	reached := false
	prev := pluginLog
	pluginLog = func(string, ...any) { reached = true }
	t.Cleanup(func() { pluginLog = prev })

	d, _, _ := newTestDispatcher(t, config.PluginsConfig{})
	d.logf("probe")

	if !reached {
		t.Error("a dispatcher's diagnostics do not reach pluginLog; refusals would go somewhere other than plugin-debug.log")
	}
}

// TestWarnOnce_GoesThroughTheInjectedLogger and the debounce case below cover
// the other two conversions to the seam. Without them "the dispatcher's
// diagnostics are injectable" would be true of one call site out of three.
func TestWarnOnce_GoesThroughTheInjectedLogger(t *testing.T) {
	d, _, _ := newTestDispatcher(t, config.PluginsConfig{})
	recorded := capturePluginLog(t, d)

	d.warnOnce("k", "something broke: %s", "detail")
	d.warnOnce("k", "something broke: %s", "detail")

	got := recorded()
	if len(got) != 1 {
		t.Fatalf("recorded %d lines, want 1 (warnOnce logs once per key): %q", len(got), got)
	}
	if !strings.Contains(got[0], "something broke: detail") {
		t.Errorf("recorded %q, want the formatted message", got[0])
	}
}

func TestPublishDebounced_GoesThroughTheInjectedLogger(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{Debounce: 60})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)
	recorded := capturePluginLog(t, d)

	d.Publish(idleEvent())
	if !waitForFile(t, filepath.Join(pluginsDir, "dumper", "out.txt")) {
		t.Fatal("plugin did not run for the first event")
	}
	d.Publish(idleEvent())

	if _, ok := waitForRecordedLine(recorded, "debounced"); !ok {
		t.Errorf("no debounce line recorded; got %q", recorded())
	}
}

// TestSetLog_IsSafeWhileRunsAreInFlight builds the interleave the lock exists
// for: a recorder installed while this dispatcher's own goroutines are logging.
// Without it the field's protection is a claim rather than a guarantee — both
// earlier attempts at this seam (a package variable, then an unguarded field)
// raced, and each time the suite stayed green because every other test installs
// its recorder before starting anything.
//
// Only meaningful under -race, which the unit CI job runs.
func TestSetLog_IsSafeWhileRunsAreInFlight(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			// Refused runs log without spawning anything, so the goroutines
			// reading d.log are this dispatcher's own and the test stays fast.
			_ = d.RunAction("absent", "", idleEvent(), 0, ActionContext{})
		}
	}()
	for range 50 {
		d.setLog(func(string, ...any) {})
	}
	<-done
}

// TestPublish_BrokenManifestIsReportedInert covers the other value this package
// logs that a caller chose. A plugin's manifest arrives from a registry or a
// git checkout, and a parse failure quotes what it could not read — so the
// error carries manifest text into a log read as jind-ai's own. Measured with a
// raw newline in it before it was bounded.
func TestPublish_BrokenManifestIsReportedInert(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	// No double quote in the payload: it sits inside a double-quoted YAML
	// scalar, and one would end the scalar early — the parser would then fail
	// somewhere else, with an error that happens not to carry a newline, and
	// the test would pass against an unbounded log line. Measured.
	forged := "x\n[2026-01-01 00:00:00.000] plugin run not started"
	broken := "schema_version: \"" + forged + strings.Repeat("A", 4096) + "\"\n" +
		"name: evil\nversion: 0.1.0\ndescription: d\njin: \">=0.0.0\"\n" +
		"install:\n  source: {}\nactions:\n  - id: default\n    entrypoint: true\n    on: []\n"
	installTestPlugin(t, pluginsDir, stateDir, "evil", broken)

	recorded := capturePluginLog(t, d)
	d.Publish(idleEvent())

	line := assertInertDiagnostic(t, recorded, "evil")
	// Inert is half of it. The line exists to say why the plugin was skipped,
	// and an empty diagnostic would be inert too.
	if !strings.Contains(line, "manifest") {
		t.Errorf("logged line does not say what went wrong: %q", line)
	}
}

// TestPublish_UnreadableLockIsReportedInert and the failing-run case below hold
// the two remaining lines this package writes to the same rule as the others.
//
// Neither discriminates today, and that is worth saying rather than leaving for
// the next reader to measure: removing the sanitising from either line still
// passes. No caller-chosen text reaches them in the current code — ExecPlugin
// reduces a failed run to `exit status N`, and a lock file that fails to parse
// produced no newline in the errors tried here. They are regression anchors for
// the rule, not evidence that it is load-bearing on these two lines; the lines
// where it is are covered by TestRunAction_RefusalIsBoundAndQuoted and
// TestPublish_BrokenManifestIsReportedInert, whose mutants do die.
func TestPublish_UnreadableLockIsReportedInert(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	installTestPlugin(t, pluginsDir, stateDir, "dumper", dumpEntrypointRuntime)

	forged := "x\n[2026-01-01 00:00:00.000] plugin run not started"
	corrupt := "plugins:\n  dumper: " + forged + strings.Repeat("A", 4096) + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, "plugins.lock.yaml"), []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}

	recorded := capturePluginLog(t, d)
	d.Publish(idleEvent())
	assertInertDiagnostic(t, recorded, "registry")
}

func TestRun_FailingPluginIsReportedInert(t *testing.T) {
	d, pluginsDir, stateDir := newTestDispatcher(t, config.PluginsConfig{})
	// The entrypoint writes a forged line to stderr and exits non-zero, so the
	// error the dispatcher reports carries text the plugin chose.
	forged := `x\n[2026-01-01 00:00:00.000] plugin run not started`
	failing := `schema_version: 1
name: failer
version: 0.1.0
description: fails
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: bash -c 'printf "` + forged + `" >&2; exit 3'
on:
  - status_changed:idle
`
	installTestPlugin(t, pluginsDir, stateDir, "failer", failing)

	recorded := capturePluginLog(t, d)
	d.Publish(idleEvent())
	assertInertDiagnostic(t, recorded, "failer")
}

// assertInertDiagnostic waits for a recorded line naming want, checks it cannot
// forge entries or fill the file, and returns it so a caller can go on to assert
// what the line says.
func assertInertDiagnostic(t *testing.T, recorded func() []string, want string) string {
	t.Helper()
	line, ok := waitForRecordedLine(recorded, want)
	if !ok {
		t.Fatalf("no line recorded naming %q", want)
	}
	assertInert(t, line, 1024)
	return line
}

// waitForRecordedLine returns the first recorded line containing want. It waits
// because the dispatch that produces one runs on its own goroutine, so the line
// lands after the Publish that caused it returns. Three seconds, the budget this
// package's other waits allow a plugin process.
func waitForRecordedLine(recorded func() []string, want string) (string, bool) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range recorded() {
			if strings.Contains(l, want) {
				return l, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", false
}
