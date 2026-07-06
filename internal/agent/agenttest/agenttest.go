// Package agenttest exposes helpers for tests that need to drive the agent
// registry deterministically (register a stub, swap the current entry, wipe
// state between tests).
//
// It intentionally lives outside internal/agent so production code can never
// import Reset/Snapshot by mistake.
package agenttest

import (
	"github.com/takaaki-s/jindaiko/internal/agent"
	"github.com/takaaki-s/jindaiko/internal/session"
)

// StubAgent is a minimal Agent implementation for tests. Zero-value works;
// the callbacks are optional and default to sensible no-ops.
type StubAgent struct {
	KindStr     string
	SpawnFn     func(session.SpawnOptions) session.SpawnPlan
	InterpretFn func(session.StatusSignal) (session.StatusUpdate, bool)
	SetupFn     func(session.SetupContext) error
	DescribeFn  session.DescriptionEnhancer
}

func (s *StubAgent) Kind() string {
	if s.KindStr == "" {
		return "stub"
	}
	return s.KindStr
}

func (s *StubAgent) Setup(ctx session.SetupContext) error {
	if s.SetupFn != nil {
		return s.SetupFn(ctx)
	}
	return nil
}

func (s *StubAgent) SpawnCommand(opts session.SpawnOptions) session.SpawnPlan {
	if s.SpawnFn != nil {
		return s.SpawnFn(opts)
	}
	return session.SpawnPlan{Command: s.Kind()}
}

func (s *StubAgent) StatusSource() session.StatusSource { return statusSourceFn(s.InterpretFn) }

func (s *StubAgent) Description() session.DescriptionEnhancer { return s.DescribeFn }

type statusSourceFn func(session.StatusSignal) (session.StatusUpdate, bool)

func (f statusSourceFn) Interpret(sig session.StatusSignal) (session.StatusUpdate, bool) {
	if f == nil {
		return session.StatusUpdate{}, false
	}
	return f(sig)
}

// Reset wipes the registry. Call from t.Cleanup so tests never leak state
// into each other.
func Reset() {
	agent.ResetRegistryForTest()
}

// Register is a convenience wrapper around agent.Register that returns the
// argument to allow chaining in table tests.
func Register(a session.Agent) session.Agent {
	agent.Register(a)
	return a
}
