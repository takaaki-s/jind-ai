package session

import "github.com/takaaki-s/jind-ai/internal/transcript"

// This file is the whole surface between the session domain and an agent
// adapter. Implementations live under internal/agent/<kind>/; the import
// direction stays session ← agent, never the reverse.

// SpawnOptions is the input an Agent adapter receives when jind-ai needs to
// build the shell command that starts (or resumes) the agent inside a tmux
// pane.
type SpawnOptions struct {
	// StateDir is jind-ai's persistent state directory
	// (~/.local/state/jind-ai) — the same value Setup was handed for this
	// spawn, and the root of whatever Setup wrote there.
	//
	// Setup is best-effort and its failures are swallowed, so the artefacts it
	// wrote may be absent: derive what the spawn needs from this directory as
	// it stands, and omit the flag when there is nothing to point at.
	StateDir string
	// ExecPath is the jin binary this session's children re-enter to call
	// back — the same value Setup received, repeated here because
	// SpawnCommand names it directly (the Codex adapter's `-c hooks.…`
	// payloads) rather than only through a file Setup wrote.
	//
	// It is not os.Executable(), and empty is reachable in production: treat
	// it as "no callback path" rather than as impossible.
	ExecPath string
	// JinSessionID is jind-ai's own session UUID. Adapters typically expose
	// it to the agent via the JIN_SESSION_ID env var so hook callbacks can
	// correlate back to a jind-ai session.
	JinSessionID string
	// AgentSessionID is the adapter-side persistent identifier (Claude
	// Code's --session-id / --resume UUID, for example). Empty on the very
	// first spawn of a fresh session.
	AgentSessionID string
	// AgentSessionStarted is true when the agent has been launched at
	// least once with AgentSessionID; adapters use it to decide between a
	// "new session" and a "resume" command line.
	AgentSessionStarted bool
	// AgentSessionIDConfirmed is true when the agent itself reported
	// AgentSessionID. No adapter may resume on AgentSessionStarted alone: that
	// flag is set at spawn, so it cannot say the agent ever minted the id. The
	// Codex adapter satisfies this with the flag; opencode gets the same answer
	// from the `ses_` prefix, which only a reported id carries; Claude Code
	// hands its own id in on the command line, so the pre-minted one is real.
	AgentSessionIDConfirmed bool
	// WorkDir is the absolute directory the agent should start in (~ is
	// already expanded).
	WorkDir string
	// Model is the model the operator picked for this session, in whatever
	// spelling the agent's own CLI takes — jind-ai does not normalise it, and
	// the three CLIs do not agree (opencode wants `provider/model`, Claude
	// Code takes an alias like `opus`). Empty means no model was named: emit
	// no flag at all rather than an empty one.
	//
	// It is persisted, so every resume replays whatever was stored — a record
	// written by an older jind-ai, or edited by hand, reaches SpawnCommand
	// having passed no gate. Treat it the way the adapters treat session ids:
	// SpawnPlan's shell-safety contract applies, so it belongs in ExtraEnv.
	Model string
	// CustomEnv carries user-configured env vars from config.yaml. The
	// Manager forwards them to the shell command; adapters may also read
	// them if they need to.
	CustomEnv map[string]string
}

