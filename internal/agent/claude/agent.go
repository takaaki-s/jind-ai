// Package claude is the Claude Code adapter. It owns every CC-specific
// concern jind-ai used to inline into internal/session — hook language,
// hooks-settings.json generation, trust-dialog suppression, the shell
// command shape, and the transcript-derived description enhancer.
//
// The type-name Agent implements session.Agent (via the aliases exposed in
// internal/agent). Register instances via the internal/agent/register
// blank-import package so the daemon can Lookup("claude") them at start-up.
package claude

import (
	"sync"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/debug"
)

// claudeLog is shared across the whole adapter (agent / trust / hooks_settings)
// so debug output for CC-specific setup goes to a single logger instance.
var claudeLog = debug.NewLogger("daemon-debug.log")

// The Claude Code adapter caches a handful of pieces so multiple session
// starts don't repeatedly perform expensive or racy work:
//
//   - hooksOnce guards hooks-settings.json generation. The file describes how
//     Claude Code invokes `jin hook`; it must exist once per daemon process
//     and it is safe to reuse across every session (the content is
//     session-independent).
//   - enhancer is the Layer C description enhancer — it holds a transcript
//     reader whose only state is the ~/.claude directory path.
//   - statusSrc is stateless but held as a value so we don't allocate one per
//     hook event.
type Agent struct {
	hooksOnce sync.Once
	hooksPath string
	hooksErr  error

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

// StatusSource returns the CC hook interpreter.
func (a *Agent) StatusSource() agent.StatusSource { return a.statusSrc }

// Description returns the Layer C enhancer that mines the CC transcript for
// a better human-readable label.
func (a *Agent) Description() agent.DescriptionSource { return a.enhancer }

// Setup writes the process-wide hooks-settings.json (exactly once) and the
// per-workDir trust flag. Both failures are logged but do not abort the
// session start — the historical behaviour is "warn and continue", matching
// what Claude Code itself tolerates.
//
// SpawnCommand consults a.hooksPath to decide whether to pass --settings; if
// the hooks file could not be written the flag is simply omitted.
func (a *Agent) Setup(ctx agent.SetupContext) error {
	a.hooksOnce.Do(func() {
		a.hooksPath, a.hooksErr = EnsureHooksSettingsFile(ctx.StateDir, ctx.ExecPath)
		if a.hooksErr != nil {
			claudeLog("[HOOKS] Warning: failed to generate hooks settings: %v", a.hooksErr)
		}
	})
	if err := EnsureTrustState(ctx.WorkDir); err != nil {
		claudeLog("[TRUST] Warning: failed to set trust state: %v", err)
	}
	return nil
}

// HooksSettingsPath returns the cached path written by Setup; empty means
// Setup either hasn't run yet or the write failed.
func (a *Agent) HooksSettingsPath() string { return a.hooksPath }
