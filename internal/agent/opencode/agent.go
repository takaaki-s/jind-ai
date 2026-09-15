// Package opencode is the opencode (sst/opencode) adapter. opencode has no
// hook-command surface of its own — its extension point is a Bun-runtime
// TypeScript plugin — so this adapter carries a plugin in its binary,
// materialises it under jind-ai's own state directory, and points opencode at
// that directory with OPENCODE_CONFIG_DIR.
//
// OPENCODE_CONFIG_DIR is additive, not a replacement: ConfigPaths.directories()
// returns unique([~/.config/opencode, ...project .opencode dirs,
// ...$OPENCODE_CONFIG_DIR]), so the user's own agents / commands / plugins keep
// loading and jind-ai never has to write to ~/.config/opencode or to the user's
// repository.
//
// Register instances via the internal/agent/register blank-import package so
// the daemon can Lookup("opencode") them at start-up.
package opencode

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/debug"
)

var opencodeLog = debug.NewLogger("daemon-debug.log")

// Agent is the process-wide opencode adapter state. Nothing here comes from a
// Setup: statusSrc is cached only so a hook does not reallocate one, and it is
// built in New and never reassigned, so it needs no lock. The
// OPENCODE_CONFIG_DIR a spawn is given is worked out from SpawnOptions.StateDir
// on each call — see installedConfigDir.
type Agent struct {
	statusSrc *EventStatusSource
}

// New returns a fully-wired opencode adapter.
func New() *Agent {
	return &Agent{statusSrc: NewEventStatusSource()}
}

// Kind is the identifier jind-ai persists in Session.AgentKind.
func (a *Agent) Kind() string { return "opencode" }

// Capabilities records that opencode reports permission waits reliably even
// though jin deliberately does not yet drive its selection-based dialog.
func (a *Agent) Capabilities() agent.AgentCapabilities {
	return agent.AgentCapabilities{
		SchemaVersion:       agent.AgentCapabilitiesSchemaVersion,
		Liveness:            agent.CapabilitySupported,
		Send:                agent.CapabilitySupported,
		Respond:             agent.CapabilityUnsupported,
		Resume:              agent.CapabilitySupported,
		Hooks:               agent.CapabilitySupported,
		Transcript:          agent.CapabilitySupported,
		ReliableNeedsAnswer: agent.CapabilitySupported,
	}
}

// RecognizesSessionID answers with hasSessionIDPrefix — the LOOSE predicate,
// not isSessionID beside it — and the choice is the point.
//
// This adapter is the one that cannot afford a wrong refusal: opencode mints
// its own id, and a resume that does not happen starts a new session with
// nothing saying so. Answering with the same predicate SpawnCommand already
// gates the resume on makes the two sets identical, so the gate adds no silent
// failure that was not already there.
//
// isSessionID would break that. Its base62 alphabet is evidence about today's
// ids rather than a rule opencode promised. Injection is not the reason to
// reach for the strict test: Manager's safeAgentSessionID has already refused
// every shell metacharacter before this is called.
func (a *Agent) RecognizesSessionID(id string) bool { return hasSessionIDPrefix(id) }

// Setup materialises the bundled plugin under
// <StateDir>/opencode/plugin/jin.ts, which is where SpawnCommand looks for it
// when deciding whether to hand opencode an OPENCODE_CONFIG_DIR.
//
// Failures are logged and swallowed: losing the plugin costs live status
// reporting (the session falls back to pane-death detection), which is strictly
// better than refusing to launch. A failed write leaves whatever an earlier
// spawn published untouched, so one session's transient failure cannot disable
// status reporting for the sessions already running out of it.
func (a *Agent) Setup(ctx agent.SetupContext) error {
	dir, err := WritePlugin(ctx.StateDir, ctx.ExecPath)
	if err != nil {
		opencodeLog("[OPENCODE] Warning: failed to write plugin: %v", err)
		return nil
	}
	// Fail-open, and separately from the plugin: status reporting is what
	// makes the session usable at all, whereas the context is a convenience
	// for the agent inside it. A session that starts without knowing about
	// `jin docs` is far better than one that does not start.
	if err := WriteAgentContext(dir); err != nil {
		opencodeLog("[OPENCODE] Warning: failed to write agent context: %v", err)
	}
	return nil
}

// SpawnCommand delegates to the package-level builder with the config dir
// this spawn's own state directory can actually offer — "" when no plugin is
// installed there, which is what makes the builder omit OPENCODE_CONFIG_DIR.
func (a *Agent) SpawnCommand(opts agent.SpawnOptions) agent.SpawnPlan {
	return SpawnCommand(opts, installedConfigDir(opts.StateDir))
}

// StatusSource returns the cached interpreter for the canonical event names
// the bundled plugin normalises opencode's bus events into.
func (a *Agent) StatusSource() agent.StatusSource { return a.statusSrc }

// Description returns nil: the opencode adapter has no Layer C enhancer
// today, so sessions keep whatever description Layer A/B derived. The
// interface explicitly permits nil here.
func (a *Agent) Description() agent.DescriptionSource { return nil }

