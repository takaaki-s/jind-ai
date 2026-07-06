package session

// Package session owns the interface + supporting types that describe how
// jindaiko talks to an interactive agent (Claude Code, Codex CLI, ...). The
// concrete implementations live under internal/agent/<kind>/; the session
// domain only knows this narrow surface so it can spawn / observe an agent
// without importing the adapter packages (import direction stays session ←
// agent, never the reverse).

// SpawnOptions is the input an Agent adapter receives when jindaiko needs to
// build the shell command that starts (or resumes) the agent inside a tmux
// pane.
type SpawnOptions struct {
	// JinSessionID is jindaiko's own session UUID. Adapters typically expose
	// it to the agent via the JIN_SESSION_ID env var so hook callbacks can
	// correlate back to a jindaiko session.
	JinSessionID string
	// AgentSessionID is the adapter-side persistent identifier (Claude
	// Code's --session-id / --resume UUID, for example). Empty on the very
	// first spawn of a fresh session.
	AgentSessionID string
	// AgentSessionStarted is true when the agent has been launched at
	// least once with AgentSessionID; adapters use it to decide between a
	// "new session" and a "resume" command line.
	AgentSessionStarted bool
	// WorkDir is the absolute directory the agent should start in (~ is
	// already expanded).
	WorkDir string
	// CustomEnv carries user-configured env vars from config.yaml. The
	// Manager forwards them to the shell command; adapters may also read
	// them if they need to.
	CustomEnv map[string]string
}

// SpawnPlan is what an Agent adapter returns to describe how to launch the
// agent. Manager splices the pieces into the fixed shell template it uses to
// wrap every session (`cd DIR; env -u ... KEY=VAL SHELL -ic 'COMMAND'`).
//
// Shell safety contract — how Manager treats each field:
//
//   - Command is placed inside single quotes; Manager defensively escapes
//     any single quote it finds. Adapters SHOULD NOT pre-escape their own
//     quotes: the replacement is designed for the raw form, and
//     double-escaping would break the wrapping. Emit the literal command
//     as if you were typing it into an interactive shell.
//   - ExtraEnv values are single-quoted by Manager (KEY='value'), so
//     arbitrary content survives — including whitespace and shell
//     metacharacters. Adapters can pass values verbatim.
//   - ExtraEnv keys and UnsetEnv entries must be POSIX env-var names
//     matching [A-Za-z_][A-Za-z0-9_]*. Manager rejects any that don't
//     (returns an error before the process is spawned).
//
// The contract is intentionally "Manager is the last line of defence":
// adapters return honest, unescaped values and Manager makes them safe.
type SpawnPlan struct {
	// Command is the single-line shell command that starts the agent
	// (e.g. `claude --settings /path/to/hooks.json --session-id UUID`).
	Command string
	// ExtraEnv is agent-specific KEY=VALUE pairs that must be exported for
	// the process. Manager adds them alongside the fixed JIN_SESSION_ID
	// group. Nil / empty is fine.
	ExtraEnv map[string]string
	// UnsetEnv lists env-var names that must be cleared before exec (env
	// -u NAME). Manager already unsets TMUX / TMUX_PANE unconditionally;
	// adapters add their own (Claude Code needs CLAUDECODE unset so
	// nested invocations work).
	UnsetEnv []string
}

// StatusSignal is the raw event an agent adapter interprets to decide the
// session's next Status. Manager builds it from whatever channel it caught
// the signal on (hook callback, pane output tail, ...) and hands it to
// StatusSource.Interpret.
type StatusSignal struct {
	// Kind identifies the transport (currently only "hook"). Adapters
	// switch on this and ignore signals they don't understand.
	Kind string
	// Payload is an untyped key/value bag; the exact keys depend on Kind
	// and are adapter-defined. For "hook" the Manager fills in "event",
	// "notification_type", "stop_reason", "cwd".
	Payload map[string]string
}

