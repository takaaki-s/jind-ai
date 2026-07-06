// Package register wires every known adapter into the agent registry via
// its init function. Import it for its side effect from the top of the CLI:
//
//	import _ "github.com/takaaki-s/jindaiko/internal/agent/register"
//
// Keeping the wiring here lets internal/agent stay agnostic of the concrete
// adapter packages, breaking the import cycle that would otherwise form
// (agent → claude → agent).
package register

import (
	"github.com/takaaki-s/jindaiko/internal/agent"
	"github.com/takaaki-s/jindaiko/internal/agent/claude"
)

func init() {
	agent.Register(claude.New())
}
