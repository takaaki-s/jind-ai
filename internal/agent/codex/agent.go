// Package codex is the OpenAI Codex CLI adapter. It owns every Codex-specific
// concern: hook injection via per-invocation -c overrides, the SessionStart
// write-back path Codex needs because it has no `--session-id` equivalent, the
// shell command shape, and the rollout-derived Layer C-transcript enhancer.
//
// Register instances via the internal/agent/register blank-import package so
// the daemon can Lookup("codex") them at start-up.
package codex

import (
	"fmt"
	"os"

	"github.com/takaaki-s/jind-ai/internal/agent"
)

// Agent is the Codex adapter state, one instance shared by the whole process,
// so nothing here may come from a Setup:
//
//   - locator is built once, from a home directory resolved eagerly at
//     construction, and shared by enhancer and every TranscriptReader
//     Transcript() hands out, so a rollout path either resolves is cached for
//     both — see rollout.go's Locator cache.
//   - enhancer and statusSrc are cached instances so hot-path calls to
//     Description() / StatusSource() do not reallocate on every hook.
//
// The FIELDS are read without a lock, but what locator points at is not still:
// it caches uuid->rollout paths behind its own mutex, and SpawnCommand writes
// to that cache on a resume.
type Agent struct {
	locator   *Locator
	enhancer  *DescriptionEnhancer
	statusSrc *HookStatusSource
}

// New returns a fully-wired Codex adapter. Home dir is resolved eagerly; if
// the lookup fails, locator falls back to a relative path (which will simply
// never find any rollout — TryGenerate then returns false and the session
// keeps whatever description Layer A/B provided).
func New() *Agent {
	home, _ := os.UserHomeDir()
	loc := NewLocator(home)
	return &Agent{
		locator:   loc,
		enhancer:  NewDescriptionEnhancer(loc),
		statusSrc: NewHookStatusSource(),
	}
}

// Kind is the identifier jind-ai persists in Session.AgentKind.
func (a *Agent) Kind() string { return "codex" }

// RecognizesExecutable identifies only the Codex CLI process. Capability
// support is deliberately not inferred from this match.
func (a *Agent) RecognizesExecutable(name string) bool { return name == "codex" }

// Capabilities records the two deliberate Codex opt-outs alongside the
// surfaces the adapter implements. PermissionRequest is not treated as a
// reliable human-wait signal, and its prompt cannot be answered by jin.
func (a *Agent) Capabilities() agent.AgentCapabilities {
	return agent.AgentCapabilities{
		SchemaVersion:       agent.AgentCapabilitiesSchemaVersion,
		Liveness:            agent.CapabilitySupported,
		Send:                agent.CapabilitySupported,
		Respond:             agent.CapabilityUnsupported,
		Resume:              agent.CapabilitySupported,
		Hooks:               agent.CapabilitySupported,
		Transcript:          agent.CapabilitySupported,
		ReliableNeedsAnswer: agent.CapabilityUnsupported,
	}
}

// RecognizesSessionID accepts anything written as a UUID. Codex has no
// --session-id equivalent, so the real id only ever arrives through the
// SessionStart hook payload, and this predicate is what stands between that
// payload and the record. Refusing a genuine id leaves the pre-minted UUID in
// place, and Session.AgentSessionIDConfirmed stays false, so the next spawn
// starts fresh — the cost is a session that starts over rather than one that
// quietly turns out to be empty.
func (a *Agent) RecognizesSessionID(id string) bool { return agent.LooksLikeUUID(id) }

// Setup does nothing, and this adapter is the one where that is the whole
// story rather than an omission. Codex hooks are injected per-invocation on
// the command line, so there is no file to write: ~/.codex/hooks.json and
// config.toml both stay untouched. See "Agent Adapters" in
// docs/architecture.md for the design principle behind that.
func (a *Agent) Setup(agent.SetupContext) error { return nil }

