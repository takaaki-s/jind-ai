package task

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// SessionLookup is the narrow boundary task projections need from session.
type SessionLookup interface {
	GetInfo(string) (session.Info, bool)
}

type Manager struct {
	mu       sync.RWMutex
	store    *Store
	sessions SessionLookup
	tasks    map[string]Task
	runs     map[string]runLocation
	now      func() time.Time
	newID    func() string
}

type runLocation struct {
	taskID      string
	executionID string
}

// RunOptions is the immutable identity and bounded metadata for one
// prompt-backed execution reservation.
type RunOptions struct {
	IdempotencyKey  string
	Title           string
	Repo            string
	RelativeWorkDir string
	AgentKind       string
	Model           string
	Fleet           string
	NoHook          bool
	RequestedBase   string
	BranchPrefix    string
	PromptSHA256    string
	PromptBytes     int
}

// RunReservation reports whether ReserveRun created the aggregate or found an
// identical prior request.
type RunReservation struct {
	Task      Info
	Execution ExecutionInfo
	Created   bool
}

func NewManager(dir string, sessions SessionLookup) (*Manager, error) {
	if sessions == nil {
		return nil, fmt.Errorf("session lookup is required")
	}
	store := NewStore(dir)
	values, err := store.LoadAll()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		store: store, sessions: sessions, tasks: make(map[string]Task), runs: make(map[string]runLocation),
		now: time.Now, newID: func() string { return uuid.New().String() },
	}
	for _, value := range values {
		m.tasks[value.ID] = value
		for _, execution := range value.Executions {
			if execution.Run == nil || execution.Run.IdempotencyKey == "" {
				continue
			}
			if _, exists := m.runs[execution.Run.IdempotencyKey]; exists {
				return nil, fmt.Errorf("duplicate task idempotency key %q", execution.Run.IdempotencyKey)
			}
			m.runs[execution.Run.IdempotencyKey] = runLocation{taskID: value.ID, executionID: execution.ID}
		}
	}
	return m, nil
}

// PromptDigest returns the lowercase SHA-256 digest stored in run metadata.
func PromptDigest(prompt string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
}

func validateRunOptions(opts RunOptions) error {
	if err := ValidateCreateOptions(CreateOptions{Title: opts.Title, Source: Source{Kind: "prompt"}, RequestedBase: opts.RequestedBase}); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(opts.IdempotencyKey) == "":
		return fmt.Errorf("idempotency key is required")
	case len(opts.IdempotencyKey) > MaxIdempotencyKeyLength:
		return fmt.Errorf("idempotency key exceeds %d bytes", MaxIdempotencyKeyLength)
	case strings.TrimSpace(opts.Repo) == "":
		return fmt.Errorf("repo is required")
	case len(opts.Repo) > MaxRepoLength:
		return fmt.Errorf("repo exceeds %d bytes", MaxRepoLength)
	case len(opts.RelativeWorkDir) > MaxRelativeWorkDirLength:
		return fmt.Errorf("relative workdir exceeds %d bytes", MaxRelativeWorkDirLength)
	case strings.TrimSpace(opts.AgentKind) == "":
		return fmt.Errorf("agent kind is required")
	case len(opts.AgentKind) > MaxAgentKindLength:
		return fmt.Errorf("agent kind exceeds %d bytes", MaxAgentKindLength)
	case len(opts.Model) > MaxModelLength:
		return fmt.Errorf("model exceeds %d bytes", MaxModelLength)
	case len(opts.Fleet) > MaxFleetLength:
		return fmt.Errorf("fleet exceeds %d bytes", MaxFleetLength)
	case len(opts.BranchPrefix)+8 > MaxWorktreeBranchLength:
		return fmt.Errorf("worktree branch exceeds %d bytes", MaxWorktreeBranchLength)
	case len(opts.PromptSHA256) != sha256.Size*2:
		return fmt.Errorf("prompt SHA-256 must be %d hexadecimal characters", sha256.Size*2)
	case opts.PromptBytes <= 0:
		return fmt.Errorf("prompt is required")
	case opts.PromptBytes > MaxPromptBytes:
		return fmt.Errorf("prompt exceeds %d bytes", MaxPromptBytes)
	}
	for _, r := range opts.PromptSHA256 {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return fmt.Errorf("prompt SHA-256 must be lowercase hexadecimal")
		}
	}
	return nil
}