// StatusUpdate is the adapter's verdict on a signal: which Status the
// session should move to and whether a desktop notification should fire.
//
// ErrorMessage / ClearError work as a tri-state so adapters can distinguish
// three intents on the shared ErrorMessage field:
//
//   - ErrorMessage != ""            → set the field (adapter has a message)
//   - ClearError == true            → clear the field (agent recovered)
//   - both zero                     → leave whatever was there in place
//
// The Claude Code adapter uses the first form for StopFailure, the second
// for Stop / UserPromptSubmit / PreToolUse / PostToolUse (the pre-refactor
// invariant "any post-error progression clears the message"), and the third
// for SessionEnd / Notification (which historically never touched the
// field). Adapters that don't care about error semantics can leave both
// zero — the field remains untouched.
type StatusUpdate struct {
	Status       Status
	ErrorMessage string
	ClearError   bool
	Notify       NotifyKind
}

// NotifyKind is the abstract notification category an adapter can request
// from a StatusUpdate. Manager maps each kind to a concrete notifier call.
type NotifyKind string

const (
	// NotifyNone is the zero value; Manager fires no notification.
	NotifyNone NotifyKind = ""
	// NotifyTaskComplete corresponds to notifier.NotifyTaskComplete
	// (assistant finished a turn).
	NotifyTaskComplete NotifyKind = "task-complete"
	// NotifyError corresponds to notifier.NotifyError (assistant reported
	// a failure). ErrorMessage on StatusUpdate is passed through.
	NotifyError NotifyKind = "error"
	// NotifyPermission corresponds to notifier.NotifyPermission (the
	// agent is blocked waiting for user approval).
	NotifyPermission NotifyKind = "permission"
)

// StatusSource translates raw StatusSignals into StatusUpdates. Adapters
// return (StatusUpdate{}, false) when a signal is meaningful but does not
// warrant a Status change (Manager still applies side effects such as CWD
// tracking).
type StatusSource interface {
	Interpret(StatusSignal) (StatusUpdate, bool)
}

// SetupContext is the input to Agent.Setup, called once per session start
// before the shell command is built. Adapters use it to write agent-side
// config files (Claude Code's hooks-settings.json, trust dialog state, ...).
type SetupContext struct {
	StateDir string // jindaiko's persistent state directory (~/.local/state/jindaiko)
	ExecPath string // absolute path to the running jin binary (os.Executable())
	WorkDir  string // absolute working directory the session will start in
}

// Agent is the interface every agent adapter satisfies. The Manager holds it
// via AgentResolver and never imports a concrete adapter package.
//
// Implementations must be safe for concurrent use: Setup and SpawnCommand
// may be invoked from multiple goroutines (per-session goroutines that
// captureOutputTmux spawns).
type Agent interface {
	// Kind returns the short identifier stored in Session.AgentKind
	// ("claude", "codex", ...).
	Kind() string
	// Setup prepares any agent-global or per-workDir state that must exist
	// before the process is spawned. Called once per startSessionTmux
	// invocation. Errors are logged but do not abort the launch — see the
	// Claude Code adapter for the intended failure semantics.
	Setup(SetupContext) error
	// SpawnCommand returns the shell command + env additions that launch
	// (or resume) the agent for the given session.
	SpawnCommand(SpawnOptions) SpawnPlan
	// StatusSource returns the adapter's interpreter for StatusSignals.
	// Must never return nil (agents that don't observe status can return a
	// no-op implementation).
	StatusSource() StatusSource
	// Description returns the adapter's Layer C description enhancer, or
	// nil if the adapter cannot produce structured descriptions.
	Description() DescriptionEnhancer
}

// AgentResolver bridges the Manager to the process-global agent registry
// that lives in internal/agent. The daemon injects a thin implementation
// that delegates to agent.Lookup; the session package never sees the
// registry itself, keeping the import direction one-way.
type AgentResolver interface {
	Resolve(kind string) (Agent, error)
}
