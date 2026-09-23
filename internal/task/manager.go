package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
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
	mu         sync.RWMutex
	store      *Store
	sessions   SessionLookup
	tasks      map[string]Task
	runs       map[string]runLocation
	remoteIDs  map[string]runLocation
	remoteKeys map[string]runLocation
	sources    map[string]string
	mutations  map[string]mutationLocation
	now        func() time.Time
	newID      func() string
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
	Source          Source
	RemoteOrigin    *RemoteOrigin
}

// RemoteRunOptions reserves the controller side of a remote execution before
// any network I/O. It deliberately contains labels and digests, never a remote
// filesystem path.
type RemoteRunOptions struct {
	IdempotencyKey  string
	Title           string
	TargetID        string
	TargetRevision  string
	RepositoryLabel string
	RepositoryID    string
	RelativeWorkDir string
	AgentKind       string
	Model           string
	Fleet           string
	NoHook          bool
	RequestedBase   string
	PromptSHA256    string
	PromptBytes     int
	Source          Source
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
		remoteIDs: make(map[string]runLocation), remoteKeys: make(map[string]runLocation),
		sources: make(map[string]string), mutations: make(map[string]mutationLocation),
		now: time.Now, newID: func() string { return uuid.New().String() },
	}
	for _, value := range values {
		m.tasks[value.ID] = value
		if key := externalSourceKey(value.Source); key != "" {
			if prior, exists := m.sources[key]; exists {
				return nil, fmt.Errorf("duplicate external source identity in tasks %s and %s", prior, value.ID)
			}
			m.sources[key] = value.ID
		}
		for _, execution := range value.Executions {
			if execution.Run == nil || execution.Run.IdempotencyKey == "" {
				continue
			}
			if _, exists := m.runs[execution.Run.IdempotencyKey]; exists {
				return nil, fmt.Errorf("duplicate task idempotency key %q", execution.Run.IdempotencyKey)
			}
			m.runs[execution.Run.IdempotencyKey] = runLocation{taskID: value.ID, executionID: execution.ID}
			if origin := execution.Run.RemoteOrigin; origin != nil {
				location := runLocation{taskID: value.ID, executionID: execution.ID}
				if err := addRemoteOriginIndex(m.remoteIDs, remoteOriginExecutionKey(origin), location, "controller execution"); err != nil {
					return nil, err
				}
				if err := addRemoteOriginIndex(m.remoteKeys, remoteOriginIdempotencyKey(origin), location, "remote idempotency"); err != nil {
					return nil, err
				}
			}
		}
		for _, mutation := range value.Mutations {
			if mutation.IdempotencyKey == "" {
				continue
			}
			if _, exists := m.mutations[mutation.IdempotencyKey]; exists {
				return nil, fmt.Errorf("duplicate mutation idempotency key %q", mutation.IdempotencyKey)
			}
			m.mutations[mutation.IdempotencyKey] = mutationLocation{taskID: value.ID, mutationID: mutation.ID}
		}
	}
	return m, nil
}

// PromptDigest returns the lowercase SHA-256 digest stored in run metadata.
func PromptDigest(prompt string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
}

func validateRunOptions(opts RunOptions) error {
	if err := ValidateCreateOptions(CreateOptions{Title: opts.Title, Source: opts.Source, RequestedBase: opts.RequestedBase}); err != nil {
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
	if opts.RemoteOrigin != nil {
		if err := validateRemoteOrigin(*opts.RemoteOrigin); err != nil {
			return err
		}
	}
	return nil
}

func validateRemoteOrigin(origin RemoteOrigin) error {
	for label, value := range map[string]string{
		"controller id": origin.ControllerID, "controller execution id": origin.ControllerExecutionID,
		"remote idempotency key": origin.IdempotencyKey,
	} {
		if strings.TrimSpace(value) == "" || len(value) > MaxIdempotencyKeyLength || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("invalid %s", label)
		}
	}
	return nil
}

func remoteOriginExecutionKey(origin *RemoteOrigin) string {
	return origin.ControllerID + "\x00" + origin.ControllerExecutionID
}

func remoteOriginIdempotencyKey(origin *RemoteOrigin) string {
	return origin.ControllerID + "\x00" + origin.IdempotencyKey
}

func addRemoteOriginIndex(index map[string]runLocation, key string, location runLocation, label string) error {
	if prior, exists := index[key]; exists && prior != location {
		return fmt.Errorf("duplicate %s binding", label)
	}
	index[key] = location
	return nil
}

