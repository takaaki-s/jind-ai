package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/internal/agent"
	jingit "github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
)

var (
	taskReadyTimeout = 90 * time.Second
	taskReadyPoll    = 100 * time.Millisecond
)

// TaskNewRequest carries a prompt only for this live attempt. The daemon never
// places Prompt in the Task store; the execution journal retains its digest and
// byte count instead.
type TaskNewRequest struct {
	IdempotencyKey  string `json:"idempotency_key"`
	Title           string `json:"title,omitempty"`
	Prompt          string `json:"prompt"`
	Repo            string `json:"repo"`
	RelativeWorkDir string `json:"workdir,omitempty"`
	RequestedBase   string `json:"requested_base,omitempty"`
	AgentKind       string `json:"agent_kind,omitempty"`
	Model           string `json:"model,omitempty"`
	Fleet           string `json:"fleet,omitempty"`
	NoHook          bool   `json:"no_hook,omitempty"`
}

type TaskNewResponse struct {
	Task      task.Info          `json:"task"`
	Execution task.ExecutionInfo `json:"execution"`
	Session   session.Info       `json:"session"`
}

type taskExecutionDriver interface {
	Reserve(session.CreateOptions) (session.Info, error)
	Provision(string, session.CreateOptions) (string, error)
	SetInitialWorkDir(string, string) error
	Start(string) error
	Get(string) (session.Info, bool)
	Submit(string, string) error
	MarkCreationFailed(string, error)
	SetCreationWarning(string, string)
}

type sessionTaskDriver struct{ manager *session.Manager }

func (d sessionTaskDriver) Reserve(opts session.CreateOptions) (session.Info, error) {
	_, info, err := d.manager.ReserveCreation(opts)
	return info, err
}
func (d sessionTaskDriver) Provision(id string, opts session.CreateOptions) (string, error) {
	return d.manager.ProvisionReserved(id, opts)
}
func (d sessionTaskDriver) SetInitialWorkDir(id, relative string) error {
	return d.manager.SetInitialWorkDir(id, relative)
}
func (d sessionTaskDriver) Start(id string) error              { return d.manager.StartBackground(id) }
func (d sessionTaskDriver) Get(id string) (session.Info, bool) { return d.manager.GetInfo(id) }
func (d sessionTaskDriver) Submit(id, prompt string) error     { return d.manager.SendPrompt(id, prompt) }
func (d sessionTaskDriver) MarkCreationFailed(id string, err error) {
	d.manager.MarkCreationFailed(id, err)
}
func (d sessionTaskDriver) SetCreationWarning(id, warning string) {
	d.manager.SetCreationWarning(id, warning)
}

