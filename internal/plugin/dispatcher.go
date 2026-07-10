package plugin

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/takaaki-s/jind-ai/internal/debug"
)

var pluginLog = debug.NewLogger("plugin-debug.log")

// maxDepth bounds direct plugin→plugin chains: a plugin runs at depth 1, so a
// run it requests would land at depth 2 and is rejected. Depth cannot follow
// the indirect loop (plugin → `jin session send` → agent → hook) because the
// environment does not survive the agent process; the debounce window is the
// primary guard for that path.
const maxDepth = 2

// DefaultDebounce is the minimum interval between deliveries of the same
// (plugin, session, event) triple when the caller does not configure one.
const DefaultDebounce = 3 * time.Second

// debouncePruneThreshold caps lastFired growth: sessions come and go for the
// daemon's whole lifetime, so once the map crosses this size expired entries
// are swept on the next debounce check. Entries past their window carry no
// information, making the sweep free of behaviour change.
const debouncePruneThreshold = 128

// EventDispatcher fans events out to installed plugins. Publish never blocks:
// registry reads and plugin processes run on background goroutines, and every
// failure is logged rather than returned (fail-open — a broken plugin must not
// stall the status pipeline).
type EventDispatcher struct {
	registry   *Registry
	pluginsDir string
	stateDir   string
	socketPath string
	debounce   time.Duration

	mu        sync.Mutex
	lastFired map[string]time.Time
	warned    map[string]bool
}

// NewDispatcher returns a dispatcher that resolves plugins through registry
// and injects socketPath as JIN_SOCKET into every run. debounce <= 0 selects
// DefaultDebounce.
func NewDispatcher(registry *Registry, pluginsDir, stateDir, socketPath string, debounce time.Duration) *EventDispatcher {
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	return &EventDispatcher{
		registry:   registry,
		pluginsDir: pluginsDir,
		stateDir:   stateDir,
		socketPath: socketPath,
		debounce:   debounce,
		lastFired:  make(map[string]time.Time),
		warned:     make(map[string]bool),
	}
}

// Publish implements Dispatcher.
func (d *EventDispatcher) Publish(ev Event) {
	go d.publish(ev)
}

func (d *EventDispatcher) publish(ev Event) {
	entries, err := d.registry.Load()
	if err != nil {
		d.warnOnce("registry", "plugin registry load failed: %v", err)
		return
	}
	for _, e := range entries {
		switch e.State {
		case StateEnabled:
			// handled below
		case StateIncompatible, StateBroken:
			d.warnOnce(e.Name+"|"+e.State.String(), "plugin %s skipped (%s): %v", e.Name, e.State, e.Err)
			continue
		default:
			continue
		}
		if !d.matches(e.Manifest, ev) {
			continue
		}
		if !d.passDebounce(e.Name, ev) {
			pluginLog("plugin %s debounced for %s %s:%s", e.Name, ev.SessionID, ev.Name, ev.Status)
			continue
		}
		go d.run(e, ev, 1, ActionContext{})
	}
}

// RunAction executes one plugin on demand (the `jin plugin run` path). It
// bypasses matcher and debounce but still enforces state and depth checks.
// Validation errors are returned synchronously; the run itself is async.
// actx carries the invoking CLI's tmux context (empty when not applicable).
func (d *EventDispatcher) RunAction(name string, ev Event, callerDepth int, actx ActionContext) error {
	if callerDepth+1 >= maxDepth {
		return fmt.Errorf("plugin %s not run: depth limit reached (JIN_PLUGIN_DEPTH=%d) — plugins cannot chain plugin runs", name, callerDepth)
	}
	entries, err := d.registry.Load()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		switch e.State {
		case StateEnabled:
			go d.run(e, ev, callerDepth+1, actx)
			return nil
		case StateIncompatible:
			return fmt.Errorf("plugin %s is incompatible: %v (try: jin plugin update %s)", name, e.Err, name)
		case StateBroken:
			return fmt.Errorf("plugin %s is broken: %v", name, e.Err)
		default:
			return fmt.Errorf("plugin %s is disabled", name)
		}
	}
	return fmt.Errorf("plugin %s is not installed", name)
}

func (d *EventDispatcher) run(e Entry, ev Event, depth int, actx ActionContext) {
	timeout := e.Manifest.EffectiveTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := ExecPlugin(ctx, ExecOptions{
		PluginDir:  filepath.Join(d.pluginsDir, e.Name),
		Run:        e.Manifest.Run,
		Env:        ev,
		Caller:     actx,
		APIVersion: e.Manifest.APIVersion,
		Depth:      depth,
		SocketPath: d.socketPath,
		LogPath:    LogPath(d.stateDir, e.Name),
		Timeout:    timeout,
	})
	if err != nil {
		d.warnOnce(e.Name+"|"+err.Error(), "plugin %s failed: %v", e.Name, err)
	}
}

func (d *EventDispatcher) matches(m *Manifest, ev Event) bool {
	for _, matcher := range m.On {
		if MatcherMatches(matcher, ev.Name, ev.Status) {
			return true
		}
	}
	return false
}

// passDebounce reports whether the (plugin, session, event) triple is outside
// its debounce window, and records the firing time when it is.
func (d *EventDispatcher) passDebounce(name string, ev Event) bool {
	key := name + "\x00" + ev.SessionID + "\x00" + ev.Name + ":" + ev.Status
	now := time.Now()

	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.lastFired[key]; ok && now.Sub(last) < d.debounce {
		return false
	}
	if len(d.lastFired) >= debouncePruneThreshold {
		for k, ts := range d.lastFired {
			if now.Sub(ts) >= d.debounce {
				delete(d.lastFired, k)
			}
		}
	}
	d.lastFired[key] = now
	return true
}

// warnOnce logs a warning once per key for the daemon's lifetime, so a
// persistently broken plugin does not flood the log on every event.
func (d *EventDispatcher) warnOnce(key, format string, args ...any) {
	d.mu.Lock()
	seen := d.warned[key]
	d.warned[key] = true
	d.mu.Unlock()
	if !seen {
		pluginLog(format, args...)
	}
}