// ReserveRun atomically creates a Task and its first Execution, or returns the
// previous reservation for an identical idempotency key. No session operation
// happens here, so Task identity always lands first.
func (m *Manager) ReserveRun(opts RunOptions) (RunReservation, error) {
	if opts.Source.Kind == "" {
		opts.Source = Source{Kind: "prompt", Ref: "sha256:" + opts.PromptSHA256}
	}
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
	if key := externalSourceKey(opts.Source); key != "" {
		if taskID, exists := m.sources[key]; exists {
			return RunReservation{}, fmt.Errorf("external source already belongs to task %s", taskID)
		}
	}
	if origin := opts.RemoteOrigin; origin != nil {
		if prior, exists := m.remoteIDs[remoteOriginExecutionKey(origin)]; exists {
			return RunReservation{}, fmt.Errorf("controller execution identity already belongs to task %s", prior.taskID)
		}
		if prior, exists := m.remoteKeys[remoteOriginIdempotencyKey(origin)]; exists {
			return RunReservation{}, fmt.Errorf("remote idempotency key already belongs to task %s", prior.taskID)
		}
	}

	now := m.now()
	taskID, executionID, sessionID := m.newID(), m.newID(), m.newID()
	identity := session.DefaultWorktreeIdentity(sessionID, opts.BranchPrefix)
	execution := Execution{
		ID: executionID, Sequence: 1, Backend: ExecutionBackendLocal, SessionID: sessionID, CreatedAt: now,
		Run: &Run{
			IdempotencyKey: opts.IdempotencyKey, Phase: ExecutionReserved,
			Repo: opts.Repo, RelativeWorkDir: opts.RelativeWorkDir,
			AgentKind: opts.AgentKind, Model: opts.Model, Fleet: opts.Fleet, NoHook: opts.NoHook,
			RequestedBase: opts.RequestedBase,
			WorktreeName:  identity.Name, WorktreeBranch: identity.Branch,
			Prompt: PromptMetadata{SHA256: opts.PromptSHA256, Bytes: opts.PromptBytes}, UpdatedAt: now,
			RemoteOrigin: cloneRemoteOrigin(opts.RemoteOrigin),
		},
	}
	value := Task{
		SchemaVersion: SchemaVersion, ID: taskID, Title: strings.TrimSpace(opts.Title),
		Source: opts.Source, RequestedBase: opts.RequestedBase,
		Executions: []Execution{execution}, Mutations: []Mutation{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.store.Save(value); err != nil {
		return RunReservation{}, err
	}
	m.tasks[taskID] = value
	m.runs[opts.IdempotencyKey] = runLocation{taskID: taskID, executionID: executionID}
	if origin := execution.Run.RemoteOrigin; origin != nil {
		location := runLocation{taskID: taskID, executionID: executionID}
		m.remoteIDs[remoteOriginExecutionKey(origin)] = location
		m.remoteKeys[remoteOriginIdempotencyKey(origin)] = location
	}
	if key := externalSourceKey(value.Source); key != "" {
		m.sources[key] = taskID
	}
	info := m.project(value)
	return RunReservation{Task: info, Execution: executionInfoByID(info, executionID), Created: true}, nil
}

func validateRemoteRunOptions(opts RemoteRunOptions) error {
	if opts.Source.Kind == "" {
		opts.Source = Source{Kind: "prompt", Ref: "sha256:" + opts.PromptSHA256}
	}
	if err := ValidateCreateOptions(CreateOptions{Title: opts.Title, Source: opts.Source, RequestedBase: opts.RequestedBase}); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(opts.IdempotencyKey) == "" || len(opts.IdempotencyKey) > MaxIdempotencyKeyLength:
		return fmt.Errorf("invalid idempotency key")
	case strings.TrimSpace(opts.TargetID) == "" || len(opts.TargetID) > 128:
		return fmt.Errorf("invalid remote target id")
	case strings.TrimSpace(opts.TargetRevision) == "" || len(opts.TargetRevision) > 128:
		return fmt.Errorf("invalid remote target revision")
	case strings.TrimSpace(opts.RepositoryLabel) == "" || len(opts.RepositoryLabel) > MaxRepositoryLength:
		return fmt.Errorf("invalid remote repository label")
	case strings.TrimSpace(opts.RepositoryID) == "" || len(opts.RepositoryID) > MaxRepositoryLength:
		return fmt.Errorf("invalid remote repository id")
	case len(opts.RelativeWorkDir) > MaxRelativeWorkDirLength:
		return fmt.Errorf("relative workdir exceeds %d bytes", MaxRelativeWorkDirLength)
	case len(opts.AgentKind) > MaxAgentKindLength:
		return fmt.Errorf("invalid agent kind")
	case len(opts.Model) > MaxModelLength:
		return fmt.Errorf("model exceeds %d bytes", MaxModelLength)
	case len(opts.Fleet) > MaxFleetLength:
		return fmt.Errorf("fleet exceeds %d bytes", MaxFleetLength)
	case len(opts.PromptSHA256) != sha256.Size*2:
		return fmt.Errorf("prompt SHA-256 must be %d hexadecimal characters", sha256.Size*2)
	case opts.PromptBytes <= 0 || opts.PromptBytes > MaxPromptBytes:
		return fmt.Errorf("invalid prompt byte count")
	}
	for _, r := range opts.PromptSHA256 {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return fmt.Errorf("prompt SHA-256 must be lowercase hexadecimal")
		}
	}
	return nil
}