func (s *Server) handleTaskNew(data json.RawMessage) Response {
	var req TaskNewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.IdempotencyKey == "" {
		return Response{Success: false, Error: "idempotency_key is required"}
	}
	if req.Prompt == "" {
		return Response{Success: false, Error: "prompt is required"}
	}
	if len(req.Prompt) > task.MaxPromptBytes {
		return Response{Success: false, Error: fmt.Sprintf("prompt exceeds %d bytes", task.MaxPromptBytes)}
	}
	if !session.PromptVerifiable(req.Prompt) {
		return Response{Success: false, Error: "prompt has no verifiable content (only whitespace or box-drawing characters)"}
	}
	repo, err := canonicalRepo(req.Repo)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if err := validateRelativeWorkDir(req.RelativeWorkDir); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.AgentKind == "" {
		req.AgentKind = s.configMgr.GetDefaultAgent()
	}
	if err := s.validateTaskAgent(req.AgentKind); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if strings.TrimSpace(req.Title) == "" {
		req.Title = filepath.Base(repo) + " task"
	}
	req.Repo = repo

	// Shared with session new: only one worktree reservation/provisioning chain
	// may inspect and mutate the repository at a time.
	s.createMu.Lock()
	reservation, err := s.taskManager.ReserveRun(task.RunOptions{
		IdempotencyKey: req.IdempotencyKey, Title: req.Title, Repo: req.Repo,
		RelativeWorkDir: req.RelativeWorkDir, AgentKind: req.AgentKind, Model: req.Model,
		Fleet: req.Fleet, NoHook: req.NoHook, RequestedBase: req.RequestedBase,
		BranchPrefix: s.configMgr.GetWorktreeConfig().BranchPrefix,
		PromptSHA256: task.PromptDigest(req.Prompt), PromptBytes: len(req.Prompt),
	})
	if err != nil {
		s.createMu.Unlock()
		return Response{Success: false, Error: err.Error()}
	}
	execution := reservation.Execution
	run := execution.Run
	if run == nil {
		s.createMu.Unlock()
		return Response{Success: false, Error: "reserved execution has no run journal"}
	}
	opts := runSessionOptions(reservation.Task.Title, execution)

	info, sessionExists := s.taskDriver.Get(execution.SessionID)
	if !sessionExists && run.Phase == task.ExecutionSubmitted {
		// A completed Task outlives its session by design. Replaying its key is
		// a read of the settled reservation, not a request to recreate work or
		// rewrite the successful journal as a missing-session failure.
		s.createMu.Unlock()
		return taskNewSuccess(reservation.Task, execution.ID, session.Info{
			ID: execution.SessionID, Status: session.StatusStopped,
		})
	}
	if !sessionExists {
		if !runCanReserveSession(run) {
			updated := s.failTaskRun(reservation.Task.ID, execution.ID, run.Phase,
				fmt.Errorf("recorded session %s is missing", execution.SessionID),
				"the Task and Execution were retained; inspect them before starting a replacement session")
			s.createMu.Unlock()
			return taskNewSuccess(updated, execution.ID, session.Info{ID: execution.SessionID, Status: session.StatusStopped})
		}
		info, err = s.taskDriver.Reserve(opts)
		if err != nil {
			updated := s.failTaskRun(reservation.Task.ID, execution.ID, task.ExecutionReserved, err,
				"retry with the same idempotency key; no worktree was created")
			s.createMu.Unlock()
			return taskNewSuccess(updated, execution.ID, session.Info{
				ID: execution.SessionID, Status: session.StatusStopped, ErrorMessage: err.Error(),
			})
		}
		run.Phase = task.ExecutionReserved
		// ReserveRun projects the Task before the session record exists. Refresh
		// it so the acknowledgement never reports its newly-reserved execution
		// as a missing reference.
		if current, ok := s.taskManager.Get(reservation.Task.ID); ok {
			reservation.Task = current
		}
	}

	startAt, guidance := resumePhase(run, info)
	if guidance != "" {
		updated := s.failTaskRun(reservation.Task.ID, execution.ID, run.FailedPhase, fmt.Errorf("cannot safely resume automatically"), guidance)
		s.createMu.Unlock()
		return taskNewSuccess(updated, execution.ID, info)
	}
	if startAt == task.ExecutionSubmitted || runPhaseActive(run.Phase) {
		s.createMu.Unlock()
		return taskNewSuccess(reservation.Task, execution.ID, info)
	}

	if startAt == task.ExecutionProvisioning {
		updated, updateErr := s.taskManager.SetRunPhase(reservation.Task.ID, execution.ID, task.ExecutionProvisioning, "", "", "", "")
		if updateErr != nil {
			s.createMu.Unlock()
			return Response{Success: false, Error: updateErr.Error()}
		}
		go s.provisionTaskRun(updated.ID, execution.ID, req.Prompt, run.RelativeWorkDir, opts)
		return taskNewSuccess(updated, execution.ID, info)
	}

	s.createMu.Unlock()
	updated, updateErr := s.taskManager.SetRunPhase(reservation.Task.ID, execution.ID, startAt, "", "", "", "")
	if updateErr != nil {
		return Response{Success: false, Error: updateErr.Error()}
	}
	go s.continueTaskRun(updated.ID, execution.ID, req.Prompt, run.RelativeWorkDir, opts, startAt)
	return taskNewSuccess(updated, execution.ID, info)
}

func (s *Server) validateTaskAgent(kind string) error {
	if s.taskAgentValidator != nil {
		return s.taskAgentValidator(kind)
	}
	_, err := agent.Lookup(kind)
	return err
}

func canonicalRepo(repo string) (string, error) {
	if strings.TrimSpace(repo) == "" {
		return "", fmt.Errorf("repo is required")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve repo: %w", err)
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("repo is not a directory: %s", repo)
	}
	if !jingit.IsGitRoot(resolved) {
		return "", fmt.Errorf("not a git repository root: %s", resolved)
	}
	return resolved, nil
}

