package claude

import "github.com/takaaki-s/jindaiko/internal/transcript"

// NewTranscriptReader is a wafer-thin wrapper over transcript.NewReader.
// It exists so callers can route every Claude Code transcript access through
// this package (mirroring the "all CC-specific code lives under
// internal/agent/claude/" invariant) without a physical move of
// internal/transcript in this task.
//
// A follow-up task will migrate the real reader into this package; keeping
// the surface here now means that move stays diff-local to the daemon /
// enhancer call sites.
func NewTranscriptReader() *transcript.Reader {
	return transcript.NewReader()
}
