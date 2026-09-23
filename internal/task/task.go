// Package task owns durable user intent independently from the agent sessions
// that execute it. A task may outlive any individual session and may acquire
// more executions over time.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

const SchemaVersion = 6

const (
	MaxTitleLength           = 200
	MaxSourceKindLength      = 64
	MaxSourceRefLength       = 512
	MaxProviderLength        = 64
	MaxRepositoryLength      = 256
	MaxExternalIDLength      = 128
	MaxSourceURLLength       = 2048
	MaxSyncTokenLength       = 256
	MaxRequestedBaseLength   = 512
	MaxPromptSummaryLength   = 512
	MaxIdempotencyKeyLength  = 200
	MaxRepoLength            = 4096
	MaxRelativeWorkDirLength = 1024
	MaxAgentKindLength       = 64
	MaxModelLength           = 256
	MaxFleetLength           = 128
	MaxWorktreeBranchLength  = 512
	MaxRunMessageLength      = 2048
	MaxPromptBytes           = 64 * 1024
	MaxMutationBodyBytes     = 48 * 1024
	MaxMutationActorLength   = 128
	MaxMutationResultID      = 128
	MaxMutationMessageLength = 512
)

// Source identifies where the task request came from without copying provider
// content into jind-ai's state directory. Kind is deliberately open-ended so a
// newer provider remains readable by an older daemon.
type Source struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Repository string `json:"repository,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`
	SyncToken  string `json:"sync_token,omitempty"`
}

// Task is the persisted aggregate. Executions are append-only and Sequence is
// their authoritative order; session state is intentionally not persisted here.
type Task struct {
	SchemaVersion int         `json:"schema_version"`
	ID            string      `json:"id"`
	Title         string      `json:"title"`
	Source        Source      `json:"source"`
	RequestedBase string      `json:"requested_base,omitempty"`
	PromptSummary string      `json:"prompt_summary,omitempty"`
	Executions    []Execution `json:"executions"`
	Mutations     []Mutation  `json:"mutations"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

type MutationStatus string

const (
	MutationRunning   MutationStatus = "running"
	MutationSucceeded MutationStatus = "succeeded"
	MutationUnknown   MutationStatus = "unknown"
)

// Mutation is a bounded audit receipt for one explicit provider side effect.
// Request content is represented only by its irreversible digest and byte
// count; credentials and provider output never enter this record.
type Mutation struct {
	ID             string          `json:"id"`
	Sequence       uint64          `json:"sequence"`
	IdempotencyKey string          `json:"idempotency_key"`
	Kind           string          `json:"kind"`
	Target         MutationTarget  `json:"target"`
	Actor          string          `json:"actor"`
	Request        MutationRequest `json:"request"`
	Status         MutationStatus  `json:"status"`
	Result         MutationResult  `json:"result,omitzero"`
	Error          string          `json:"error,omitempty"`
	Guidance       string          `json:"guidance,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type MutationTarget struct {
	Provider   string `json:"provider"`
	Repository string `json:"repository"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url"`
}

type MutationRequest struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type MutationResult struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	URL      string `json:"url"`
	Actor    string `json:"actor"`
}