func validateRelativeWorkDir(relative string) error {
	if relative == "" || relative == "." {
		return nil
	}
	if filepath.IsAbs(relative) {
		return fmt.Errorf("workdir must be relative to the repository")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workdir escapes the repository: %s", relative)
	}
	return nil
}

func runSessionOptions(title string, execution task.ExecutionInfo) session.CreateOptions {
	run := execution.Run
	return session.CreateOptions{
		ReservedID: execution.SessionID, Description: title, WorkDir: run.Repo,
		Fleet: run.Fleet, AgentKind: run.AgentKind, Model: run.Model,
		Worktree: true, WorktreeName: run.WorktreeName, WorktreeBranch: run.WorktreeBranch,
		WorktreeBase: run.RequestedBase, NoHook: run.NoHook,
	}
}

func runCanReserveSession(run *task.Run) bool {
	return run.Phase == task.ExecutionReserved ||
		(run.Phase == task.ExecutionFailed && run.FailedPhase == task.ExecutionReserved) ||
		(run.Phase == task.ExecutionInterrupted && run.FailedPhase == task.ExecutionReserved)
}

func runPhaseActive(phase task.ExecutionPhase) bool {
	switch phase {
	case task.ExecutionProvisioning, task.ExecutionConfiguring, task.ExecutionStarting, task.ExecutionWaiting, task.ExecutionSubmitting:
		return true
	default:
		return false
	}
}

func resumePhase(run *task.Run, info session.Info) (task.ExecutionPhase, string) {
	if run.Phase == task.ExecutionSubmitted {
		return task.ExecutionSubmitted, ""
	}
	phase := run.Phase
	if phase == task.ExecutionFailed || phase == task.ExecutionInterrupted {
		phase = run.FailedPhase
	}
	switch phase {
	case task.ExecutionReserved:
		return task.ExecutionProvisioning, ""
	case task.ExecutionProvisioning:
		if !info.ReviewBase.IsZero() {
			return task.ExecutionConfiguring, ""
		}
		if run.Phase == task.ExecutionFailed {
			return task.ExecutionProvisioning, ""
		}
		return "", fmt.Sprintf("provisioning was interrupted with session %s retained; inspect branch %s and worktree %s before cleanup or retry", info.ID, run.WorktreeBranch, run.WorktreeName)
	case task.ExecutionConfiguring:
		return task.ExecutionConfiguring, ""
	case task.ExecutionStarting, task.ExecutionWaiting:
		if info.ReviewBase.IsZero() {
			return "", "the recorded session has no managed worktree evidence; inspect it before retrying"
		}
		return task.ExecutionStarting, ""
	case task.ExecutionSubmitting:
		return "", fmt.Sprintf("prompt submission may be partial; attach session %s and inspect it before sending anything again", info.ID)
	default:
		return "", "execution state is not automatically resumable; inspect the Task and session"
	}
}

func (s *Server) provisionTaskRun(taskID, executionID, prompt, relativeWorkDir string, opts session.CreateOptions) {
	warning, err := s.taskDriver.Provision(opts.ReservedID, opts)
	if err != nil {
		s.taskDriver.MarkCreationFailed(opts.ReservedID, err)
		s.failTaskRun(taskID, executionID, task.ExecutionProvisioning, err,
			"retry with the same idempotency key; regular provisioning failures are rolled back")
		s.createMu.Unlock()
		return
	}
	if warning != "" {
		s.taskDriver.SetCreationWarning(opts.ReservedID, warning)
	}
	updated, err := s.taskManager.SetRunPhase(taskID, executionID, task.ExecutionConfiguring, "", "", "", warning)
	s.createMu.Unlock()
	if err != nil {
		debugLog("[TASK] persist configuring phase for %s: %v", executionID, err)
		s.failTaskRun(taskID, executionID, task.ExecutionProvisioning, err,
			"the worktree and session were retained; retry with the same idempotency key")
		return
	}
	s.continueTaskRun(updated.ID, executionID, prompt, relativeWorkDir, opts, task.ExecutionConfiguring)
}

