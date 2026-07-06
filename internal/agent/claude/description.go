// Package claude houses Claude Code-specific glue that must live outside
// internal/session (which is agent-agnostic). The initial inhabitant is the
// Layer C DescriptionEnhancer that mines the first user prompt from a session
// transcript to upgrade the auto-generated Layer A baseline.
package claude

import (
	"strings"
	"unicode/utf8"

	"github.com/takaaki-s/jindaiko/internal/session"
	"github.com/takaaki-s/jindaiko/internal/transcript"
)

// descriptionMaxBytes caps the derived description length. 60 bytes keeps the
// value from wrapping in the tabwriter-based `jin session list` output and
// leaves headroom for the locked marker.
const descriptionMaxBytes = 60

// minPromptLen is the shortest prompt we treat as a real hint. Anything under
// three characters is almost certainly a stray keystroke or a slash-command
// abbreviation.
const minPromptLen = 3

// minSlashArgLen is the shortest args string we treat as meaningful when the
// user starts with a slash command. Below this we assume the user is still
// typing the command name or has not fed a description yet.
const minSlashArgLen = 10

// CCDescriptionEnhancer implements session.DescriptionEnhancer using two
// Claude Code-specific signals, tried in order of informativeness: the
// transcript's first user turn (Layer C-transcript), falling back to the
// session name CC writes to ~/.claude/sessions/<PID>.json at start-up
// (Layer C-name) when no transcript is available yet.
type CCDescriptionEnhancer struct {
	reader     *transcript.Reader
	nameReader *CCSessionNameReader
}

// NewCCDescriptionEnhancer builds an enhancer bound to the local ~/.claude
// transcript and sessions stores. Safe to share across goroutines: both
// underlying readers only perform read-only file I/O.
func NewCCDescriptionEnhancer() *CCDescriptionEnhancer {
	return &CCDescriptionEnhancer{
		reader:     transcript.NewReader(),
		nameReader: NewCCSessionNameReader(),
	}
}

// TryGenerate tries the transcript first since it yields the higher-quality
// Layer C-transcript description, then falls back to the CC-assigned session
// name (Layer C-name) which is available as early as SessionStart, before any
// transcript has been written. Never mutates sess.
func (e *CCDescriptionEnhancer) TryGenerate(sess *session.Session) (string, session.DescriptionLayer, bool) {
	if sess == nil || sess.AgentSessionID == "" {
		return "", 0, false
	}

	if cand, ok := e.tryTranscript(sess); ok {
		return cand, session.DescriptionLayerTranscript, true
	}

	if e.nameReader != nil {
		if name, _, ok := e.nameReader.LookupName(sess.AgentSessionID); ok {
			return smartTruncate(name, descriptionMaxBytes), session.DescriptionLayerAgentName, true
		}
	}

	return "", 0, false
}

// tryTranscript mines the first meaningful user prompt from the transcript
// associated with sess.AgentSessionID, applying the slash-command aware
// interpretation documented in the F4 spec.
func (e *CCDescriptionEnhancer) tryTranscript(sess *session.Session) (string, bool) {
	workDir := sess.CurrentWorkDir
	if workDir == "" {
		workDir = sess.WorkDir
	}
	entries, err := e.reader.ReadEntries(workDir, sess.AgentSessionID, "")
	if err != nil || len(entries) == 0 {
		return "", false
	}
	for _, ent := range entries {
		if ent.Type != "user" {
			continue
		}
		text := extractFirstText(ent.Blocks)
		if text == "" {
			continue
		}
		if cand, ok := interpretUserPrompt(text); ok {
			return cand, true
		}
	}
	return "", false
}

// extractFirstText returns the first non-empty "text" block within a user
// turn's blocks. Tool result blocks and other kinds are skipped so we do not
// mistake reply payloads for the user's own words.
func extractFirstText(blocks []transcript.Block) string {
	for _, b := range blocks {
		if b.Kind != "text" {
			continue
		}
		if s := strings.TrimSpace(b.Text); s != "" {
			return s
		}
	}
	return ""
}

// interpretUserPrompt turns the raw first-user-turn text into a description
// candidate following the F4 rules:
//
//   - Empty or trivially short input yields no candidate.
//   - Slash commands with no args (or with args shorter than minSlashArgLen)
//     are considered pending — the user is still composing the request.
//   - Slash commands with meaningful args use the args as the description.
//   - Plain text uses the message body directly.
//
// The returned string is smart-truncated so it fits inside the list view.
func interpretUserPrompt(text string) (string, bool) {
	text = strings.TrimSpace(text)
	// Length thresholds are measured in runes so multi-byte scripts (Japanese,
	// CJK, emoji) aren't unfairly rejected as "too short". A three-character
	// Japanese prompt weighs nine bytes and would fail a byte-length gate.
	if utf8.RuneCountInString(text) < minPromptLen {
		return "", false
	}
	if strings.HasPrefix(text, "/") {
		parts := strings.SplitN(text, " ", 2)
		if len(parts) < 2 {
			return "", false
		}
		args := strings.TrimSpace(parts[1])
		if utf8.RuneCountInString(args) < minSlashArgLen {
			return "", false
		}
		return smartTruncate(args, descriptionMaxBytes), true
	}
	return smartTruncate(text, descriptionMaxBytes), true
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