// SpawnPlan is what an Agent adapter returns to describe how to launch the
// agent. Manager splices the pieces into the fixed shell template it uses to
// wrap every session (`cd DIR; env -u ... KEY=VAL SHELL -ic 'COMMAND'`).
//
// The two fields are NOT alike:
//
//   - **Command is executed as a shell command. Never build it out of a value
//     you did not choose.** It becomes the argument to `SHELL -ic`, so
//     `$(...)`, backticks and `;` are live. The single quotes Manager wraps it
//     in protect the OUTER shell, not the command's own contents. This was not
//     theoretical: the opencode adapter concatenated a session id taken from an
//     unvalidated hook payload, and `ses_x$(touch F)` ran on the next resume.
//   - **ExtraEnv values are data.** Manager single-quotes each one, so
//     arbitrary content survives verbatim. Pass them raw; pre-escaping is the
//     adapter's bug, not Manager's.
//   - ExtraEnv keys and UnsetEnv entries must be POSIX env-var names matching
//     [A-Za-z_][A-Za-z0-9_]*. Manager rejects any that don't, before the
//     process is spawned.
//
// So an adapter that needs an untrusted value on the command line puts the
// value in ExtraEnv and names it from Command:
//
//	Command:  `opencode --session "$JIN_OPENCODE_SESSION"`,
//	ExtraEnv: map[string]string{"JIN_OPENCODE_SESSION": id},
//
// A shell does not re-scan the result of a parameter expansion, so the value
// arrives as one argument however it is spelled.
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
	// Resumed reports that Command continues an existing agent session rather
	// than starting one. Manager keeps it to tell a failed resume from a fresh
	// start that died: only the first is worth retrying with a new id. It does
	// not say why the process ended — see classifyPaneDeath for what does.
	Resumed bool
}

// StatusSignal is the raw event an agent adapter interprets to decide the
// session's next Status. Manager builds it from whatever channel it caught
// the signal on (hook callback, pane output tail, ...) and hands it to
// StatusSource.Interpret.
type StatusSignal struct {
	// Kind identifies the transport: "hook" (live agent hook callback) or
	// "recover" (daemon-restart recovery asking the adapter to re-derive a
	// possibly stale status from its own persistent data). Adapters switch
	// on this and ignore signals they don't understand.
	//
	// For "recover" Manager applies only StatusUpdate.Status: a stale-state
	// correction re-derives where a session already stands, so it must not
	// fire notifications or touch the error field.
	Kind string
	// Payload is an untyped key/value bag; the exact keys depend on Kind
	// and are adapter-defined. For "hook" the Manager fills in "event",
	// "notification_type", "stop_reason", "cwd"; for "recover" it fills in
	// "persisted_status", "agent_session_id", "workdir".
	Payload map[string]string
}

// StatusUpdate is the adapter's verdict on a signal: which Status the
// session should move to, whether a desktop notification should fire, and any
// explicit transition for the independent human-answer evidence axis.
//
// ErrorMessage / ClearError work as a tri-state:
//
//   - ErrorMessage != ""            → set the field (adapter has a message)
//   - ClearError == true            → clear the field (agent recovered)
//   - both zero                     → leave whatever was there in place
type StatusUpdate struct {
	Status       Status
	ErrorMessage string
	ClearError   bool
	// NeedsAnswer is an explicit human-wait verdict from the adapter. Manager
	// applies it only when reliable-needs-answer is declared supported; raw
	// event names and StatusPermission are never promoted on their own.
	NeedsAnswer NeedsAnswerSignal
	// Liveness marks a verdict that reports the agent is alive rather than
	// that a turn began — a tool finishing, say, which can only happen inside
	// a turn something else already opened. Manager honours it on the "hook"
	// path by withholding such a verdict from a session sitting idle; the
	// "recover" path ignores it along with everything else but Status.
	//
	// The flag exists because an agent can raise a hook for work that is not
	// the turn this session is waiting on — a subagent's tool, finishing after
	// the parent's turn ended. See docs/gotchas.md ("Hook") for the
	// measurement.
	//
	// The zero value is "this verdict may open a turn".
	Liveness bool
	Notify   NotifyKind
}

// NeedsAnswerSignal is an adapter's explicit transition for the independent
// human-wait evidence axis. None leaves it untouched.
type NeedsAnswerSignal uint8

const (
	NeedsAnswerSignalNone NeedsAnswerSignal = iota
	NeedsAnswerSignalRequired
	NeedsAnswerSignalResolved
)

// NotifyKind is the abstract notification category an adapter attaches to
// a StatusUpdate. Manager forwards it unchanged as plugin.Event.NotifyKind
// (surfaced to plugin runtimes as JIN_NOTIFY_KIND / notify_kind JSON).
type NotifyKind string

