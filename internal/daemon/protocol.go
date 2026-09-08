package daemon

// ProtocolVersion is the wire-format version negotiated on every Client/Server
// exchange. It is deliberately separate from the build version: only an edit
// that changes the shape of a Request or Response — new/removed action, new
// required Data field, altered Data schema — bumps it.
//
// Both ends stamp outgoing messages with this constant and refuse a different
// value, so a mismatch fails with a message pointing at the fix (`jin daemon
// restart` after updating jin) instead of the "unexpected end of JSON input"
// symptoms that Data-schema drift produces.
//
// A brand-new endpoint needs no bump — old clients simply never call it — but a
// change to an existing endpoint's Data shape does.
//
// v2: NewResponse returns a StatusCreating reservation instead of a
// fully-provisioned session; Session/Info gained CreationWarning.
// v3: session.Info responses can include completion attention; add the
// idempotent attention-seen action.
// v4: session.Info responses can include an immutable review_base projection.
// v5: session.Info responses can include cached review_facts; add the bounded
// review-refresh action and ready-for-review attention state.
// v6: review_facts include a workspace fingerprint; session.Info can include a
// reported check_report; add the check-report action and checks-failed state.
const ProtocolVersion = 6
