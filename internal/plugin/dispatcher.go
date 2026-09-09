package plugin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/takaaki-s/jind-ai/internal/debug"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

var pluginLog = debug.NewLogger("plugin-debug.log")

// logFieldMax bounds one logged field. The values this package logs come from
// `jin plugin run` arguments, which the daemon accepts without inspecting.
const logFieldMax = 256

// untrusted is debug.Untrusted at this package's bound, named once so that a
// field added later gets the bound and the quoting together.
//
// The rule is uniform rather than per value: everything this package
// interpolates into a log line goes through it, except the constants and
// integers it produced itself. Deciding per value meant deciding what each one
// had already been checked against, and that reasoning was wrong twice — a
// manifest error quoting the file it could not parse, and a plugin name whose
// only validation lives in a caller two packages away.
func untrusted(s string) string { return debug.Untrusted(s, logFieldMax) }

// untrustedErr is the same rule for an error, which is most of what this
// package logs. fmt.Sprint rather than Error() so that a nil one renders
// instead of panicking: an entry's Err is a field, not a return value.
func untrustedErr(err error) string { return untrusted(fmt.Sprint(err)) }

// maxDepth bounds direct plugin→plugin chains: a plugin runs at depth 1, so a
// run it requests would land at depth 2 and is rejected. Depth cannot follow
// the indirect loop (plugin → `jin session send` → agent → hook) because the
// environment does not survive the agent process; the debounce window is the
// primary guard for that path.
//
// It does not follow a pane either. A pane opened by `jin pane popup` / `jin
// pane split` is given the four identity variables and not this one, so a
// plugin that runs another plugin from inside a popup starts it back at depth
// 1 — and nothing else stops it, because that run arrives through RunAction,
// which bypasses debounce by design. Measured, 3 trials of 3.
//
// Left this way on purpose. Propagating the depth into panes would bound the
// chain, and would also refuse the run a user makes by pressing a button in a
// popup a plugin opened as its own UI, which is a documented use for popups.
// The README says a chain started from a popup is unbounded and the author must
// stop it; that is the contract, not an oversight.
//
// The inverse — a depth arriving where no plugin put one — is guarded, since
// there the accident refuses runs rather than allowing them. A tmux server
// forked by a process that carried a depth hands it to every pane, which is why
// jinenv.TmuxEnviron writes the key empty on every pane jind-ai opens.
const maxDepth = 2

// DefaultDebounce is the minimum interval between deliveries of the same
// (plugin, action, session, event) tuple when the caller does not configure one.
const DefaultDebounce = 3 * time.Second

// MaxHandoffTimeout keeps the synchronous daemon request inside the client's
// 60s request deadline. A provider may declare a tighter manifest timeout.
const MaxHandoffTimeout = 30 * time.Second

// debouncePruneThreshold caps lastFired growth: sessions come and go for the
// daemon's whole lifetime, so once the map crosses this size expired entries
// are swept on the next debounce check. Entries past their window carry no
// information, making the sweep free of behaviour change.
const debouncePruneThreshold = 128

// PopupSizeResolver resolves the popup size a plugin action should receive as
// JIN_PLUGIN_POPUP_* env when it runs. Empty strings mean "no explicit size"
// and `jin pane popup --here` falls through to tmux's built-in default.
// Precedence: user config > manifest declaration > global plugin default >
// hardcoded. actionID identifies which action is running, for per-action config.
type PopupSizeResolver func(pluginName, actionID string, m *manifest.PopupConfig) (width, height string)

// EventDispatcher fans events out to installed plugins. Publish never blocks:
// registry reads and plugin processes run on background goroutines, and every
// failure is logged rather than returned (fail-open — a broken plugin must not
// stall the status pipeline).
type EventDispatcher struct {
	registry      *Registry
	pluginsDir    string
	stateDir      string
	identity      jinenv.Identity
	debounce      time.Duration
	popupResolver PopupSizeResolver

	mu        sync.Mutex
	lastFired map[string]time.Time
	warned    map[string]bool
	// log is where this dispatcher's diagnostics go, under mu with the rest of
	// the mutable state rather than beside the immutable configuration above. A
	// field at all so a test can read what a run recorded; under the lock because
	// the readers are this dispatcher's own goroutines — a run Publish started can
	// still be logging when a test installs its recorder, and an unsynchronised
	// field was measured racing there.
	log func(string, ...any)
}

// logf writes one diagnostic line, reading the logger under the lock and
// calling it outside. Holding the lock across the call would also be correct —
// nothing this package does re-enters — but it would put a file write inside
// the mutex the debounce map uses, and the lock is not there to serialise
// logging.
func (d *EventDispatcher) logf(format string, args ...any) {
	d.mu.Lock()
	log := d.log
	d.mu.Unlock()
	log(format, args...)
}

