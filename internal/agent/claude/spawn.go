package claude

import (
	"fmt"

	"github.com/takaaki-s/jindaiko/internal/agent"
)

// SpawnCommand builds the `claude ...` command line the daemon splices into
// its fixed shell wrapper. The wrapper handles cwd + JIN_SESSION_ID + env -u
// TMUX; we only own the agent-specific pieces:
//
//   - `--settings <path>` when Setup successfully wrote the hooks file.
//     Omitted otherwise so the CLI still starts (with default settings) and
//     the user gets a working session, just without status hooks.
//   - `--session-id <uuid>` on the very first spawn, or `--resume <uuid>`
//     when the session has already been started at least once. Falling
//     through both branches (empty AgentSessionID) yields a plain `claude`
//     invocation, which is the intended fallback for adapters that ever get
//     invoked without a pre-minted id.
//
// UnsetEnv includes CLAUDECODE because Claude Code sets it when it runs jin
// via a hook, and we must strip it before spawning a *new* CC to avoid the
// child thinking it's already inside a CC session.
func (a *Agent) SpawnCommand(opts agent.SpawnOptions) agent.SpawnPlan {
	cmd := "claude"
	if a.hooksPath != "" {
		cmd = fmt.Sprintf("claude --settings %s", a.hooksPath)
	}
	if opts.AgentSessionID != "" {
		if opts.AgentSessionStarted {
			cmd += fmt.Sprintf(" --resume %s", opts.AgentSessionID)
		} else {
			cmd += fmt.Sprintf(" --session-id %s", opts.AgentSessionID)
		}
	}
	return agent.SpawnPlan{
		Command: cmd,
		// Every Claude Code var that leaks in from a CC-parent environment
		// gets cleared here, so the spawned CC starts as a top-level
		// session with a fresh transcript. Missing any of these is not
		// just cosmetic: with CLAUDE_CODE_CHILD_SESSION=1, CC 2.x runs in
		// "child agent" mode and does not persist a .jsonl transcript,
		// which silently breaks the Layer C description enhancer (it
		// looks for that file). CLAUDECODE guards against nested tmux
		// self-detection; the CLAUDE_CODE_* group guards against session
		// inheritance from whatever process launched jindaiko's daemon or
		// tmux server.
		UnsetEnv: []string{
			"CLAUDECODE",
			"CLAUDE_CODE_CHILD_SESSION",
			"CLAUDE_CODE_SESSION_ID",
			"CLAUDE_CODE_ENTRYPOINT",
		},
	}
}