const (
	// NotifyNone is the zero value; the transition carries no notification.
	NotifyNone NotifyKind = ""
	// NotifyTaskComplete signals that the assistant finished a turn.
	NotifyTaskComplete NotifyKind = "task-complete"
	// NotifyError signals that the assistant reported a failure.
	// ErrorMessage on StatusUpdate is passed through.
	NotifyError NotifyKind = "error"
	// NotifyPermission signals that the agent is blocked waiting for
	// user approval.
	NotifyPermission NotifyKind = "permission"
)

// TranscriptSource returns an agent's own conversation as jind-ai's shared
// transcript.Entry form, which is what `jin session result` serialises. Each
// agent stores its conversation differently, so the translation into
// Entry/Block lives with the adapter and only the result shape is common.
//
// Contract, matching what transcript.Reader already does:
//
//   - since is an exclusive lower bound compared as a string. An entry whose
//     Timestamp is <= since is dropped.
//   - A session that has no log file yet returns (nil, nil), not an error.
//   - Errors are for genuine read failures only.
//   - workDir is a hint, not a key: ignore it if the log is locatable by
//     session ID alone.
//
// An Entry is the conversation as an operator would read it. Context the agent
// injected on the operator's behalf is not conversation — environment blocks,
// skill bodies, system prompts — and neither are a subagent's own turns. A
// reader may either drop those while reading (Codex) or emit them with
// Entry.Injected / Entry.Sidechain set (Claude Code, which also feeds `jin
// session result` and has always returned every line). Shared views over
// []Entry skip flagged entries, so the operator-facing answer is the same.
//
// **A reader that cannot classify an entry must drop it, never emit it
// unflagged.** Injected == false is read everywhere as "checked, and this is
// the operator's", not as "unknown". Deriving the previews without provenance
// surfaced the body of an invoked skill as the last user message on 55 of 231
// real transcripts.
//
// One method, deliberately. Views over a conversation — last message, last N
// exchanges, truncation — are kind-independent policy and belong in shared
// functions over []Entry. What it does not say is what a read costs, and the
// callers differ by orders of magnitude; PollableTranscriptSource is how a
// reader says which it is.
type TranscriptSource interface {
	ReadEntries(workDir, sessionID, since string) ([]transcript.Entry, error)
}

// PollableTranscriptSource is a TranscriptSource whose ReadEntries is cheap
// enough to call on a timer, once per session per refresh.
//
// Reading a local file qualifies; spawning a process does not. The opencode
// adapter asks opencode to print the session, so it deliberately does not
// implement this: on a list refreshed every two seconds, a preview would mean
// one process per row per refresh, permanently.
//
// Opt-in rather than opt-out. An adapter that forgets to declare itself loses
// its previews — visible, and harmless. The opposite default would let a new
// expensive reader melt the list, and no test on either side would catch it,
// because neither the reader nor the preview is wrong on its own.
//
// Callers on a polling path must type-assert for this interface and skip the
// source when it is absent. Callers with a per-command budget — handleResult —
// must not: refusing to read there would turn a slow answer into no answer.
type PollableTranscriptSource interface {
	TranscriptSource
	// CheapEnoughToPoll declares the fact by existing, and returns nothing on
	// purpose. A bool would let a reader answer false, which says exactly what
	// not implementing the interface already says.
	CheapEnoughToPoll()
}

// StatusSource translates raw StatusSignals into StatusUpdates. Adapters
// return (StatusUpdate{}, false) when a signal is meaningful but does not
// warrant a Status change (Manager still applies side effects such as CWD
// tracking).
type StatusSource interface {
	Interpret(StatusSignal) (StatusUpdate, bool)
}