// setLog installs a logger and returns the one it replaced. Only tests call it;
// production sets the field once in NewDispatcher, before the dispatcher is
// reachable from any goroutine. It returns the previous value so a caller
// restoring it afterwards never has to read the field itself — a read outside
// the lock is the thing this pair exists to prevent.
func (d *EventDispatcher) setLog(fn func(string, ...any)) func(string, ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.log
	d.log = fn
	return prev
}

// NewDispatcher returns a dispatcher that resolves plugins through registry
// and injects identity into every run. debounce <= 0 selects DefaultDebounce.
// A nil popupResolver is replaced with one that always returns empty strings.
//
// identity is supplied by the caller so that a plugin and an agent started by
// the same daemon get the same one.
func NewDispatcher(registry *Registry, pluginsDir, stateDir string, identity jinenv.Identity, debounce time.Duration, popupResolver PopupSizeResolver) *EventDispatcher {
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	if popupResolver == nil {
		popupResolver = func(string, string, *manifest.PopupConfig) (string, string) { return "", "" }
	}
	return &EventDispatcher{
		registry:      registry,
		pluginsDir:    pluginsDir,
		stateDir:      stateDir,
		identity:      identity,
		debounce:      debounce,
		popupResolver: popupResolver,
		log:           pluginLog,
		lastFired:     make(map[string]time.Time),
		warned:        make(map[string]bool),
	}
}

// Publish implements Dispatcher.
func (d *EventDispatcher) Publish(ev Event) {
	go d.publish(ev)
}

func (d *EventDispatcher) publish(ev Event) {
	entries, err := d.registry.Load()
	if err != nil {
		d.warnOnce("registry", "plugin registry load failed: %s", untrustedErr(err))
		return
	}
	for _, e := range entries {
		switch e.State {
		case StateEnabled:
			// handled below
		case StateIncompatible, StateBroken:
			// The name comes from a key in the lock file and the error quotes
			// the manifest it could not parse; neither was chosen here. A
			// manifest that fails to unmarshal was measured putting a raw
			// newline into a line read as jind-ai's own.
			d.warnOnce(e.Name+"|"+e.State.String(), "plugin %s skipped (%s): %s",
				untrusted(e.Name), e.State, untrustedErr(e.Err))
			continue
		default:
			continue
		}
		for i := range e.Manifest.Actions {
			a := &e.Manifest.Actions[i]
			if !d.matches(a, ev) {
				continue
			}
			if !d.passDebounce(e.Name, a.ID, ev) {
				d.logf("plugin %s:%s debounced for %s %s:%s",
					untrusted(e.Name), untrusted(a.ID), untrusted(ev.SessionID), ev.Name, ev.Status)
				continue
			}
			go d.run(e, a, ev, 1, ActionContext{})
		}
	}
}

