package task

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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
	now      func() time.Time
	newID    func() string
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
		store: store, sessions: sessions, tasks: make(map[string]Task),
		now: time.Now, newID: func() string { return uuid.New().String() },
	}
	for _, value := range values {
		m.tasks[value.ID] = value
	}
	return m, nil
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