// SetupContext is the input to Agent.Setup, called once per spawn — the start
// path and the quick-fail resume retry both go through it — before the shell
// command is built. Adapters use it to write agent-side config files (Claude
// Code's hooks-settings.json, trust dialog state, ...).
//
// It is narrower than SpawnOptions on purpose, and the difference is the
// contract: nothing about preparing a directory should depend on whether the
// session is being resumed, so Setup cannot see AgentSessionID.
type SetupContext struct {
	// StateDir is jind-ai's persistent state directory
	// (~/.local/state/jind-ai).
	StateDir string
	// ExecPath is the jin binary this session's children re-enter to call
	// back: the command an adapter writes into the agent's hook wiring, and
	// the path the opencode adapter bakes into the plugin it materialises.
	// It is not os.Executable() but the stable copy EstablishHookBinary took,
	// so the value belongs to the calling Manager rather than to the process.
	ExecPath string
	// WorkDir is the absolute working directory the session will start in.
	WorkDir string
}

// BlockKind identifies the sort of blocking prompt an agent's TUI is showing
// while it waits for a person to answer.
//
// Not every kind can be answered. A kind exists for a screen jin can only
// RECOGNISE, precisely so RespondToBlock can refuse it by name instead of
// typing into it. A screen the adapter cannot classify must come back as
// BlockNone, which is what makes RespondToBlock send nothing at all.
type BlockKind string

const (
	// BlockNone means the capture shows no blocking prompt.
	BlockNone BlockKind = ""
	// BlockPermission is a tool-approval dialog offering numbered choices.
	BlockPermission BlockKind = "tool-permission"
	// BlockQuestion is one question with numbered answers.
	BlockQuestion BlockKind = "question"
	// BlockQuestionMulti is several questions gathered into one form.
	// Recognised but not answerable: answering one question leaves the form
	// standing, so "did the block clear?" — the only post-condition
	// RespondToBlock has — cannot tell a half-answered form from an answer
	// that never landed.
	BlockQuestionMulti BlockKind = "question-multi"
	// BlockQuestionSubmit is such a form waiting on its final confirmation.
	// Recognised but not answerable, for the same reason.
	BlockQuestionSubmit BlockKind = "question-submit"
)

// Answerable reports whether RespondToBlock can drive this kind.
//
// It is deliberately a whitelist: a kind added later is unanswerable until
// someone writes it in here, which is the safe direction for a value that
// decides whether keys reach an approval dialog.
func (k BlockKind) Answerable() bool {
	return k == BlockPermission || k == BlockQuestion
}

// The bounds a BlockAnswer has to satisfy, enforced at both the edge and the
// adapter:
//
//   - MaxAnswerOption is a consequence of delivery. An answer is sent as ONE
//     keystroke, so "12" would go out as "1" then "2" — and on a numbered
//     dialog the "1" selects and commits an answer by itself.
//   - MaxAnswerTextBytes is a consequence of verification. An agent folds a
//     read that large into a placeholder (see sendChunkMaxBytes), hiding the
//     very text RespondToBlock looks for before pressing the key that submits
//     it.
const (
	MaxAnswerOption    = 9
	MaxAnswerTextBytes = 700
)

// BlockAnswer is the answer a caller wants to give a blocking prompt.
// Exactly one field carries it; the daemon rejects a request that sets both
// or neither, so an adapter may assume it received one of the two.
type BlockAnswer struct {
	// Option is a choice's on-screen number, 1-based.
	Option int
	// Text is free text, for a prompt that offers a free-text entry.
	Text string
}

// KeyStep is one step of the key sequence that answers a blocking prompt.
// Exactly one of Key and Literal is set.
type KeyStep struct {
	// Key is a tmux key name ("Enter", "Down"), sent via SendKeys.
	Key string
	// Literal is text typed verbatim, sent via SendKeysLiteral.
	Literal string
	// Verify asks Manager to confirm this step's Literal rendered into the
	// pane before it runs the next step, and to abandon the sequence when it
	// did not.
	//
	// Set it only where the adapter has measured that the agent draws the
	// text. A step whose effect cannot be seen must not claim it can: the
	// steps after it — a committing Enter, typically — would then run on a
	// guess about what the earlier ones did.
	Verify bool
}

