// Package claude houses Claude Code-specific glue that must live outside
// internal/session (which is agent-agnostic). The Layer C DescriptionEnhancer
// tracks whatever name Claude Code has settled on for the conversation —
// preferring the AI-generated title CC writes to the transcript (surfaced in
// `/status` as "Session name"), and falling back to the name Claude Code
// writes to ~/.claude/sessions/<PID>.json when no AI title has been recorded
// yet.
package claude

import (
	"strings"
	"unicode/utf8"

	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/transcript"
)

// descriptionMaxBytes caps the derived description length. 60 bytes keeps the
// value from wrapping in the tabwriter-based `jin session list` output and
// leaves headroom for the locked marker.
const descriptionMaxBytes = 60

// ccNameSourceDerived is the value CC 2.x writes to nameSource when the
// session name was derived from the tmux window name (or another externally
// supplied hint) rather than generated from the conversation. jind-ai itself
// hands CC that hint, so a "derived" name round-trips our own tmux name
// (e.g. "jin-<8>-<2>") back into Description. It is still slightly better
// than the Layer A baseline — it matches CC's own /resume picker — so we
// accept it at the weak Layer C-name sublayer and let any later, stronger
// signal overwrite it.
const ccNameSourceDerived = "derived"

// CCDescriptionEnhancer implements session.DescriptionEnhancer using two
// Claude Code-specific signals, tried in order of informativeness:
//
//  1. The AI-generated title CC writes to the transcript as
//     `{"type":"ai-title","aiTitle":"…"}`. This is the same value CC shows
//     next to "Session name" in `/status` and is the closest thing CC has
//     to an authoritative conversation label.
//  2. The name field in ~/.claude/sessions/<PID>.json. When nameSource is
//     "derived" this is just the tmux hint jind-ai itself passed CC, so
//     it is downgraded to the weak sublayer.
//
// The enhancer never mines the raw first user prompt — Claude Code owns the
// session naming and jind-ai lets it lead. Other agents that lack a native
// naming path can plug their own enhancer that uses DescriptionLayerTranscript.
type CCDescriptionEnhancer struct {
	reader     *transcript.Reader
	nameReader *CCSessionNameReader
}

// NewCCDescriptionEnhancer builds an enhancer bound to the local
// ~/.claude/{projects,sessions} stores. Safe to share across goroutines:
// both underlying readers only perform read-only file I/O.
func NewCCDescriptionEnhancer() *CCDescriptionEnhancer {
	return &CCDescriptionEnhancer{
		reader:     transcript.NewReader(),
		nameReader: NewCCSessionNameReader(),
	}
}

// TryGenerate returns the best available Layer C-name candidate for sess.
//
// The tried-in-order layering is:
//
//   - Transcript aiTitle → DescriptionLayerAgentName (strong).
//     CC-authored conversation title; overrides everything below.
//   - sessions/<PID>.json name with nameSource != "derived" →
//     DescriptionLayerAgentName (strong). Same tier as aiTitle so whichever
//     one lands first is preserved by the strict-greater layer guard.
//   - sessions/<PID>.json name with nameSource == "derived" →
//     DescriptionLayerAgentNameDerived (weak). The round-trip of jind-ai's
//     own tmux hint; escapes the Baseline but lets any later stronger name
//     overwrite it.
//
// Returns ("", 0, false) when no signal is available: nil sess, missing
// AgentSessionID, no transcript file yet AND no session-name file. Never
// mutates sess. Never returns an error.
func (e *CCDescriptionEnhancer) TryGenerate(sess *session.Session) (string, session.DescriptionLayer, bool) {
	if sess == nil || sess.AgentSessionID == "" {
		return "", 0, false
	}

	if e.reader != nil {
		workDir := sess.CurrentWorkDir
		if workDir == "" {
			workDir = sess.WorkDir
		}
		if title, ok := e.reader.ReadAITitle(workDir, sess.AgentSessionID); ok {
			return smartTruncate(title, descriptionMaxBytes), session.DescriptionLayerAgentName, true
		}
	}

	if e.nameReader != nil {
		if name, src, ok := e.nameReader.LookupName(sess.AgentSessionID); ok {
			layer := session.DescriptionLayerAgentName
			if src == ccNameSourceDerived {
				layer = session.DescriptionLayerAgentNameDerived
			}
			return smartTruncate(name, descriptionMaxBytes), layer, true
		}
	}

	return "", 0, false
}

// smartTruncate keeps the first line of s and shortens it to at most maxBytes
// bytes plus a trailing horizontal ellipsis (U+2026). It prefers a whitespace
// boundary within the budget; if that boundary would drop more than half the
// budget it falls back to a hard byte cut. Hard cuts back off by one byte at a
// time to avoid producing invalid UTF-8 when the cut lands mid-rune.
//
// Returns the original string unchanged when it already fits.
func smartTruncate(s string, maxBytes int) string {
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s
	}

	cut := strings.LastIndexAny(s[:maxBytes], " \t")
	if cut < maxBytes/2 {
		cut = maxBytes
	}
	truncated := s[:cut]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	truncated = strings.TrimRight(truncated, " \t")
	return truncated + "…"
}