// RunAction executes one plugin action on demand (the `jin plugin run` path).
// It bypasses matcher and debounce but still enforces state and depth checks.
// actionID selects which of the plugin's actions runs; "" means the default
// action (actions[0]) and an unknown id is a synchronous error. The run itself
// is async. actx carries the invoking CLI's tmux context.
//
// Every run that does not start is logged as well as returned, because the
// caller that most needs to know is the one that cannot report: a plugin key
// binding fires `jin plugin run` through tmux's `run-shell -b` with stdout and
// stderr discarded, so the returned error reaches no one. Measured with a depth
// inherited from a tmux server, where every binding was refused and the screen
// showed nothing.
//
// Under JIN_DEBUG=1, that is. pluginLog is a no-op otherwise, so on a default
// install the binding is still silent; making it diagnosable without the flag
// would need a channel this package does not have.
//
// "not started" rather than "refused" because a registry read failure comes out
// of here too, and that is this daemon's fault rather than the caller's.
func (d *EventDispatcher) RunAction(name, actionID string, ev Event, callerDepth int, actx ActionContext) (err error) {
	defer func() {
		if err != nil {
			// The error already names the plugin; the requested action is the
			// part only the caller knows, and a depth refusal happens before
			// any action is resolved. Both go through debug.Untrusted: they
			// come from a `jin plugin run` argument, and a name carrying a
			// newline would otherwise forge entries in a log read as jind-ai's
			// own.
			d.logf("plugin run not started (requested action %s): %s",
				untrusted(actionID), untrustedErr(err))
		}
	}()
	if callerDepth+1 >= maxDepth {
		return fmt.Errorf("plugin %s not run: depth limit reached (%s=%d) — plugins cannot chain plugin runs", name, jinenv.EnvDepth, callerDepth)
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
			var a *manifest.Action
			if actionID == "" {
				a = e.Manifest.DefaultAction()
				if a == nil {
					return fmt.Errorf("plugin %s has no actions", name)
				}
			} else {
				a = e.Manifest.FindAction(actionID)
				if a == nil {
					return fmt.Errorf("plugin %s has no action %q (available: [%s])",
						name, actionID, strings.Join(e.Manifest.ActionIDs(), ", "))
				}
			}
			if a.Handoff {
				return fmt.Errorf("plugin %s action %s is a PR handoff provider; use jin session pr-handoff", name, a.ID)
			}
			go d.run(e, a, ev, callerDepth+1, actx)
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

// ResolveHandoff validates that an installed, enabled plugin exposes the
// requested structured handoff endpoint and returns its canonical action ID.
// Empty actionID means the manifest's default action, matching plugin run.
func (d *EventDispatcher) ResolveHandoff(name, actionID string) (string, error) {
	_, action, err := d.resolveHandoff(name, actionID)
	if err != nil {
		return "", err
	}
	return action.ID, nil
}

// RunHandoff executes a handoff endpoint synchronously and returns its bounded
// stdout. The caller owns interpretation and persistence of the JSON result.
func (d *EventDispatcher) RunHandoff(name, actionID, key string, ev Event, payload []byte) ([]byte, error) {
	entry, action, err := d.resolveHandoff(name, actionID)
	if err != nil {
		return nil, err
	}
	timeout := entry.Manifest.EffectiveTimeout()
	if timeout > MaxHandoffTimeout {
		timeout = MaxHandoffTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return ExecHandoff(ctx, ExecOptions{
		PluginDir:  filepath.Join(d.pluginsDir, entry.Name),
		Run:        action.Entrypoint,
		ActionID:   action.ID,
		Env:        ev,
		Depth:      1,
		Identity:   d.identity,
		LogPath:    LogPath(d.stateDir, entry.Name),
		Timeout:    timeout,
		HandoffKey: key,
	}, payload)
}

func (d *EventDispatcher) resolveHandoff(name, actionID string) (Entry, *manifest.Action, error) {
	entries, err := d.registry.Load()
	if err != nil {
		return Entry{}, nil, err
	}
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}
		switch entry.State {
		case StateIncompatible:
			return Entry{}, nil, fmt.Errorf("plugin %s is incompatible: %v (try: jin plugin update %s)", name, entry.Err, name)
		case StateBroken:
			return Entry{}, nil, fmt.Errorf("plugin %s is broken: %v", name, entry.Err)
		case StateDisabled:
			return Entry{}, nil, fmt.Errorf("plugin %s is disabled", name)
		}
		action := entry.Manifest.DefaultAction()
		if actionID != "" {
			action = entry.Manifest.FindAction(actionID)
		}
		if action == nil {
			return Entry{}, nil, fmt.Errorf("plugin %s has no action %q", name, actionID)
		}
		if !action.Handoff {
			return Entry{}, nil, fmt.Errorf("plugin %s action %s is not a handoff provider", name, action.ID)
		}
		return entry, action, nil
	}
	return Entry{}, nil, fmt.Errorf("plugin %s is not installed", name)
}

func (d *EventDispatcher) run(e Entry, a *manifest.Action, ev Event, depth int, actx ActionContext) {
	timeout := e.Manifest.EffectiveTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	popupWidth, popupHeight := d.popupResolver(e.Name, a.ID, a.Popup)

	err := ExecPlugin(ctx, ExecOptions{
		PluginDir:   filepath.Join(d.pluginsDir, e.Name),
		Run:         a.Entrypoint,
		ActionID:    a.ID,
		Env:         ev,
		Caller:      actx,
		Depth:       depth,
		Identity:    d.identity,
		LogPath:     LogPath(d.stateDir, e.Name),
		Timeout:     timeout,
		PopupWidth:  popupWidth,
		PopupHeight: popupHeight,
	})
	if err != nil {
		d.warnOnce(e.Name+"|"+a.ID+"|"+err.Error(), "plugin %s:%s failed: %s",
			untrusted(e.Name), untrusted(a.ID), untrustedErr(err))
	}
}

func (d *EventDispatcher) matches(a *manifest.Action, ev Event) bool {
	for _, matcher := range a.On {
		if manifest.MatcherMatches(matcher, ev.Name, ev.Status) {
			return true
		}
	}
	return false
}

// passDebounce reports whether the (plugin, action, session, event) tuple is
// outside its debounce window, and records the firing time when it is.
func (d *EventDispatcher) passDebounce(name, actionID string, ev Event) bool {
	key := name + "\x00" + actionID + "\x00" + ev.SessionID + "\x00" + ev.Name + ":" + ev.Status
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
		d.logf(format, args...)
	}
}
