// Package codex is the OpenAI Codex CLI adapter. It owns every Codex-specific
// concern jind-ai used to inline into internal/session — hook injection via
// per-invocation -c overrides, the SessionStart write-back path Codex needs
// because it has no `--session-id` equivalent, the shell command shape, and
// the rollout-derived Layer C-transcript enhancer.
//
// The type-name Agent implements session.Agent (via the aliases exposed in
// internal/agent). Register instances via the internal/agent/register
// blank-import package so the daemon can Lookup("codex") them at start-up.
package codex

import (
	"os"
	"sync"

	"github.com/takaaki-s/jind-ai/internal/agent"
)

// Agent is the process-wide Codex adapter state. All fields are set once via
// Setup and read from every subsequent SpawnCommand call, so we protect the
// write side with a sync.Once and expose the read side without locking.
//
//   - execPath is os.Executable() captured from the first Setup and reused
//     by hook_args.go to build the `-c 'hooks.X=[...]'` payloads. It never
//     changes for the lifetime of the daemon.
//   - home is the value os.UserHomeDir() returned at Agent construction —
//     used by the DescriptionEnhancer's Locator when CODEX_HOME is unset.
//     Grabbed eagerly rather than at Setup because it, too, is invariant
//     for a running daemon and tests set CODEX_HOME directly.
//   - enhancer and statusSrc are cached instances so hot-path calls to
//     Description() / StatusSource() don't reallocate on every hook.
type Agent struct {
	setupOnce sync.Once
	execPath  string
	home      string
	enhancer  *DescriptionEnhancer
	statusSrc *HookStatusSource
}

// New returns a fully-wired Codex adapter. Home dir is resolved eagerly; if
// the lookup fails the enhancer falls back to a relative-path Locator (which
// will simply never find any rollout — TryGenerate then returns false and
// the session keeps whatever description Layer A/B provided).
func New() *Agent {
	home, _ := os.UserHomeDir()
	return &Agent{
		home:      home,
		enhancer:  NewDescriptionEnhancer(home),
		statusSrc: NewHookStatusSource(),
	}
}

// Kind is the identifier jind-ai persists in Session.AgentKind.
func (a *Agent) Kind() string { return "codex" }

// Setup captures os.Executable() so SpawnCommand can wire the `-c` hook
// payload back to `jin hook`. Unlike the Claude adapter, Setup writes no
// files: Codex hooks are injected per-invocation on the command line
// (02_design.md §3.3), so ~/.codex/hooks.json and config.toml both stay
// untouched.
func (a *Agent) Setup(ctx agent.SetupContext) error {
	a.setupOnce.Do(func() {
		a.execPath = ctx.ExecPath
	})
	return nil
}

// SpawnCommand delegates to the package-level builder with the captured
// execPath. When Setup has not run yet — an edge case the interface
// contract does not forbid — execPath is the zero value, and HookArgs
// gracefully falls back to a hook-less `codex` invocation.
func (a *Agent) SpawnCommand(opts agent.SpawnOptions) agent.SpawnPlan {
	return SpawnCommand(opts, a.execPath)
}

// StatusSource returns the cached hook-event interpreter.
func (a *Agent) StatusSource() agent.StatusSource { return a.statusSrc }

// Description returns the cached Layer C-transcript enhancer that pulls
// the first genuine user prompt out of the rollout JSONL.
func (a *Agent) Description() agent.DescriptionSource { return a.enhancer }
