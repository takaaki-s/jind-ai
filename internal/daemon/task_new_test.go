package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
)

type fakeTaskExecutionDriver struct {
	mu             sync.Mutex
	sessions       map[string]session.Info
	reserveCalls   int
	provisionCalls int
	startCalls     int
	submitCalls    int
	workdir        string
	prompt         string
	opts           session.CreateOptions
	provisionGate  chan struct{}
	reserveErr     error
	provisionErr   error
	workdirErr     error
	startErr       error
	startStatus    session.Status
	submitErr      error
}

func newFakeTaskExecutionDriver() *fakeTaskExecutionDriver {
	return &fakeTaskExecutionDriver{sessions: make(map[string]session.Info)}
}

func (f *fakeTaskExecutionDriver) GetInfo(id string) (session.Info, bool) { return f.Get(id) }

func (f *fakeTaskExecutionDriver) Reserve(opts session.CreateOptions) (session.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	f.opts = opts
	if f.reserveErr != nil {
		return session.Info{}, f.reserveErr
	}
	info := session.Info{ID: opts.ReservedID, Status: session.StatusCreating, WorkDir: opts.WorkDir}
	f.sessions[info.ID] = info
	return info, nil
}

func (f *fakeTaskExecutionDriver) Provision(id string, opts session.CreateOptions) (string, error) {
	f.mu.Lock()
	f.provisionCalls++
	f.opts = opts
	gate, err := f.provisionGate, f.provisionErr
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	info := f.sessions[id]
	info.WorkDir = filepath.Join(opts.WorkDir, ".fake-worktree")
	info.ReviewBase = session.ReviewBase{RequestedRef: "origin/main", CommitOID: "abc", WorktreePath: info.WorkDir}
	f.sessions[id] = info
	f.mu.Unlock()
	return "hook warning", nil
}

func (f *fakeTaskExecutionDriver) SetInitialWorkDir(_ string, relative string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workdir = relative
	return f.workdirErr
}

func (f *fakeTaskExecutionDriver) Start(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.startErr != nil {
		return f.startErr
	}
	info := f.sessions[id]
	info.Status = f.startStatus
	if info.Status == "" {
		info.Status = session.StatusIdle
	}
	f.sessions[id] = info
	return nil
}

func (f *fakeTaskExecutionDriver) Get(id string) (session.Info, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.sessions[id]
	return info, ok
}

func (f *fakeTaskExecutionDriver) Submit(_ string, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitCalls++
	f.prompt = prompt
	return f.submitErr
}

func (f *fakeTaskExecutionDriver) MarkCreationFailed(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	info.Status = session.StatusStopped
	if err != nil {
		info.ErrorMessage = err.Error()
	}
	f.sessions[id] = info
}

func (f *fakeTaskExecutionDriver) SetCreationWarning(string, string) {}

func newTaskNewTestServer(t *testing.T) (*Server, *fakeTaskExecutionDriver, string) {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	driver := newFakeTaskExecutionDriver()
	taskManager, err := task.NewManager(filepath.Join(dir, "tasks"), driver)
	if err != nil {
		t.Fatal(err)
	}
	configManager, err := config.NewManager(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		taskManager: taskManager, taskDriver: driver, configMgr: configManager,
		taskAgentValidator: func(string) error { return nil },
	}, driver, repo
}

