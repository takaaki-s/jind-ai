package daemon

// ProtocolVersion is the wire-format version negotiated on every Client/Server
// exchange. It is deliberately separate from the build version (semver): a
// docs-only or refactor patch keeps this constant unchanged, and only edits
// that change the shape of a Request or Response — new/removed action, new
// required Data field, altered Data schema — should bump it.
//
// Client.send stamps outgoing requests with this constant and refuses any
// response that carries a different value. Server.handleConnection does the
// same for incoming requests. When the two ends disagree, the connection
// fails with a message pointing at the fix (`jin daemon restart` after
// updating jin), instead of the endpoint-specific "unexpected end of JSON
// input"-style symptoms produced when only Data schemas drift.
//
// Bumping guidance: increment this ONLY when a change would cause a
// same-version-mismatch scenario to surface today's "empty Data" bug or any
// analogous silent parse error. A brand-new endpoint that never existed
// before does not need a bump — old clients simply never call it — but a
// change to an existing endpoint's Data shape does.
const ProtocolVersion = 1