// Transcript returns the reader that asks opencode to print the session.
//
// Never nil. Whether the read can happen — whether `opencode` is on the
// daemon's PATH — is decided per call, and the answer when it is not is an
// error naming the reason. Returning nil would report "this adapter has no
// transcript reader", which is a different and untrue thing.
func (a *Agent) Transcript() agent.TranscriptSource { return NewTranscriptReader() }

// ClearInputKeys returns the tmux keys Manager.SendPrompt sends before each
// attempt to wipe opencode's input line. C-u empties it, verified against
// opencode 1.17.18.
func (a *Agent) ClearInputKeys() []string { return []string{"C-u"} }

// PastePlaceholder opts opencode into SendPrompt's paste transport, and
// predicts the summary line opencode will render for this prompt.
//
// opencode is the one adapter where typing a prompt is genuinely expensive: it
// renders each character as if keyed, so an 8KB prompt takes 88s and grows RSS
// by 770MB, while the same prompt pasted settles in 0.41s with no growth. Past
// roughly 3KB the keystroke path cannot finish inside the verify budget at all.
//
// The cost is a weaker check: a folded paste hides the text, so the line count
// is all there is to compare against. That is worth it because a paste is one
// atomic write — the chunk-boundary losses the tail match was built to catch
// cannot happen on this path — and because small pastes still arrive as plain
// text, where SendPrompt falls back to matching the tail as usual.
//
// The "~" opencode prints is decoration; the number is exact, checked at 1, 2,
// 3, 7, 33, 100, 200, 999, 1000 and 2500 lines over payloads from 900B to 62KB.
// It says "lines" even for one.
func (a *Agent) PastePlaceholder(prompt string) string {
	return fmt.Sprintf("[Pasted ~%d lines]", pasteLineCount(prompt))
}

// DismissOverlayKeys returns nil: opencode's completion behaviour has not been
// measured, so there is nothing to base a key on. The run that would have
// settled it could not be made, because the local install has no provider
// configured and never reaches a usable prompt.
//
// This adapter also takes the paste transport, where the whole prompt arrives
// as one bracketed write rather than as typing, so it is not even clear that a
// completion would be triggered. Measure before sending a key that is
// destructive when it misfires.
func (a *Agent) DismissOverlayKeys(string) []string { return nil }

// DetectBlock reports BlockNone always. Unlike the Codex adapter next to it,
// this one opts out with the measurements already in hand — written down here
// so that finishing the job is a coding task rather than another round of
// measuring.
//
// Measured against opencode 1.18.4 with `permission: {bash: "ask"}`, on a
// throwaway tmux server. The dialog is a horizontal button row, not a numbered
// list:
//
//	△ Permission required
//	  # Shell command
//	$ sha256sum /etc/hostname
//	 Allow once   Allow always   Reject    ctrl+f fullscreen  ⇆ select  enter confirm
//
//	key / input          effect
//	C-u                  inert (2/2)
//	typed prose          not drawn, not buffered — but an `h` or `l` INSIDE
//	                     it moves the selection (2/2)
//	a digit              inert (1/1) — there are no numbers to address
//	Down, Tab            inert (1/1, 2/2)
//	Right / l            selection moves right (2/2)
//	h                    selection moves left (2/2)
//	bracketed paste      inert; not drawn, not buffered (1/1) — and this is
//	                     the transport PastePlaceholder above selects
//	Escape               dismisses the dialog (3/3)
//	at either end        the selection WRAPS (1/1)
//	initial selection    "Allow once" (3/3)
//
// The selection wraps, so there is no home position and a fixed number of moves
// cannot address a button: the current one has to be read first. It can be —
// the highlight is an SGR background in `capture-pane -e` output, read
// correctly 5/5. So: read, move, read again to confirm, then Enter.
//
// It is left unimplemented because that sequence rests on reading a position,
// and a misread moves to a different button and confirms it — on a permission
// dialog, approving something nobody approved. Claude Code's digit is absolute
// and needs no position, so shipping both together would put two very different
// risks behind one flag.
func (a *Agent) DetectBlock(string) agent.BlockKind { return agent.BlockNone }

// AnswerBlockKeys refuses always, for the reason DetectBlock returns
// BlockNone. See there for the measurements an implementation would use.
func (a *Agent) AnswerBlockKeys(agent.BlockKind, string, agent.BlockAnswer) ([]agent.KeyStep, error) {
	return nil, fmt.Errorf("opencode sessions cannot be answered by jin yet: " +
		"its permission dialog is driven by moving a selection rather than by typing a choice. " +
		"Attach the session and answer it directly")
}

// pasteLineCount counts prompt's lines the way opencode counts them when it
// summarises a paste. Two details are measured, not assumed, and getting either
// wrong would reject sends that in fact landed:
//
//   - CR is a line break in its own right, so CRLF breaks twice: "a\r\nb\r\nc"
//     is reported as 5 lines, not 3.
//   - Trailing blank or whitespace-only lines are dropped, however many.
//
// Blank lines in the MIDDLE count, and a prompt with no break at all is 1.
func pasteLineCount(s string) int {
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + strings.Count(s, "\r") + 1
}