// Execution is one stable local-or-remote attempt. A local attempt references
// a local session; a remote attempt carries only opaque identities and a
// bounded summary. Neither form contains prompt body or transcript data.
type Execution struct {
	ID        string           `json:"id"`
	Sequence  uint64           `json:"sequence"`
	Backend   ExecutionBackend `json:"backend"`
	SessionID string           `json:"session_id,omitempty"`
	Remote    *RemoteLink      `json:"remote,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	Run       *Run             `json:"run,omitempty"`
}

type ExecutionBackend string

const (
	ExecutionBackendLocal  ExecutionBackend = "local"
	ExecutionBackendRemote ExecutionBackend = "remote"
)

type RemoteSyncState string

const (
	RemoteSyncDispatching RemoteSyncState = "dispatching"
	RemoteSyncBound       RemoteSyncState = "bound"
	RemoteSyncUnreachable RemoteSyncState = "unreachable"
	RemoteSyncBlocked     RemoteSyncState = "blocked"
)

// RemoteLink is the controller-owned durable binding to a remote execution.
// Every value is an opaque identity or bounded projection; paths, pane IDs,
// transcript content, and credentials never cross this boundary.
type RemoteLink struct {
	TargetID              string                  `json:"target_id"`
	TargetRevision        string                  `json:"target_revision"`
	RepositoryLabel       string                  `json:"repository_label"`
	RepositoryID          string                  `json:"repository_id"`
	RepositoryIdentity    string                  `json:"repository_identity,omitempty"`
	ServerInstanceID      string                  `json:"server_instance_id,omitempty"`
	ServerBootID          string                  `json:"server_boot_id,omitempty"`
	Capabilities          []string                `json:"capabilities,omitempty"`
	ControllerExecutionID string                  `json:"controller_execution_id"`
	RemoteTaskID          string                  `json:"remote_task_id,omitempty"`
	RemoteExecutionID     string                  `json:"remote_execution_id,omitempty"`
	SyncState             RemoteSyncState         `json:"sync_state"`
	Summary               *RemoteSummary          `json:"summary,omitempty"`
	Cancel                *RemoteOperationReceipt `json:"cancel,omitempty"`
	Cleanup               *RemoteOperationReceipt `json:"cleanup,omitempty"`
	Error                 string                  `json:"error,omitempty"`
	UpdatedAt             time.Time               `json:"updated_at"`
}

type RemoteOperationStatus string

const (
	RemoteOperationRunning   RemoteOperationStatus = "running"
	RemoteOperationSucceeded RemoteOperationStatus = "succeeded"
	RemoteOperationFailed    RemoteOperationStatus = "failed"
	RemoteOperationUnknown   RemoteOperationStatus = "unknown"
)

// RemoteOperationReceipt is the durable result of one explicit remote side
// effect. Running exists only in local journals; wire responses expose a
// terminal or unknown receipt.
type RemoteOperationReceipt struct {
	IdempotencyKey string                 `json:"idempotency_key"`
	Status         RemoteOperationStatus  `json:"status"`
	Removed        RemoteRemovedResources `json:"removed,omitzero"`
	Error          string                 `json:"error,omitempty"`
	Guidance       string                 `json:"guidance,omitempty"`
	UpdatedAt      time.Time              `json:"updated_at,omitempty"`
}

type RemoteRemovedResources struct {
	Session  bool `json:"session,omitempty"`
	Worktree bool `json:"worktree,omitempty"`
	Branch   bool `json:"branch,omitempty"`
}

func (r RemoteRemovedResources) IsZero() bool { return r == RemoteRemovedResources{} }

type RemoteSummary struct {
	Sequence   uint64                 `json:"sequence"`
	ObservedAt time.Time              `json:"observed_at"`
	Execution  RemoteExecutionSummary `json:"execution"`
	Session    *RemoteSessionSummary  `json:"session,omitempty"`
}

type RemoteExecutionSummary struct {
	Phase       ExecutionPhase `json:"phase"`
	FailedPhase ExecutionPhase `json:"failed_phase,omitempty"`
	Error       string         `json:"error,omitempty"`
	Guidance    string         `json:"guidance,omitempty"`
}

type RemoteSessionSummary struct {
	ID          string                  `json:"id"`
	Status      session.Status          `json:"status"`
	Attention   session.AttentionInfo   `json:"attention,omitzero"`
	ReviewFacts session.ReviewFacts     `json:"review_facts,omitzero"`
	CheckReport session.CheckReportInfo `json:"check_report,omitzero"`
}

// RemoteOrigin is persisted only by a target executing work for a controller.
// It binds retries before any worktree or session side effect occurs.
type RemoteOrigin struct {
	ControllerID          string `json:"controller_id"`
	ControllerExecutionID string `json:"controller_execution_id"`
	IdempotencyKey        string `json:"idempotency_key"`
}

// ExecutionPhase is the durable orchestration state of a prompt-backed run.
type ExecutionPhase string

const (
	ExecutionReserved     ExecutionPhase = "reserved"
	ExecutionProvisioning ExecutionPhase = "provisioning"
	ExecutionConfiguring  ExecutionPhase = "configuring"
	ExecutionStarting     ExecutionPhase = "starting"
	ExecutionWaiting      ExecutionPhase = "waiting"
	ExecutionSubmitting   ExecutionPhase = "submitting"
	ExecutionSubmitted    ExecutionPhase = "submitted"
	ExecutionFailed       ExecutionPhase = "failed"
	ExecutionInterrupted  ExecutionPhase = "interrupted"
)

// PromptMetadata is deliberately irreversible. The body crosses IPC only for
// the live submission attempt and is never written to a Task record.
type PromptMetadata struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// Run is the bounded journal for a prompt-backed execution. It contains enough
// identity to reconcile a retry, but no prompt body or captured environment.
type Run struct {
	IdempotencyKey    string                  `json:"idempotency_key"`
	Phase             ExecutionPhase          `json:"phase"`
	FailedPhase       ExecutionPhase          `json:"failed_phase,omitempty"`
	Repo              string                  `json:"repo"`
	RelativeWorkDir   string                  `json:"relative_work_dir,omitempty"`
	AgentKind         string                  `json:"agent_kind"`
	Model             string                  `json:"model,omitempty"`
	Fleet             string                  `json:"fleet,omitempty"`
	NoHook            bool                    `json:"no_hook,omitempty"`
	RequestedBase     string                  `json:"requested_base,omitempty"`
	WorktreeName      string                  `json:"worktree_name"`
	WorktreeBranch    string                  `json:"worktree_branch"`
	Prompt            PromptMetadata          `json:"prompt"`
	Error             string                  `json:"error,omitempty"`
	Guidance          string                  `json:"guidance,omitempty"`
	Warning           string                  `json:"warning,omitempty"`
	RemoteOrigin      *RemoteOrigin           `json:"remote_origin,omitempty"`
	RemoteCancel      *RemoteOperationReceipt `json:"remote_cancel,omitempty"`
	RemoteCleanup     *RemoteOperationReceipt `json:"remote_cleanup,omitempty"`
	SummarySequence   uint64                  `json:"summary_sequence,omitempty"`
	SummaryDigest     string                  `json:"summary_digest,omitempty"`
	SummaryObservedAt time.Time               `json:"summary_observed_at,omitempty"`
	UpdatedAt         time.Time               `json:"updated_at"`
}

type ReferenceState string

const (
	ReferencePresent ReferenceState = "present"
	ReferenceMissing ReferenceState = "missing"
)

// ExecutionInfo joins a durable execution with a live session snapshot. A
// deleted session becomes ReferenceMissing rather than invalidating the task.
type ExecutionInfo struct {
	Execution
	ReferenceState ReferenceState        `json:"reference_state"`
	SessionStatus  session.Status        `json:"session_status,omitempty"`
	Attention      session.AttentionInfo `json:"attention,omitzero"`
}

// LatestAttention makes the task's currently relevant completion receipt
// available without requiring callers to traverse executions themselves.
type LatestAttention struct {
	ExecutionID     string                `json:"execution_id"`
	Backend         ExecutionBackend      `json:"backend"`
	SessionID       string                `json:"session_id"`
	RemoteSessionID string                `json:"remote_session_id,omitempty"`
	ReferenceState  ReferenceState        `json:"reference_state"`
	SessionStatus   session.Status        `json:"session_status,omitempty"`
	Attention       session.AttentionInfo `json:"attention,omitzero"`
}

// Info is the read projection returned over IPC.
type Info struct {
	SchemaVersion   int              `json:"schema_version"`
	ID              string           `json:"id"`
	Title           string           `json:"title"`
	Source          Source           `json:"source"`
	RequestedBase   string           `json:"requested_base,omitempty"`
	PromptSummary   string           `json:"prompt_summary,omitempty"`
	Executions      []ExecutionInfo  `json:"executions"`
	Mutations       []Mutation       `json:"mutations"`
	LatestAttention *LatestAttention `json:"latest_attention,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

var mutationKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func MutationBodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func DefaultMutationKey(taskID, kind string, target MutationTarget, request MutationRequest) string {
	sum := sha256.Sum256([]byte(taskID + "\x00" + kind + "\x00" + target.Provider + "\x00" +
		target.Repository + "\x00" + target.ExternalID + "\x00" + request.SHA256))
	return "mut_" + hex.EncodeToString(sum[:16])
}

func ValidateMutation(m Mutation) error {
	if m.Kind != "issue_comment" {
		return fmt.Errorf("unsupported mutation kind %q", m.Kind)
	}
	if !mutationKeyPattern.MatchString(m.IdempotencyKey) {
		return fmt.Errorf("invalid mutation idempotency key")
	}
	if m.Target.Provider == "" || len(m.Target.Provider) > MaxProviderLength ||
		m.Target.Repository == "" || len(m.Target.Repository) > MaxRepositoryLength ||
		m.Target.ExternalID == "" || len(m.Target.ExternalID) > MaxExternalIDLength {
		return fmt.Errorf("invalid mutation target identity")
	}
	if err := validateMutationURL(m.Target.URL); err != nil {
		return err
	}
	if m.Actor == "" || len(m.Actor) > MaxMutationActorLength || strings.ContainsAny(m.Actor, "\r\n\x00") {
		return fmt.Errorf("invalid mutation actor")
	}
	if len(m.Request.SHA256) != sha256.Size*2 || m.Request.Bytes <= 0 || m.Request.Bytes > MaxMutationBodyBytes {
		return fmt.Errorf("invalid mutation request metadata")
	}
	if _, err := hex.DecodeString(m.Request.SHA256); err != nil {
		return fmt.Errorf("invalid mutation request digest")
	}
	if m.Status != MutationRunning && m.Status != MutationSucceeded && m.Status != MutationUnknown {
		return fmt.Errorf("invalid mutation status %q", m.Status)
	}
	if len(m.Error) > MaxMutationMessageLength || len(m.Guidance) > MaxMutationMessageLength {
		return fmt.Errorf("mutation diagnostic exceeds %d bytes", MaxMutationMessageLength)
	}
	if m.Status == MutationSucceeded {
		if m.Result.Provider != m.Target.Provider || m.Result.ID == "" || len(m.Result.ID) > MaxMutationResultID ||
			m.Result.Actor != m.Actor || len(m.Result.Actor) > MaxMutationActorLength {
			return fmt.Errorf("invalid mutation result identity")
		}
		if err := validateMutationURL(m.Result.URL); err != nil {
			return err
		}
	}
	return nil
}

func validateMutationURL(value string) error {
	if value == "" || len(value) > MaxSourceURLLength {
		return fmt.Errorf("invalid mutation URL")
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return fmt.Errorf("mutation URL must be https without credentials or query")
	}
	return nil
}

type CreateOptions struct {
	Title         string
	Source        Source
	RequestedBase string
	PromptSummary string
}

func ValidateCreateOptions(opts CreateOptions) error {
	if strings.TrimSpace(opts.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if len(opts.Title) > MaxTitleLength {
		return fmt.Errorf("title exceeds %d bytes", MaxTitleLength)
	}
	if len(opts.Source.Kind) > MaxSourceKindLength {
		return fmt.Errorf("source kind exceeds %d bytes", MaxSourceKindLength)
	}
	if len(opts.Source.Ref) > MaxSourceRefLength {
		return fmt.Errorf("source ref exceeds %d bytes", MaxSourceRefLength)
	}
	if len(opts.Source.Provider) > MaxProviderLength {
		return fmt.Errorf("source provider exceeds %d bytes", MaxProviderLength)
	}
	if len(opts.Source.Repository) > MaxRepositoryLength {
		return fmt.Errorf("source repository exceeds %d bytes", MaxRepositoryLength)
	}
	if len(opts.Source.ExternalID) > MaxExternalIDLength {
		return fmt.Errorf("source external id exceeds %d bytes", MaxExternalIDLength)
	}
	if len(opts.Source.URL) > MaxSourceURLLength {
		return fmt.Errorf("source URL exceeds %d bytes", MaxSourceURLLength)
	}
	if len(opts.Source.SyncToken) > MaxSyncTokenLength {
		return fmt.Errorf("source sync token exceeds %d bytes", MaxSyncTokenLength)
	}
	hasExternal := opts.Source.Provider != "" || opts.Source.Repository != "" || opts.Source.ExternalID != "" || opts.Source.URL != "" || opts.Source.SyncToken != ""
	if hasExternal && (opts.Source.Provider == "" || opts.Source.Repository == "" || opts.Source.ExternalID == "") {
		return fmt.Errorf("external source requires provider, repository, and external id")
	}
	if len(opts.RequestedBase) > MaxRequestedBaseLength {
		return fmt.Errorf("requested base exceeds %d bytes", MaxRequestedBaseLength)
	}
	if len(opts.PromptSummary) > MaxPromptSummaryLength {
		return fmt.Errorf("prompt summary exceeds %d bytes", MaxPromptSummaryLength)
	}
	return nil
}

func normalize(t *Task) {
	if t.SchemaVersion < SchemaVersion {
		t.SchemaVersion = SchemaVersion
	}
	if t.Source.Kind == "" {
		t.Source.Kind = "unknown"
	}
	if t.Executions == nil {
		t.Executions = []Execution{}
	}
	if t.Mutations == nil {
		t.Mutations = []Mutation{}
	}
	var next uint64 = 1
	for i := range t.Executions {
		if t.Executions[i].Backend == "" {
			t.Executions[i].Backend = ExecutionBackendLocal
		}
		if t.Executions[i].Sequence == 0 {
			t.Executions[i].Sequence = next
		}
		if t.Executions[i].Sequence >= next {
			next = t.Executions[i].Sequence + 1
		}
		run := t.Executions[i].Run
		if run != nil && t.Executions[i].Backend == ExecutionBackendLocal && runPhaseTransient(run.Phase) {
			run.FailedPhase = run.Phase
			run.Phase = ExecutionInterrupted
			run.Error = "daemon restarted while this execution was in progress"
			run.Guidance = "retry `jin task new` with the same idempotency key and input; jind-ai will reuse recorded identities"
		}
		if t.Executions[i].Remote != nil {
			normalizeRemoteOperation(t.Executions[i].Remote.Cancel)
			normalizeRemoteOperation(t.Executions[i].Remote.Cleanup)
		}
		if run != nil {
			normalizeRemoteOperation(run.RemoteCancel)
			normalizeRemoteOperation(run.RemoteCleanup)
		}
	}
	for i := range t.Mutations {
		if t.Mutations[i].Sequence == 0 {
			t.Mutations[i].Sequence = uint64(i + 1)
		}
		if t.Mutations[i].Status == MutationRunning {
			t.Mutations[i].Status = MutationUnknown
			t.Mutations[i].Error = "daemon restarted while the provider mutation was running"
			t.Mutations[i].Guidance = "retry with the same idempotency key to reconcile; jind-ai will not submit the mutation again unless success is proven"
		}
	}
}

func normalizeRemoteOperation(receipt *RemoteOperationReceipt) {
	if receipt != nil && receipt.Status == RemoteOperationRunning {
		receipt.Status = RemoteOperationUnknown
		receipt.Error = "daemon restarted while the remote operation was running"
		receipt.Guidance = "retry the same operation with the same idempotency key to reconcile"
	}
}

func runPhaseTransient(phase ExecutionPhase) bool {
	switch phase {
	case ExecutionReserved, ExecutionProvisioning, ExecutionConfiguring, ExecutionStarting, ExecutionWaiting, ExecutionSubmitting:
		return true
	default:
		return false
	}
}