func (s *Server) continueTaskRun(taskID, executionID, prompt, relativeWorkDir string, opts session.CreateOptions, startAt task.ExecutionPhase) {
	if startAt == task.ExecutionConfiguring {
		if err := s.taskDriver.SetInitialWorkDir(opts.ReservedID, relativeWorkDir); err != nil {
			s.taskDriver.MarkCreationFailed(opts.ReservedID, err)
			s.failTaskRun(taskID, executionID, task.ExecutionConfiguring, err,
				"the worktree and session were retained; inspect them, or intentionally create a replacement Task with a new idempotency key and corrected --workdir")
			return
		}
	}
	if _, err := s.taskManager.SetRunPhase(taskID, executionID, task.ExecutionStarting, "", "", "", ""); err != nil {
		s.failTaskRun(taskID, executionID, task.ExecutionConfiguring, err,
			"the session was configured but the journal update failed; retry with the same idempotency key")
		return
	}
	if err := s.taskDriver.Start(opts.ReservedID); err != nil {
		s.taskDriver.MarkCreationFailed(opts.ReservedID, err)
		s.failTaskRun(taskID, executionID, task.ExecutionStarting, err,
			"retry with the same idempotency key; the worktree and session were retained")
		return
	}
	if _, err := s.taskManager.SetRunPhase(taskID, executionID, task.ExecutionWaiting, "", "", "", ""); err != nil {
		s.failTaskRun(taskID, executionID, task.ExecutionStarting, err,
			"the session may already be running; retry with the same idempotency key to reconcile it")
		return
	}
	deadline := time.Now().Add(taskReadyTimeout)
	for {
		info, ok := s.taskDriver.Get(opts.ReservedID)
		if !ok {
			s.failTaskRun(taskID, executionID, task.ExecutionWaiting, fmt.Errorf("session disappeared while waiting for readiness"),
				"the Task and Execution remain; inspect before creating a replacement")
			return
		}
		if info.Status == session.StatusIdle {
			break
		}
		if info.Status == session.StatusStopped {
			message := info.ErrorMessage
			if message == "" {
				message = "agent exited before becoming ready"
			}
			s.failTaskRun(taskID, executionID, task.ExecutionWaiting, fmt.Errorf("session stopped: %s", message),
				"retry with the same idempotency key after resolving the session startup failure")
			return
		}
		if time.Now().After(deadline) {
			s.failTaskRun(taskID, executionID, task.ExecutionWaiting, fmt.Errorf("session did not become idle within %s", taskReadyTimeout),
				"the session and worktree were retained; inspect or retry with the same idempotency key")
			return
		}
		time.Sleep(taskReadyPoll)
	}
	if _, err := s.taskManager.SetRunPhase(taskID, executionID, task.ExecutionSubmitting, "", "", "", ""); err != nil {
		s.failTaskRun(taskID, executionID, task.ExecutionWaiting, err,
			"the prompt was not submitted; retry with the same idempotency key")
		return
	}
	if err := s.taskDriver.Submit(opts.ReservedID, prompt); err != nil {
		s.failTaskRun(taskID, executionID, task.ExecutionSubmitting, err,
			fmt.Sprintf("prompt submission may be partial; attach session %s and inspect it before retrying", opts.ReservedID))
		return
	}
	if _, err := s.taskManager.SetRunPhase(taskID, executionID, task.ExecutionSubmitted, "", "", "", ""); err != nil {
		debugLog("[TASK] prompt submitted for %s but final journal write failed: %v", executionID, err)
		s.failTaskRun(taskID, executionID, task.ExecutionSubmitting, err,
			fmt.Sprintf("the prompt was submitted but its final journal update failed; attach session %s and inspect it before sending anything again", opts.ReservedID))
	}
}

func (s *Server) failTaskRun(taskID, executionID string, failed task.ExecutionPhase, err error, guidance string) task.Info {
	message := ""
	if err != nil {
		message = err.Error()
	}
	updated, updateErr := s.taskManager.SetRunPhase(taskID, executionID, task.ExecutionFailed, failed, message, guidance, "")
	if updateErr != nil {
		debugLog("[TASK] persist failure for %s: %v", executionID, updateErr)
		if current, ok := s.taskManager.Get(taskID); ok {
			return current
		}
	}
	return updated
}

func taskNewSuccess(info task.Info, executionID string, sess session.Info) Response {
	execution := task.ExecutionInfo{}
	for _, candidate := range info.Executions {
		if candidate.ID == executionID {
			execution = candidate
			break
		}
	}
	payload, _ := json.Marshal(TaskNewResponse{Task: info, Execution: execution, Session: sess})
	return Response{Success: true, Data: payload}
}

// NewTaskIdempotencyKey creates the client-side key used before IPC begins.
func NewTaskIdempotencyKey() string { return uuid.New().String() }