// ReserveRemoteRun creates the controller Task/Execution before opening SSH.
// An identical retry returns the same controller execution identity.
func (m *Manager) ReserveRemoteRun(opts RemoteRunOptions) (RunReservation, error) {
	if opts.Source.Kind == "" {
		opts.Source = Source{Kind: "prompt", Ref: "sha256:" + opts.PromptSHA256}
	}
	if err := validateRemoteRunOptions(opts); err != nil {
		return RunReservation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if location, ok := m.runs[opts.IdempotencyKey]; ok {
		value := m.tasks[location.taskID]
		execution, ok := executionByID(value, location.executionID)
		if !ok || !sameRemoteRun(value, execution, opts) {
			return RunReservation{}, fmt.Errorf("idempotency key %q belongs to a different task request", opts.IdempotencyKey)
		}
		info := m.project(value)
		return RunReservation{Task: info, Execution: executionInfoByID(info, execution.ID), Created: false}, nil
	}
	if key := externalSourceKey(opts.Source); key != "" {
		if taskID, exists := m.sources[key]; exists {
			return RunReservation{}, fmt.Errorf("external source already belongs to task %s", taskID)
		}
	}
	now := m.now()
	taskID, executionID := m.newID(), m.newID()
	execution := Execution{
		ID: executionID, Sequence: 1, Backend: ExecutionBackendRemote, CreatedAt: now,
		Remote: &RemoteLink{
			TargetID: opts.TargetID, TargetRevision: opts.TargetRevision, RepositoryLabel: opts.RepositoryLabel,
			RepositoryID:          opts.RepositoryID,
			ControllerExecutionID: executionID, SyncState: RemoteSyncDispatching, UpdatedAt: now,
		},
		Run: &Run{
			IdempotencyKey: opts.IdempotencyKey, Phase: ExecutionReserved,
			RelativeWorkDir: opts.RelativeWorkDir, AgentKind: opts.AgentKind, Model: opts.Model,
			Fleet: opts.Fleet, NoHook: opts.NoHook, RequestedBase: opts.RequestedBase,
			Prompt: PromptMetadata{SHA256: opts.PromptSHA256, Bytes: opts.PromptBytes}, UpdatedAt: now,
		},
	}
	value := Task{
		SchemaVersion: SchemaVersion, ID: taskID, Title: strings.TrimSpace(opts.Title), Source: opts.Source,
		RequestedBase: opts.RequestedBase, Executions: []Execution{execution}, Mutations: []Mutation{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.store.Save(value); err != nil {
		return RunReservation{}, err
	}
	m.tasks[taskID] = value
	m.runs[opts.IdempotencyKey] = runLocation{taskID: taskID, executionID: executionID}
	if key := externalSourceKey(value.Source); key != "" {
		m.sources[key] = taskID
	}
	info := m.project(value)
	return RunReservation{Task: info, Execution: executionInfoByID(info, executionID), Created: true}, nil
}

func sameRemoteRun(value Task, execution Execution, opts RemoteRunOptions) bool {
	if execution.Backend != ExecutionBackendRemote || execution.Remote == nil || execution.Run == nil {
		return false
	}
	link, run := execution.Remote, execution.Run
	return value.Title == strings.TrimSpace(opts.Title) && value.Source == opts.Source &&
		link.TargetID == opts.TargetID && link.TargetRevision == opts.TargetRevision &&
		link.RepositoryLabel == opts.RepositoryLabel && link.RepositoryID == opts.RepositoryID &&
		run.IdempotencyKey == opts.IdempotencyKey && run.RelativeWorkDir == opts.RelativeWorkDir &&
		run.AgentKind == opts.AgentKind && run.Model == opts.Model && run.Fleet == opts.Fleet && run.NoHook == opts.NoHook &&
		run.RequestedBase == opts.RequestedBase && run.Prompt.SHA256 == opts.PromptSHA256 && run.Prompt.Bytes == opts.PromptBytes
}

// ApplyRemotePreflight binds the endpoint identity observed before start.
func (m *Manager) ApplyRemotePreflight(taskID, executionID, targetRevision, serverInstanceID, serverBootID string, capabilities []string, repositoryIdentity string) (Info, error) {
	return m.updateRemoteLink(taskID, executionID, func(link *RemoteLink) error {
		if link.TargetRevision != targetRevision {
			return fmt.Errorf("remote target revision changed")
		}
		if link.ServerInstanceID != "" && link.ServerInstanceID != serverInstanceID {
			return fmt.Errorf("remote server instance changed")
		}
		if link.RepositoryIdentity != "" && link.RepositoryIdentity != repositoryIdentity {
			return fmt.Errorf("remote repository identity changed")
		}
		link.ServerInstanceID = serverInstanceID
		link.ServerBootID = serverBootID
		link.Capabilities = append([]string(nil), capabilities...)
		link.RepositoryIdentity = repositoryIdentity
		link.SyncState = RemoteSyncDispatching
		link.Error = ""
		return nil
	})
}

// BindRemoteExecution records the accepted remote identities and projection.
func (m *Manager) BindRemoteExecution(taskID, executionID, serverInstanceID, serverBootID, remoteTaskID, remoteExecutionID string, summary RemoteSummary) (Info, error) {
	return m.updateRemoteLink(taskID, executionID, func(link *RemoteLink) error {
		if link.ServerInstanceID == "" || link.ServerInstanceID != serverInstanceID {
			return fmt.Errorf("remote server instance changed")
		}
		if link.RemoteTaskID != "" && link.RemoteTaskID != remoteTaskID {
			return fmt.Errorf("remote task identity changed")
		}
		if link.RemoteExecutionID != "" && link.RemoteExecutionID != remoteExecutionID {
			return fmt.Errorf("remote execution identity changed")
		}
		if err := applyRemoteSummary(link, summary); err != nil {
			return err
		}
		link.ServerBootID = serverBootID
		link.RemoteTaskID = remoteTaskID
		link.RemoteExecutionID = remoteExecutionID
		link.SyncState = RemoteSyncBound
		link.Error = ""
		return nil
	})
}

func (m *Manager) ApplyRemoteInspection(taskID, executionID, serverInstanceID, serverBootID, remoteTaskID, remoteExecutionID string, summary RemoteSummary) (Info, error) {
	return m.BindRemoteExecution(taskID, executionID, serverInstanceID, serverBootID, remoteTaskID, remoteExecutionID, summary)
}

func (m *Manager) SetRemoteSyncState(taskID, executionID string, state RemoteSyncState, message string) (Info, error) {
	return m.updateRemoteLink(taskID, executionID, func(link *RemoteLink) error {
		link.SyncState = state
		link.Error = boundedRunString(message)
		return nil
	})
}

func (m *Manager) updateRemoteLink(taskID, executionID string, update func(*RemoteLink) error) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tasks[taskID]
	if !ok {
		return Info{}, fmt.Errorf("task not found: %s", taskID)
	}
	updated := current
	updated.Executions = append([]Execution(nil), current.Executions...)
	found := false
	for i := range updated.Executions {
		if updated.Executions[i].ID != executionID {
			continue
		}
		if updated.Executions[i].Backend != ExecutionBackendRemote || updated.Executions[i].Remote == nil {
			return Info{}, fmt.Errorf("execution %s is not remote", executionID)
		}
		link := cloneRemoteLink(updated.Executions[i].Remote)
		if err := update(link); err != nil {
			link.SyncState = RemoteSyncBlocked
			link.Error = boundedRunString(err.Error())
			link.UpdatedAt = m.now()
			updated.Executions[i].Remote = link
			updated.UpdatedAt = link.UpdatedAt
			if saveErr := m.store.Save(updated); saveErr != nil {
				return Info{}, saveErr
			}
			m.tasks[taskID] = updated
			return m.project(updated), err
		}
		link.UpdatedAt = m.now()
		updated.Executions[i].Remote = link
		found = true
		break
	}
	if !found {
		return Info{}, fmt.Errorf("execution not found: %s", executionID)
	}
	updated.UpdatedAt = m.now()
	if err := m.store.Save(updated); err != nil {
		return Info{}, err
	}
	m.tasks[taskID] = updated
	return m.project(updated), nil
}

func applyRemoteSummary(link *RemoteLink, summary RemoteSummary) error {
	if summary.Sequence == 0 {
		return fmt.Errorf("remote summary sequence is required")
	}
	if link.Summary != nil {
		if summary.Sequence < link.Summary.Sequence {
			return nil
		}
		if summary.Sequence == link.Summary.Sequence {
			left, _ := json.Marshal(link.Summary)
			right, _ := json.Marshal(summary)
			if !bytes.Equal(left, right) {
				return fmt.Errorf("remote summary changed without advancing sequence")
			}
			return nil
		}
	}
	copy := summary
	if summary.Session != nil {
		sessionCopy := *summary.Session
		copy.Session = &sessionCopy
	}
	link.Summary = &copy
	return nil
}

func cloneRemoteLink(link *RemoteLink) *RemoteLink {
	if link == nil {
		return nil
	}
	clone := *link
	clone.Capabilities = append([]string(nil), link.Capabilities...)
	if link.Summary != nil {
		summary := *link.Summary
		if link.Summary.Session != nil {
			sessionCopy := *link.Summary.Session
			summary.Session = &sessionCopy
		}
		clone.Summary = &summary
	}
	return &clone
}

// RecordRemoteSummaryDigest assigns a stable monotonic sequence to a target
// projection. Re-observing identical structured state keeps the same number.
func (m *Manager) RecordRemoteSummaryDigest(taskID, executionID, digest string) (uint64, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tasks[taskID]
	if !ok {
		return 0, time.Time{}, fmt.Errorf("task not found: %s", taskID)
	}
	updated := current
	updated.Executions = append([]Execution(nil), current.Executions...)
	for i := range updated.Executions {
		if updated.Executions[i].ID != executionID || updated.Executions[i].Run == nil {
			continue
		}
		run := *updated.Executions[i].Run
		if run.RemoteOrigin == nil {
			return 0, time.Time{}, fmt.Errorf("execution %s has no remote origin", executionID)
		}
		if run.SummaryDigest == digest && run.SummarySequence > 0 {
			return run.SummarySequence, run.SummaryObservedAt, nil
		}
		run.SummarySequence++
		run.SummaryDigest = digest
		run.SummaryObservedAt = m.now()
		run.UpdatedAt = run.SummaryObservedAt
		updated.Executions[i].Run = &run
		updated.UpdatedAt = run.UpdatedAt
		if err := m.store.Save(updated); err != nil {
			return 0, time.Time{}, err
		}
		m.tasks[taskID] = updated
		return run.SummarySequence, run.SummaryObservedAt, nil
	}
	return 0, time.Time{}, fmt.Errorf("execution not found: %s", executionID)
}

// FindRemoteExecution resolves the target-side binding created before any
// worktree or session side effect. The optional remote execution identity
// prevents a controller from inspecting a different retry binding.
func (m *Manager) FindRemoteExecution(controllerID, controllerExecutionID, remoteExecutionID string) (Info, ExecutionInfo, error) {
	m.mu.RLock()
	location, ok := m.remoteIDs[controllerID+"\x00"+controllerExecutionID]
	if !ok {
		m.mu.RUnlock()
		return Info{}, ExecutionInfo{}, fmt.Errorf("remote execution not found")
	}
	value := m.tasks[location.taskID]
	m.mu.RUnlock()
	info := m.project(value)
	execution := executionInfoByID(info, location.executionID)
	if execution.ID == "" || (remoteExecutionID != "" && execution.ID != remoteExecutionID) {
		return Info{}, ExecutionInfo{}, fmt.Errorf("remote execution identity mismatch")
	}
	return info, execution, nil
}

func sameRun(value Task, execution Execution, opts RunOptions) bool {
	run := execution.Run
	return value.Title == strings.TrimSpace(opts.Title) && value.Source == opts.Source && run.IdempotencyKey == opts.IdempotencyKey &&
		run.Repo == opts.Repo && run.RelativeWorkDir == opts.RelativeWorkDir && run.AgentKind == opts.AgentKind &&
		run.Model == opts.Model && run.Fleet == opts.Fleet && run.NoHook == opts.NoHook &&
		run.RequestedBase == opts.RequestedBase && run.Prompt.SHA256 == opts.PromptSHA256 && run.Prompt.Bytes == opts.PromptBytes &&
		sameRemoteOrigin(run.RemoteOrigin, opts.RemoteOrigin)
}

func sameRemoteOrigin(left, right *RemoteOrigin) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func cloneRemoteOrigin(origin *RemoteOrigin) *RemoteOrigin {
	if origin == nil {
		return nil
	}
	clone := *origin
	return &clone
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
	kind := opts.Source.Kind
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "manual"
	}
	opts.Source.Kind = kind
	if err := ValidateCreateOptions(opts); err != nil {
		return Info{}, err
	}
	now := m.now()
	value := Task{
		SchemaVersion: SchemaVersion, ID: m.newID(), Title: strings.TrimSpace(opts.Title),
		Source: opts.Source, RequestedBase: opts.RequestedBase,
		PromptSummary: opts.PromptSummary, Executions: []Execution{}, Mutations: []Mutation{}, CreatedAt: now, UpdatedAt: now,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if key := externalSourceKey(value.Source); key != "" {
		if taskID, exists := m.sources[key]; exists {
			return Info{}, fmt.Errorf("external source already belongs to task %s", taskID)
		}
	}
	if err := m.store.Save(value); err != nil {
		return Info{}, err
	}
	m.tasks[value.ID] = value
	if key := externalSourceKey(value.Source); key != "" {
		m.sources[key] = value.ID
	}
	return m.project(value), nil
}

func externalSourceKey(source Source) string {
	if source.Provider == "" || source.Repository == "" || source.ExternalID == "" {
		return ""
	}
	return strings.ToLower(source.Provider) + "\x00" + strings.ToLower(source.Repository) + "\x00" + source.ExternalID
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
		ID: m.newID(), Sequence: sequence, Backend: ExecutionBackendLocal,
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
		Mutations:  append([]Mutation(nil), value.Mutations...),
		CreatedAt:  value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	for _, execution := range value.Executions {
		// Info is a value projection. Clone the optional journal too, otherwise
		// a caller holding ExecutionInfo could mutate Manager state outside mu.
		if execution.Run != nil {
			run := *execution.Run
			run.RemoteOrigin = cloneRemoteOrigin(run.RemoteOrigin)
			execution.Run = &run
		}
		execution.Remote = cloneRemoteLink(execution.Remote)
		projected := ExecutionInfo{Execution: execution, ReferenceState: ReferenceMissing}
		if execution.Backend == ExecutionBackendRemote && execution.Remote != nil &&
			execution.Remote.Summary != nil && execution.Remote.Summary.Session != nil {
			projected.ReferenceState = ReferencePresent
			projected.SessionStatus = execution.Remote.Summary.Session.Status
			projected.Attention = execution.Remote.Summary.Session.Attention
		} else if execution.Backend == ExecutionBackendLocal {
			if sess, ok := m.sessions.GetInfo(execution.SessionID); ok {
				projected.ReferenceState = ReferencePresent
				projected.SessionStatus = sess.Status
				projected.Attention = sess.Attention
			}
		}
		info.Executions = append(info.Executions, projected)
	}
	if len(info.Executions) > 0 {
		latest := info.Executions[len(info.Executions)-1]
		info.LatestAttention = &LatestAttention{
			ExecutionID: latest.ID, Backend: latest.Backend, SessionID: latest.SessionID,
			ReferenceState: latest.ReferenceState, SessionStatus: latest.SessionStatus,
			Attention: latest.Attention,
		}
		if latest.Remote != nil && latest.Remote.Summary != nil && latest.Remote.Summary.Session != nil {
			info.LatestAttention.RemoteSessionID = latest.Remote.Summary.Session.ID
		}
	}
	return info
}
