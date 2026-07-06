// Package agent hosts the Agent interface and the process-global registry
// that maps a kind string (e.g. "claude") to a concrete adapter.
//
// Adapters are registered via a blank-import package (internal/agent/register)
// so this file never has to depend on any specific adapter package — the
// dependency graph is:
//
//	internal/session  ──►  (owns Agent interface and supporting types)
//	                       ▲
//	                       │  type-alias re-export
//	                       │
//	internal/agent    ◄──  internal/agent/register  ──►  internal/agent/<kind>/
//
// The Manager only ever talks to a session.AgentResolver, so it stays free
// of any import into this package.
package agent

import "github.com/takaaki-s/jindaiko/internal/session"

// The type aliases below re-export the interface + supporting types the
// session package owns. Adapter implementations use these local names so
// their signatures read as "agent.SpawnPlan" etc., without pulling every
// call site to also import internal/session.

// Agent is the interface satisfied by every adapter.
type Agent = session.Agent

// SpawnOptions is the input to Agent.SpawnCommand.
type SpawnOptions = session.SpawnOptions

// SpawnPlan is the output of Agent.SpawnCommand.
type SpawnPlan = session.SpawnPlan

// StatusSignal is a raw event handed to Agent.StatusSource().Interpret.
type StatusSignal = session.StatusSignal

// StatusUpdate is the adapter's verdict on a StatusSignal.
type StatusUpdate = session.StatusUpdate

// SetupContext carries the paths Agent.Setup needs.
type SetupContext = session.SetupContext

// StatusSource interprets raw StatusSignals.
type StatusSource = session.StatusSource

// DescriptionSource is the Layer C description enhancer surface.
type DescriptionSource = session.DescriptionEnhancer

// NotifyKind categorises the desktop notification an adapter requests.
type NotifyKind = session.NotifyKind

// Notification kind constants — re-exported for adapter convenience.
const (
	NotifyNone         = session.NotifyNone
	NotifyTaskComplete = session.NotifyTaskComplete
	NotifyError        = session.NotifyError
	NotifyPermission   = session.NotifyPermission
)
