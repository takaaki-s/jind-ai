// Package claude is the Claude Code adapter. It owns every CC-specific
// concern: hook language, hooks-settings.json generation, trust-dialog
// suppression, the shell command shape, and the transcript-derived description
// enhancer.
//
// Register instances via the internal/agent/register blank-import package so
// the daemon can Lookup("claude") them at start-up.
package claude

import (
	"strings"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/debug"
)

// claudeLog is shared across the whole adapter (agent / trust / hooks_settings)
// so debug output for CC-specific setup goes to a single logger instance.
var claudeLog = debug.NewLogger("daemon-debug.log")

// Agent is the Claude Code adapter state. One instance serves every session,
// so nothing here may come from a Setup: enhancer holds a transcript reader
// whose only state is the ~/.claude directory path, and statusSrc is stateless
// but held as a value so a hook event does not allocate one. Both are built in
// New and never reassigned, so neither needs a lock.
type Agent struct {
	enhancer  *CCDescriptionEnhancer
	statusSrc *HookStatusSource
}

// New returns a fully-wired Claude Code adapter.
func New() *Agent {
	return &Agent{
		enhancer:  NewCCDescriptionEnhancer(),
		statusSrc: NewHookStatusSource(),
	}
}

// Kind is the identifier jind-ai persists in Session.AgentKind.
func (a *Agent) Kind() string { return "claude" }

// RecognizesExecutable identifies only Claude Code's own executable. Wrappers
// are handled by the shared process-tree walker rather than broadened here.
func (a *Agent) RecognizesExecutable(name string) bool { return name == "claude" }

// Capabilities declares adapter support, independent of whether one running
// process currently has healthy hook wiring.
func (a *Agent) Capabilities() agent.AgentCapabilities {
	return agent.AgentCapabilities{
		SchemaVersion:       agent.AgentCapabilitiesSchemaVersion,
		Liveness:            agent.CapabilitySupported,
		Send:                agent.CapabilitySupported,
		Respond:             agent.CapabilitySupported,
		Resume:              agent.CapabilitySupported,
		Hooks:               agent.CapabilitySupported,
		Transcript:          agent.CapabilitySupported,
		ReliableNeedsAnswer: agent.CapabilitySupported,
	}
}

// RecognizesSessionID accepts anything written as a UUID. Claude Code is told
// its session id rather than minting one, so a re-key is the exception rather
// than the rule — and the value kept on a refusal is the one CC was launched
// with, which makes this the cheapest of the three adapters to refuse.
func (a *Agent) RecognizesSessionID(id string) bool { return agent.LooksLikeUUID(id) }

// StatusSource returns the CC hook interpreter.
func (a *Agent) StatusSource() agent.StatusSource { return a.statusSrc }

// Description returns the Layer C enhancer that mines the CC transcript for
// a better human-readable label.
func (a *Agent) Description() agent.DescriptionSource { return a.enhancer }

// Transcript returns the Claude Code transcript reader. transcript.Reader's
// ReadEntries already has the signature TranscriptSource declares, so this
// hands it over unwrapped, built per call — caching it buys nothing.
func (a *Agent) Transcript() agent.TranscriptSource { return NewTranscriptReader() }

// Setup writes hooks-settings.json into ctx.StateDir and the per-workDir trust
// flag. Both failures are logged but do not abort the session start.
//
// It writes on every spawn rather than once, which makes the file
// self-healing: one deleted or hand-edited by mistake comes back at the next
// session start. The write is published by rename, so a session starting
// alongside this one never reads a half-written file.
//
// A failed write is not repaired here and does not need to be: SpawnCommand
// asks ctx.StateDir what it can still serve, so a session whose own write
// failed still gets --settings from an earlier spawn's file, and with nothing
// usable there the flag is simply omitted.
func (a *Agent) Setup(ctx agent.SetupContext) error {
	if _, err := EnsureHooksSettingsFile(ctx.StateDir, ctx.ExecPath); err != nil {
		claudeLog("[HOOKS] Warning: failed to generate hooks settings: %v", err)
	}
	if err := EnsureTrustState(ctx.WorkDir); err != nil {
		claudeLog("[TRUST] Warning: failed to set trust state: %v", err)
	}
	return nil
}

// ClearInputKeys returns the tmux keys Manager.SendPrompt sends before each
// attempt to wipe Claude Code's input line. C-u is the standard readline
// kill-line binding and Claude Code's TUI honours it.
func (a *Agent) ClearInputKeys() []string { return []string{"C-u"} }

// PastePlaceholder returns "": Claude Code receives prompts as keystrokes because
// its placeholder numbers pastes ("#1", "#2", ...) rather than
// measuring them, so a fold would leave nothing to verify against — and at
// 32KB the keystroke path is fast enough that there is nothing to gain.
func (a *Agent) PastePlaceholder(string) string { return "" }

// DismissOverlayKeys returns Escape for prompts that leave a completion
// overlay open, and nil for every other prompt.
//
// Escape is the right key and it is not a free one. Measured on Claude Code
// 2.1.224 against a throwaway tmux server, 3/3 per row:
//
//	prompt                     without Escape                with Escape
//	list @internal/agent       Enter eaten; input left       submitted verbatim
//	                           holding "list
//	                           @internal/agentdocs/"
//	/<ambiguous-prefix>        ran a DIFFERENT command       submitted as sent
//	/<exact-command>           ran as sent                   ran as sent
//	say pong only              submitted                     submitted
//
// The third row is what justifies returning keys for bare slash commands even
// though nothing is drawn on screen: a burst `send-keys -l` write does not
// render the overlay at all, yet the selection is live and SendPrompt's nudge
// key walks it onto a different entry. Escape discards that selection.
//
// Not for every prompt, because Escape also interrupts a running turn (2/3).
// SendPrompt only sends while a session reads as idle, but jin is known to
// report idle while a sub-agent runs, so restricting the key to prompts that
// can open an overlay keeps that misfire away from ordinary sends.
func (a *Agent) DismissOverlayKeys(prompt string) []string {
	if !opensCompletionOverlay(prompt) {
		return nil
	}
	return []string{"Escape"}
}

// opensCompletionOverlay reports whether Claude Code still has a completion
// overlay open once prompt has been typed in full.
//
// The overlay survives only while the caret sits inside the token that opened
// it. Measured 3/3 per case: "list @internal/agent" keeps it open, "list
// @internal/agent and say ok" does not, and a slash that is not the first
// character of the input never opens one. Requiring "@" at the START of the
// final token is measured too, at the counts each case got: a backtick-wrapped
// path (3/3), a double-quoted one (2/2) and a mid-sentence email address (1/1)
// all drew no overlay and submitted verbatim. See overlay_test.go for the
// exact prompts.
//
// Whitespace is dropped before the check, which errs toward true for a
// trailing space. That is the safe direction: an unnecessary Escape was
// measured harmless (3/3), while a missed one leaves the defect as it was. It
// says nothing about a trigger neither rule names.
//
// "#" was measured to draw no overlay (3/3) and is deliberately absent. What
// its Enter does was not measured: "#" opens a save-destination dialog, and
// finding out costs a write to a memory file.
func opensCompletionOverlay(prompt string) bool {
	// One split, read by both rules — they are two readings of the same
	// tokenization, not two independent parses. Fields splits on newlines
	// too, so the final token is the prompt's, not its first line's.
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false
	}
	// A slash command is the whole input or it is not a command at all.
	if len(fields) == 1 && strings.HasPrefix(fields[0], "/") {
		return true
	}
	// A file reference keeps its picker open only while it is the last thing
	// typed.
	return strings.HasPrefix(fields[len(fields)-1], "@")
}