// SpawnCommand delegates to the package-level builder, which reads the jin
// binary straight off opts. An empty ExecPath — reachable in production when
// the daemon could resolve none — falls back to a hook-less `codex` invocation.
//
// A resume evicts AgentSessionID from locator's cache first. Whether `codex
// resume` can retarget a UUID onto a different rollout file is unverified (see
// rollout.go), but if it can, the next Find must re-glob rather than keep
// answering with whatever the cache resolved before the resume.
func (a *Agent) SpawnCommand(opts agent.SpawnOptions) agent.SpawnPlan {
	if isResume(opts) {
		a.locator.invalidate(opts.AgentSessionID)
	}
	return SpawnCommand(opts)
}

// StatusSource returns the cached hook-event interpreter.
func (a *Agent) StatusSource() agent.StatusSource { return a.statusSrc }

// Description returns the cached Layer C-transcript enhancer that pulls
// the first genuine user prompt out of the rollout JSONL.
func (a *Agent) Description() agent.DescriptionSource { return a.enhancer }

// Transcript returns the rollout reader. It is a partial view of what Claude
// Code's reader gives — Codex records no token usage per message, no error
// flag on a tool result, and one tool name for every call — and transcript.go
// marks each gap where the mapping loses something.
//
// The wrapper is built fresh per call, unlike enhancer and statusSrc above: it
// holds nothing but a Locator pointer, and constructing it measured at 240ns
// against a read that takes milliseconds. What it wraps is not fresh, though —
// it shares locator with enhancer, so a `jin session result` call and a
// hook-driven description attempt hit the same uuid->path cache.
func (a *Agent) Transcript() agent.TranscriptSource { return NewTranscriptReader(a.locator) }

// ClearInputKeys returns the tmux keys Manager.SendPrompt sends before each
// attempt to wipe Codex's input line. C-u is the standard readline kill-line
// binding and Codex's TUI honours it.
func (a *Agent) ClearInputKeys() []string { return []string{"C-u"} }

// PastePlaceholder returns "": Codex receives prompts as keystrokes because
// at 16KB the keystroke path verifies in 1.7s, so trading the tail match
// for a placeholder count buys nothing. Its placeholder does carry an exact
// byte count ("[Pasted Content N chars]"), so this could be revisited if a
// size shows up that typing cannot reach.
func (a *Agent) PastePlaceholder(string) string { return "" }

// DismissOverlayKeys returns nil: Codex's completion behaviour has not been
// measured, so there is nothing to base a key on.
//
// Claude Code was found to leave a completion overlay open for prompts ending
// in an in-progress token, which then eats the Enter SendPrompt presses. Codex
// has slash commands too, so it may well share the defect — but the run that
// would have settled it could not be made. Guessing a key here would be the
// same mistake in the other direction: Escape is destructive on a running
// turn. Measure first, then return keys.
func (a *Agent) DismissOverlayKeys(string) []string { return nil }

// DetectBlock reports BlockNone always: what Codex puts on screen while it
// waits for an approval has not been measured, so there is no literal to match
// against.
//
// The attempt is worth recording, because the next person will otherwise
// repeat it. Reaching an approval dialog needs a model turn that asks to run
// something, and the account was over its usage limit on both models it offers
// (2/2 refused). Two OTHER Codex menus were reachable — the directory-trust
// prompt and a rate-limit model switch — and both are numbered `› 1. ...`
// lists where a bare digit selects and confirms (2/2). That is suggestive and
// it is not evidence.
//
// The cost of guessing is not symmetric with the cost of waiting: a wrong
// literal makes DetectBlock report a block that is not there, and
// RespondToBlock then types a digit into whatever the pane actually holds.
func (a *Agent) DetectBlock(string) agent.BlockKind { return agent.BlockNone }

// AnswerBlockKeys refuses always, for the reason DetectBlock returns
// BlockNone. It is unreachable through RespondToBlock while DetectBlock opts
// out — Manager stops at BlockNone — but it is the message a caller would
// get if that ever changed, so it names the state of play rather than
// blaming the caller.
func (a *Agent) AnswerBlockKeys(agent.BlockKind, string, agent.BlockAnswer) ([]agent.KeyStep, error) {
	return nil, fmt.Errorf("codex sessions cannot be answered by jin: " +
		"its approval prompt has not been measured, so jin does not know what keys it takes. " +
		"Attach the session and answer it directly")
}
