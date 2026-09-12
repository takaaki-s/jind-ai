// Package task owns durable user intent independently from the agent sessions
// that execute it. A task may outlive any individual session and may acquire
// more executions over time.
package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

const SchemaVersion = 1

const (
	MaxTitleLength         = 200
	MaxSourceKindLength    = 64
	MaxSourceRefLength     = 512
	MaxRequestedBaseLength = 512
	MaxPromptSummaryLength = 512
)

// Source identifies where the task request came from without copying provider
// content into jind-ai's state directory. Kind is deliberately open-ended so a
// newer provider remains readable by an older daemon.
type Source struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
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
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// Execution is a stable link from one task attempt to one existing session.
// It contains no prompt or transcript data.
type Execution struct {
	ID        string    `json:"id"`
	Sequence  uint64    `json:"sequence"`
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
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
	ExecutionID    string                `json:"execution_id"`
	SessionID      string                `json:"session_id"`
	ReferenceState ReferenceState        `json:"reference_state"`
	SessionStatus  session.Status        `json:"session_status,omitempty"`
	Attention      session.AttentionInfo `json:"attention,omitzero"`
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
	LatestAttention *LatestAttention `json:"latest_attention,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
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
	if len(opts.RequestedBase) > MaxRequestedBaseLength {
		return fmt.Errorf("requested base exceeds %d bytes", MaxRequestedBaseLength)
	}
	if len(opts.PromptSummary) > MaxPromptSummaryLength {
		return fmt.Errorf("prompt summary exceeds %d bytes", MaxPromptSummaryLength)
	}
	return nil
}

func normalize(t *Task) {
	if t.SchemaVersion == 0 {
		t.SchemaVersion = SchemaVersion
	}
	if t.Source.Kind == "" {
		t.Source.Kind = "unknown"
	}
	if t.Executions == nil {
		t.Executions = []Execution{}
	}
	var next uint64 = 1
	for i := range t.Executions {
		if t.Executions[i].Sequence == 0 {
			t.Executions[i].Sequence = next
		}
		if t.Executions[i].Sequence >= next {
			next = t.Executions[i].Sequence + 1
		}
	}
}