// Agent is the interface every agent adapter satisfies. The Manager holds it
// via AgentResolver and never imports a concrete adapter package.
//
// Implementations must be safe for concurrent use: Setup and SpawnCommand
// may be invoked from multiple goroutines (per-session goroutines that
// captureOutputTmux spawns).
//
// Everything an adapter may decline is a method here rather than a side
// interface, so that an adapter which forgets one fails to compile. Each
// opt-out is spelled below, and every one of them fails silently if it is
// reached by omission instead.
//
// Nothing an adapter is HANDED may be carried out of the call that handed it.
// The registry gives out ONE instance per kind for the whole process
// (internal/agent/registry.go), while StateDir and ExecPath belong to the
// Manager running one session, so an adapter that remembered either would
// answer for whichever Manager wrote last. State of an adapter's own — Codex
// caches uuid→rollout paths, and every adapter builds its enhancer once — is
// its own to make safe.
type Agent interface {
	// Kind returns the short identifier stored in Session.AgentKind
	// ("claude", "codex", ...).
	Kind() string
	// Setup prepares any agent-global or per-workDir state that must exist
	// before the process is spawned. Called once per spawn — the start path
	// and the quick-fail resume retry both go through it. Errors are logged
	// but do not abort the launch.
	//
	// Whatever an implementation touches on its own account is still its own
	// to make safe — the Claude Code adapter's Setup takes claude.trustMu to
	// edit ~/.claude.json.
	Setup(SetupContext) error
	// SpawnCommand returns the shell command + env additions that launch
	// (or resume) the agent for the given session.
	SpawnCommand(SpawnOptions) SpawnPlan
	// RecognizesSessionID reports whether id is written the way this
	// adapter's agent writes its own session ids. Manager asks before
	// letting a hook payload re-key Session.AgentSessionID.
	//
	// Shape, not ownership: a well-formed id belonging to a different session
	// of the same kind passes, and Manager has no way to know. What the gate
	// narrows is which VALUES can be recorded.
	//
	// Answer the LOOSE question. Manager applies a kind-independent safety
	// gate first (see safeAgentSessionID), so this predicate is free to accept
	// anything shaped like an id this agent could mint, including formats it
	// has not shipped yet. Being wrong in the strict direction is the
	// expensive one — a refused id leaves opencode resuming nothing, with the
	// operator's conversation simply absent.
	RecognizesSessionID(id string) bool
	// StatusSource returns the adapter's interpreter for StatusSignals.
	// Must never return nil (agents that don't observe status can return a
	// no-op implementation).
	StatusSource() StatusSource
	// Description returns the adapter's Layer C description enhancer, or
	// nil if the adapter cannot produce structured descriptions.
	Description() DescriptionEnhancer
	// Transcript returns the adapter's reader for the agent's own on-disk
	// conversation log, or nil when this adapter cannot read one.
	//
	// nil means "cannot read", never "the conversation is empty". A caller
	// answering *with* the conversation must fail on nil — that is `session
	// result`, where zero entries and success is indistinguishable from a
	// child agent that ran and said nothing. A caller merely decorating
	// something it renders anyway may stay silent, which is what
	// Manager.AttachLastMessages does for list rows. Neither may dress nil up
	// as an empty conversation.
	Transcript() TranscriptSource
	// ClearInputKeys returns the tmux key names (SendKeys form — e.g.
	// "C-u", "C-a", "BSpace" — not literal text) that clear this adapter's
	// TUI input line to empty. Manager.SendPrompt sends these before each
	// send attempt so residual text cannot concatenate with the new prompt.
	//
	// Return nil to opt out: SendPrompt then carries the residual-concat risk
	// documented in docs/gotchas.md "Session send". An adapter whose TUI has
	// no safe clear sequence — one that rebinds C-u, say — should return nil
	// rather than sending keys with side effects.
	ClearInputKeys() []string
	// PastePlaceholder returns the text this adapter's TUI will show for
	// prompt when it arrives as a single bracketed paste, or "" to receive
	// prompts as literal keystrokes instead (the default).
	//
	// Returning a placeholder selects the paste transport. A TUI usually
	// collapses a large paste into a summary line, so the prompt text is not
	// on screen and SendPrompt's usual tail match cannot succeed; what it
	// looks for instead is exactly the string returned here. The adapter owns
	// both the wording and whatever quantity it embeds; the manager owns only
	// the comparison.
	//
	// Worth it only where typing is pathologically expensive — OpenCode is the
	// one shipped adapter where it is, by two orders of magnitude (see
	// docs/gotchas.md "Session send"). Claude Code could not use this anyway:
	// its summary numbers pastes ("#1", "#2") rather than measuring them.
	//
	// The returned text is matched after SendPrompt's usual normalization.
	PastePlaceholder(prompt string) string
	// DismissOverlayKeys returns the tmux key names that close any completion
	// overlay this adapter's TUI leaves open once prompt has been typed in
	// full, or nil when prompt cannot open one.
	//
	// This exists because SendPrompt's verify proves the prompt's tail is
	// rendered in the input area, which is NOT the same as "Enter will submit
	// it". Measured on Claude Code 2.1.224, a prompt ending in an in-progress
	// completion token leaves an overlay open and Enter is consumed to accept
	// a candidate: the prompt is rewritten in place, never submitted, and
	// SendPrompt still returns nil (3/3).
	//
	// The prompt is a parameter because the key is not free: on Claude Code,
	// Escape also interrupts a running turn (2/3). Return keys only for
	// prompts that can actually open an overlay. Return nil to opt out — the
	// correct answer for an adapter whose overlay behaviour has not been
	// measured, since sending a key on a guess is how this bug is introduced.
	DismissOverlayKeys(prompt string) []string
	// DetectBlock reports which blocking prompt the captured pane shows, or
	// BlockNone when it shows none.
	//
	// Manager asks this both questions it has to settle — "is there anything
	// to answer?" before it sends a key, and "did the answer take?" after — so
	// the two can never be judged by different rules.
	//
	// Manager sends nothing on BlockNone, so a screen the adapter does not
	// recognise costs a refusal. Only a positive, answerable verdict puts keys
	// in the pane, which is why an adapter should order its checks so the
	// kinds it cannot drive are ruled out first.
	//
	// The parameter is the capture rather than a session, so adapter tests
	// need a string and not a tmux server. Return BlockNone unconditionally to
	// opt out.
	DetectBlock(capture string) BlockKind
	// AnswerBlockKeys returns the keys that answer kind with ans, or an error
	// explaining why this agent cannot express that answer.
	//
	// capture is the same snapshot DetectBlock classified, not a fresh one, so
	// an adapter that has to read the screen reads the frame the verdict was
	// made against rather than a later one that may have moved.
	//
	// An error is a refusal, and Manager has sent nothing by the time it
	// arrives. The message is the entirety of what the caller gets, so it
	// should name what to do instead rather than only what failed. Return an
	// error unconditionally to opt out.
	AnswerBlockKeys(kind BlockKind, capture string, ans BlockAnswer) ([]KeyStep, error)
}

// AgentResolver bridges the Manager to the process-global agent registry
// that lives in internal/agent. The daemon injects a thin implementation
// that delegates to agent.Lookup; the session package never sees the
// registry itself, keeping the import direction one-way.
type AgentResolver interface {
	Resolve(kind string) (Agent, error)
}

// AgentCatalog is the optional discovery surface used by local pane adoption.
// Existing resolvers that only need to spawn a known kind can keep implementing
// AgentResolver alone. A catalog returns a snapshot in any order; detection
// sorts it before producing user-visible candidates.
type AgentCatalog interface {
	Agents() []Agent
}

// AgentExecutableDetector is the opt-in predicate an adapter supplies for
// classifying process evidence. The input is a normalized executable basename,
// never a shell command line. Detection is identity evidence only: a positive
// result must not be used to infer hooks, resume, transcript, or response
// support; those remain governed by AgentCapabilities.
type AgentExecutableDetector interface {
	RecognizesExecutable(name string) bool
}