func taskNewRequestJSON(t *testing.T, repo string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(TaskNewRequest{
		IdempotencyKey: "request-1", Title: "Implement it", Prompt: "Please implement it",
		Repo: repo, RelativeWorkDir: "service", RequestedBase: "main", AgentKind: "claude", Model: "opus",
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func waitForTaskRunPhase(t *testing.T, manager *task.Manager, taskID string, phase task.ExecutionPhase) task.Info {
	t.Helper()
	var got task.Info
	waitFor(t, 2*time.Second, "task run phase "+string(phase), func() bool {
		current, ok := manager.Get(taskID)
		if !ok || len(current.Executions) == 0 || current.Executions[0].Run == nil {
			return false
		}
		got = current
		return current.Executions[0].Run.Phase == phase
	})
	return got
}

func TestHandleTaskNewAcknowledgesStableIdentitiesBeforeAsyncWork(t *testing.T) {
	s, driver, repo := newTaskNewTestServer(t)
	driver.provisionGate = make(chan struct{})
	resp := s.handleTaskNew(taskNewRequestJSON(t, repo))
	if !resp.Success {
		t.Fatalf("task new: %s", resp.Error)
	}
	var result TaskNewResponse
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Task.ID == "" || result.Execution.ID == "" || result.Session.ID == "" {
		t.Fatalf("result identities = %+v", result)
	}
	if result.Execution.SessionID != result.Session.ID || result.Execution.Run.Phase != task.ExecutionProvisioning {
		t.Fatalf("result = %+v", result)
	}
	if result.Execution.ReferenceState != task.ReferencePresent {
		t.Fatalf("reference state = %s", result.Execution.ReferenceState)
	}
	close(driver.provisionGate)
	finished := waitForTaskRunPhase(t, s.taskManager, result.Task.ID, task.ExecutionSubmitted)
	if driver.prompt != "Please implement it" || driver.workdir != "service" {
		t.Fatalf("prompt=%q workdir=%q", driver.prompt, driver.workdir)
	}
	if driver.opts.ReservedID != result.Session.ID || !driver.opts.Worktree || driver.opts.WorktreeName == "" || driver.opts.WorktreeBranch == "" {
		t.Fatalf("session options = %+v", driver.opts)
	}
	if finished.Executions[0].Run.Warning != "hook warning" {
		t.Fatalf("run warning = %q", finished.Executions[0].Run.Warning)
	}

	duplicate := s.handleTaskNew(taskNewRequestJSON(t, repo))
	if !duplicate.Success {
		t.Fatalf("duplicate: %s", duplicate.Error)
	}
	if driver.reserveCalls != 1 || driver.provisionCalls != 1 || driver.submitCalls != 1 {
		t.Fatalf("duplicate side effects: reserve=%d provision=%d submit=%d", driver.reserveCalls, driver.provisionCalls, driver.submitCalls)
	}

	driver.mu.Lock()
	delete(driver.sessions, result.Session.ID)
	driver.mu.Unlock()
	missingSession := s.handleTaskNew(taskNewRequestJSON(t, repo))
	if !missingSession.Success {
		t.Fatalf("settled replay after session deletion: %s", missingSession.Error)
	}
	settled, _ := s.taskManager.Get(result.Task.ID)
	if settled.Executions[0].Run.Phase != task.ExecutionSubmitted {
		t.Fatalf("session deletion rewrote settled run: %+v", settled.Executions[0].Run)
	}
}

func TestHandleTaskNewRetainsFailureAndRetriesSameIdentity(t *testing.T) {
	s, driver, repo := newTaskNewTestServer(t)
	driver.provisionErr = errors.New("branch already exists")
	resp := s.handleTaskNew(taskNewRequestJSON(t, repo))
	if !resp.Success {
		t.Fatalf("task new: %s", resp.Error)
	}
	var result TaskNewResponse
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatal(err)
	}
	failed := waitForTaskRunPhase(t, s.taskManager, result.Task.ID, task.ExecutionFailed)
	run := failed.Executions[0].Run
	if run.FailedPhase != task.ExecutionProvisioning || run.Error != "branch already exists" || run.Guidance == "" {
		t.Fatalf("failed run = %+v", run)
	}

	driver.mu.Lock()
	driver.provisionErr = nil
	driver.mu.Unlock()
	retry := s.handleTaskNew(taskNewRequestJSON(t, repo))
	if !retry.Success {
		t.Fatalf("retry: %s", retry.Error)
	}
	waitForTaskRunPhase(t, s.taskManager, result.Task.ID, task.ExecutionSubmitted)
	if driver.reserveCalls != 1 || driver.provisionCalls != 2 || driver.submitCalls != 1 {
		t.Fatalf("retry side effects: reserve=%d provision=%d submit=%d", driver.reserveCalls, driver.provisionCalls, driver.submitCalls)
	}
}

func TestHandleTaskNewJournalsEveryExecutionBoundaryFailure(t *testing.T) {
	tests := []struct {
		name        string
		failedPhase task.ExecutionPhase
		configure   func(*fakeTaskExecutionDriver)
		timeout     bool
	}{
		{name: "session reservation", failedPhase: task.ExecutionReserved, configure: func(d *fakeTaskExecutionDriver) {
			d.reserveErr = errors.New("reserve failed")
		}},
		{name: "initial workdir", failedPhase: task.ExecutionConfiguring, configure: func(d *fakeTaskExecutionDriver) {
			d.workdirErr = errors.New("workdir failed")
		}},
		{name: "session start", failedPhase: task.ExecutionStarting, configure: func(d *fakeTaskExecutionDriver) {
			d.startErr = errors.New("start failed")
		}},
		{name: "session stopped while waiting", failedPhase: task.ExecutionWaiting, configure: func(d *fakeTaskExecutionDriver) {
			d.startStatus = session.StatusStopped
		}},
		{name: "readiness timeout", failedPhase: task.ExecutionWaiting, timeout: true, configure: func(d *fakeTaskExecutionDriver) {
			d.startStatus = session.StatusRunning
		}},
		{name: "prompt submission", failedPhase: task.ExecutionSubmitting, configure: func(d *fakeTaskExecutionDriver) {
			d.submitErr = errors.New("send uncertain")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, driver, repo := newTaskNewTestServer(t)
			tc.configure(driver)
			oldTimeout, oldPoll := taskReadyTimeout, taskReadyPoll
			if tc.timeout {
				taskReadyTimeout, taskReadyPoll = 15*time.Millisecond, time.Millisecond
			}
			defer func() { taskReadyTimeout, taskReadyPoll = oldTimeout, oldPoll }()

			resp := s.handleTaskNew(taskNewRequestJSON(t, repo))
			if !resp.Success {
				t.Fatalf("task new: %s", resp.Error)
			}
			var result TaskNewResponse
			if err := json.Unmarshal(resp.Data, &result); err != nil {
				t.Fatal(err)
			}
			failed := waitForTaskRunPhase(t, s.taskManager, result.Task.ID, task.ExecutionFailed)
			run := failed.Executions[0].Run
			if run.FailedPhase != tc.failedPhase || run.Error == "" || run.Guidance == "" {
				t.Fatalf("failed run = %+v", run)
			}
			if result.Execution.SessionID == "" {
				t.Fatal("failure lost the reserved session identity")
			}

			if tc.failedPhase == task.ExecutionSubmitting {
				before := driver.submitCalls
				retry := s.handleTaskNew(taskNewRequestJSON(t, repo))
				if !retry.Success || driver.submitCalls != before {
					t.Fatalf("uncertain submission was retried: response=%+v calls=%d", retry, driver.submitCalls)
				}
			}
		})
	}
}

func TestResumePhaseFailsClosedAtUncertainBoundaries(t *testing.T) {
	info := session.Info{ID: "session-1"}
	phase, guidance := resumePhase(&task.Run{
		Phase: task.ExecutionInterrupted, FailedPhase: task.ExecutionSubmitting,
	}, info)
	if phase != "" || guidance == "" {
		t.Fatalf("submitting resume = %q, %q", phase, guidance)
	}
	phase, guidance = resumePhase(&task.Run{
		Phase: task.ExecutionInterrupted, FailedPhase: task.ExecutionProvisioning,
	}, info)
	if phase != "" || guidance == "" {
		t.Fatalf("provisioning resume = %q, %q", phase, guidance)
	}
}

func TestHandleTaskNewAfterRestartReusesWaitingSession(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	driver := newFakeTaskExecutionDriver()
	tasksDir := filepath.Join(dir, "tasks")
	manager, err := task.NewManager(tasksDir, driver)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Resume safely"
	reserved, err := manager.ReserveRun(task.RunOptions{
		IdempotencyKey: "restart-1", Title: "Resume", Repo: repo, AgentKind: "claude",
		RequestedBase: "main", BranchPrefix: "jin/", PromptSHA256: task.PromptDigest(prompt), PromptBytes: len(prompt),
	})
	if err != nil {
		t.Fatal(err)
	}
	driver.sessions[reserved.Execution.SessionID] = session.Info{
		ID: reserved.Execution.SessionID, Status: session.StatusStopped,
		ReviewBase: session.ReviewBase{RequestedRef: "origin/main", CommitOID: "abc", WorktreePath: "/worktree"},
	}
	if _, err := manager.SetRunPhase(reserved.Task.ID, reserved.Execution.ID, task.ExecutionWaiting, "", "", "", ""); err != nil {
		t.Fatal(err)
	}

	restarted, err := task.NewManager(tasksDir, driver)
	if err != nil {
		t.Fatal(err)
	}
	configManager, err := config.NewManager(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		taskManager: restarted, taskDriver: driver, configMgr: configManager,
		taskAgentValidator: func(string) error { return nil },
	}
	data, _ := json.Marshal(TaskNewRequest{
		IdempotencyKey: "restart-1", Title: "Resume", Prompt: prompt, Repo: repo,
		RequestedBase: "main", AgentKind: "claude",
	})
	resp := s.handleTaskNew(data)
	if !resp.Success {
		t.Fatalf("resume: %s", resp.Error)
	}
	waitForTaskRunPhase(t, restarted, reserved.Task.ID, task.ExecutionSubmitted)
	if driver.reserveCalls != 0 || driver.provisionCalls != 0 || driver.startCalls != 1 || driver.submitCalls != 1 {
		t.Fatalf("restart side effects: reserve=%d provision=%d start=%d submit=%d",
			driver.reserveCalls, driver.provisionCalls, driver.startCalls, driver.submitCalls)
	}
}

func TestHandleTaskNewRejectsUnsafeInputBeforeReservation(t *testing.T) {
	s, driver, repo := newTaskNewTestServer(t)
	tests := []TaskNewRequest{
		{Prompt: "x", Repo: repo, AgentKind: "claude"},
		{IdempotencyKey: "x", Prompt: "   ", Repo: repo, AgentKind: "claude"},
		{IdempotencyKey: "x", Prompt: "x", Repo: repo, RelativeWorkDir: "../outside", AgentKind: "claude"},
		{IdempotencyKey: "x", Prompt: "x", Repo: filepath.Join(repo, "missing"), AgentKind: "claude"},
	}
	for _, req := range tests {
		data, _ := json.Marshal(req)
		if resp := s.handleTaskNew(data); resp.Success {
			t.Fatalf("accepted request: %+v", req)
		}
	}
	if driver.reserveCalls != 0 || len(s.taskManager.List()) != 0 {
		t.Fatal("invalid request created state")
	}
}