// ReserveRun atomically creates a Task and its first Execution, or returns the
// previous reservation for an identical idempotency key. No session operation
// happens here, so Task identity always lands first.
func (m *Manager) ReserveRun(opts RunOptions) (RunReservation, error) {
	if err := validateRunOptions(opts); err != nil {
		return RunReservation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if location, ok := m.runs[opts.IdempotencyKey]; ok {
		value := m.tasks[location.taskID]
		execution, ok := executionByID(value, location.executionID)
		if !ok || execution.Run == nil {
			return RunReservation{}, fmt.Errorf("task run index is inconsistent for key %q", opts.IdempotencyKey)
		}
		if !sameRun(value, execution, opts) {
			return RunReservation{}, fmt.Errorf("idempotency key %q belongs to a different task request", opts.IdempotencyKey)
		}
		info := m.project(value)
		return RunReservation{Task: info, Execution: executionInfoByID(info, execution.ID), Created: false}, nil
	}

	now := m.now()
	taskID, executionID, sessionID := m.newID(), m.newID(), m.newID()
	identity := session.DefaultWorktreeIdentity(sessionID, opts.BranchPrefix)
	execution := Execution{
		ID: executionID, Sequence: 1, SessionID: sessionID, CreatedAt: now,
		Run: &Run{
			IdempotencyKey: opts.IdempotencyKey, Phase: ExecutionReserved,
			Repo: opts.Repo, RelativeWorkDir: opts.RelativeWorkDir,
			AgentKind: opts.AgentKind, Model: opts.Model, Fleet: opts.Fleet, NoHook: opts.NoHook,
			RequestedBase: opts.RequestedBase,
			WorktreeName:  identity.Name, WorktreeBranch: identity.Branch,
			Prompt: PromptMetadata{SHA256: opts.PromptSHA256, Bytes: opts.PromptBytes}, UpdatedAt: now,
		},
	}
	value := Task{
		SchemaVersion: SchemaVersion, ID: taskID, Title: strings.TrimSpace(opts.Title),
		Source: Source{Kind: "prompt", Ref: "sha256:" + opts.PromptSHA256}, RequestedBase: opts.RequestedBase,
		Executions: []Execution{execution}, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.store.Save(value); err != nil {
		return RunReservation{}, err
	}
	m.tasks[taskID] = value
	m.runs[opts.IdempotencyKey] = runLocation{taskID: taskID, executionID: executionID}
	info := m.project(value)
	return RunReservation{Task: info, Execution: executionInfoByID(info, executionID), Created: true}, nil
}

func sameRun(value Task, execution Execution, opts RunOptions) bool {
	run := execution.Run
	return value.Title == strings.TrimSpace(opts.Title) && run.IdempotencyKey == opts.IdempotencyKey &&
		run.Repo == opts.Repo && run.RelativeWorkDir == opts.RelativeWorkDir && run.AgentKind == opts.AgentKind &&
		run.Model == opts.Model && run.Fleet == opts.Fleet && run.NoHook == opts.NoHook &&
		run.RequestedBase == opts.RequestedBase && run.Prompt.SHA256 == opts.PromptSHA256 && run.Prompt.Bytes == opts.PromptBytes
}

func executionByID(value Task, id string) (Execution, bool) {
	for _, execution := range value.Executions {
		if execution.ID == id {
			return execution, true
		}
	}
	return Execution{}, false
}

func executionInfoByID(info Info, id string) ExecutionInfo {
	for _, execution := range info.Executions {
		if execution.ID == id {
			return execution
		}
	}
	return ExecutionInfo{}
}

// SetRunPhase atomically advances or records failure for one run.
func (m *Manager) SetRunPhase(taskID, executionID string, phase, failedPhase ExecutionPhase, message, guidance, warning string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tasks[taskID]
	if !ok {
		return Info{}, fmt.Errorf("task not found: %s", taskID)
	}
	updated := current
	updated.Executions = append([]Execution(nil), current.Executions...)
	found := false
	now := m.now()
	for i := range updated.Executions {
		if updated.Executions[i].ID != executionID {
			continue
		}
		if updated.Executions[i].Run == nil {
			return Info{}, fmt.Errorf("execution %s is not an orchestrated run", executionID)
		}
		run := *updated.Executions[i].Run
		run.Phase, run.FailedPhase = phase, failedPhase
		run.Error = boundedRunString(message)
		run.Guidance = boundedRunString(guidance)
		if warning != "" {
			run.Warning = boundedRunString(warning)
		}
		run.UpdatedAt = now
		updated.Executions[i].Run = &run
		found = true
		break
	}
	if !found {
		return Info{}, fmt.Errorf("execution not found: %s", executionID)
	}
	updated.UpdatedAt = now
	if err := m.store.Save(updated); err != nil {
		return Info{}, err
	}
	m.tasks[taskID] = updated
	return m.project(updated), nil
}

func boundedRunString(value string) string {
	if len(value) <= MaxRunMessageLength {
		return value
	}
	value = value[:MaxRunMessageLength]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (m *Manager) Create(opts CreateOptions) (Info, error) {
	if err := ValidateCreateOptions(opts); err != nil {
		return Info{}, err
	}
	now := m.now()
	kind := opts.Source.Kind
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "manual"
	}
	value := Task{
		SchemaVersion: SchemaVersion, ID: m.newID(), Title: strings.TrimSpace(opts.Title),
		Source: Source{Kind: kind, Ref: opts.Source.Ref}, RequestedBase: opts.RequestedBase,
		PromptSummary: opts.PromptSummary, Executions: []Execution{}, CreatedAt: now, UpdatedAt: now,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.Save(value); err != nil {
		return Info{}, err
	}
	m.tasks[value.ID] = value
	return m.project(value), nil
}

func (m *Manager) AppendExecution(taskID, sessionID string) (Info, error) {
	if sessionID == "" {
		return Info{}, fmt.Errorf("session id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tasks[taskID]
	if !ok {
		return Info{}, fmt.Errorf("task not found: %s", taskID)
	}
	if _, ok := m.sessions.GetInfo(sessionID); !ok {
		return Info{}, fmt.Errorf("session not found: %s", sessionID)
	}
	updated := current
	updated.Executions = append([]Execution(nil), current.Executions...)
	var sequence uint64 = 1
	for _, existing := range updated.Executions {
		if existing.Sequence >= sequence {
			sequence = existing.Sequence + 1
		}
	}
	now := m.now()
	updated.Executions = append(updated.Executions, Execution{
		ID: m.newID(), Sequence: sequence,
		SessionID: sessionID, CreatedAt: now,
	})
	updated.UpdatedAt = now
	if err := m.store.Save(updated); err != nil {
		return Info{}, err
	}
	m.tasks[taskID] = updated
	return m.project(updated), nil
}

func (m *Manager) Get(id string) (Info, bool) {
	m.mu.RLock()
	value, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok {
		return Info{}, false
	}
	return m.project(value), true
}

func (m *Manager) List() []Info {
	m.mu.RLock()
	values := make([]Task, 0, len(m.tasks))
	for _, value := range m.tasks {
		values = append(values, value)
	}
	m.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].CreatedAt.Before(values[j].CreatedAt)
	})
	infos := make([]Info, 0, len(values))
	for _, value := range values {
		infos = append(infos, m.project(value))
	}
	return infos
}

func (m *Manager) project(value Task) Info {
	info := Info{
		SchemaVersion: value.SchemaVersion, ID: value.ID, Title: value.Title, Source: value.Source,
		RequestedBase: value.RequestedBase, PromptSummary: value.PromptSummary,
		Executions: make([]ExecutionInfo, 0, len(value.Executions)),
		CreatedAt:  value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	for _, execution := range value.Executions {
		// Info is a value projection. Clone the optional journal too, otherwise
		// a caller holding ExecutionInfo could mutate Manager state outside mu.
		if execution.Run != nil {
			run := *execution.Run
			execution.Run = &run
		}
		projected := ExecutionInfo{Execution: execution, ReferenceState: ReferenceMissing}
		if sess, ok := m.sessions.GetInfo(execution.SessionID); ok {
			projected.ReferenceState = ReferencePresent
			projected.SessionStatus = sess.Status
			projected.Attention = sess.Attention
		}
		info.Executions = append(info.Executions, projected)
	}
	if len(info.Executions) > 0 {
		latest := info.Executions[len(info.Executions)-1]
		info.LatestAttention = &LatestAttention{
			ExecutionID: latest.ID, SessionID: latest.SessionID,
			ReferenceState: latest.ReferenceState, SessionStatus: latest.SessionStatus,
			Attention: latest.Attention,
		}
	}
	return info
}
