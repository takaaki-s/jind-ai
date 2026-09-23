package task

import (
	"fmt"
	"strings"
)

type RemoteOperationKind string

const (
	RemoteOperationCancel  RemoteOperationKind = "cancel"
	RemoteOperationCleanup RemoteOperationKind = "cleanup"
)

func validateRemoteOperation(kind RemoteOperationKind, key string) error {
	if kind != RemoteOperationCancel && kind != RemoteOperationCleanup {
		return fmt.Errorf("unsupported remote operation %q", kind)
	}
	if strings.TrimSpace(key) == "" || len(key) > MaxIdempotencyKeyLength || strings.ContainsAny(key, "\r\n\x00") {
		return fmt.Errorf("invalid remote operation idempotency key")
	}
	return nil
}

func remoteLinkOperation(link *RemoteLink, kind RemoteOperationKind) **RemoteOperationReceipt {
	if kind == RemoteOperationCancel {
		return &link.Cancel
	}
	return &link.Cleanup
}

func runRemoteOperation(run *Run, kind RemoteOperationKind) **RemoteOperationReceipt {
	if kind == RemoteOperationCancel {
		return &run.RemoteCancel
	}
	return &run.RemoteCleanup
}

// ReserveRemoteOperation persists controller intent before network I/O. A
// retry with the same key is sent again only while the outcome is unknown.
func (m *Manager) ReserveRemoteOperation(taskID, executionID string, kind RemoteOperationKind, key string) (Info, RemoteOperationReceipt, bool, error) {
	if err := validateRemoteOperation(kind, key); err != nil {
		return Info{}, RemoteOperationReceipt{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tasks[taskID]
	if !ok {
		return Info{}, RemoteOperationReceipt{}, false, fmt.Errorf("task not found: %s", taskID)
	}
	updated := current
	updated.Executions = append([]Execution(nil), current.Executions...)
	shouldCall := false
	var receipt RemoteOperationReceipt
	for i := range updated.Executions {
		if updated.Executions[i].ID != executionID || updated.Executions[i].Remote == nil {
			continue
		}
		link := cloneRemoteLink(updated.Executions[i].Remote)
		if link.RemoteExecutionID == "" || link.ServerInstanceID == "" {
			return Info{}, RemoteOperationReceipt{}, false, fmt.Errorf("remote execution is not bound")
		}
		slot := remoteLinkOperation(link, kind)
		if *slot != nil {
			if (*slot).IdempotencyKey != key && (*slot).Status != RemoteOperationFailed {
				return Info{}, RemoteOperationReceipt{}, false, fmt.Errorf("remote %s already uses idempotency key %q", kind, (*slot).IdempotencyKey)
			}
			if (*slot).IdempotencyKey == key {
				receipt = **slot
				shouldCall = receipt.Status == RemoteOperationUnknown
				return m.project(current), receipt, shouldCall, nil
			}
		}
		receipt = RemoteOperationReceipt{IdempotencyKey: key, Status: RemoteOperationRunning, UpdatedAt: m.now()}
		*slot = cloneRemoteOperationReceipt(&receipt)
		shouldCall = true
		link.UpdatedAt = receipt.UpdatedAt
		updated.Executions[i].Remote = link
		updated.UpdatedAt = receipt.UpdatedAt
		if err := m.store.Save(updated); err != nil {
			return Info{}, RemoteOperationReceipt{}, false, err
		}
		m.tasks[taskID] = updated
		return m.project(updated), receipt, shouldCall, nil
	}
	return Info{}, RemoteOperationReceipt{}, false, fmt.Errorf("remote execution not found: %s", executionID)
}

func (m *Manager) ApplyRemoteOperation(taskID, executionID string, kind RemoteOperationKind, receipt RemoteOperationReceipt) (Info, error) {
	if err := validateRemoteOperation(kind, receipt.IdempotencyKey); err != nil {
		return Info{}, err
	}
	return m.updateRemoteLink(taskID, executionID, func(link *RemoteLink) error {
		slot := remoteLinkOperation(link, kind)
		if *slot == nil || (*slot).IdempotencyKey != receipt.IdempotencyKey {
			return fmt.Errorf("remote %s receipt does not match reserved operation", kind)
		}
		receipt.Error = boundedRunString(receipt.Error)
		receipt.Guidance = boundedRunString(receipt.Guidance)
		receipt.UpdatedAt = m.now()
		*slot = cloneRemoteOperationReceipt(&receipt)
		return nil
	})
}

// ReserveTargetRemoteOperation persists a target receipt before touching its
// locally owned Session. A new key is accepted only after a definite failure;
// running, unknown, and succeeded receipts remain bound to their original key.
func (m *Manager) ReserveTargetRemoteOperation(taskID, executionID string, kind RemoteOperationKind, key string) (Info, RemoteOperationReceipt, bool, error) {
	if err := validateRemoteOperation(kind, key); err != nil {
		return Info{}, RemoteOperationReceipt{}, false, err
	}
	shouldRun := false
	var receipt RemoteOperationReceipt
	info, err := m.updateTargetRun(taskID, executionID, func(run *Run) error {
		if run.RemoteOrigin == nil {
			return fmt.Errorf("execution has no remote origin")
		}
		slot := runRemoteOperation(run, kind)
		if *slot != nil {
			if (*slot).IdempotencyKey != key && (*slot).Status != RemoteOperationFailed {
				return fmt.Errorf("remote %s already uses a different idempotency key", kind)
			}
			if (*slot).IdempotencyKey == key {
				receipt = **slot
				return nil
			}
		}
		receipt = RemoteOperationReceipt{IdempotencyKey: key, Status: RemoteOperationRunning, UpdatedAt: m.now()}
		*slot = cloneRemoteOperationReceipt(&receipt)
		shouldRun = true
		return nil
	})
	return info, receipt, shouldRun, err
}

func (m *Manager) FinishTargetRemoteOperation(taskID, executionID string, kind RemoteOperationKind, receipt RemoteOperationReceipt) (Info, error) {
	if err := validateRemoteOperation(kind, receipt.IdempotencyKey); err != nil {
		return Info{}, err
	}
	return m.updateTargetRun(taskID, executionID, func(run *Run) error {
		slot := runRemoteOperation(run, kind)
		if *slot == nil || (*slot).IdempotencyKey != receipt.IdempotencyKey {
			return fmt.Errorf("remote %s receipt does not match reserved operation", kind)
		}
		receipt.Error = boundedRunString(receipt.Error)
		receipt.Guidance = boundedRunString(receipt.Guidance)
		receipt.UpdatedAt = m.now()
		*slot = cloneRemoteOperationReceipt(&receipt)
		return nil
	})
}

func (m *Manager) updateTargetRun(taskID, executionID string, update func(*Run) error) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tasks[taskID]
	if !ok {
		return Info{}, fmt.Errorf("task not found: %s", taskID)
	}
	updated := current
	updated.Executions = append([]Execution(nil), current.Executions...)
	for i := range updated.Executions {
		if updated.Executions[i].ID != executionID || updated.Executions[i].Run == nil {
			continue
		}
		run := *updated.Executions[i].Run
		run.RemoteOrigin = cloneRemoteOrigin(run.RemoteOrigin)
		run.RemoteCancel = cloneRemoteOperationReceipt(run.RemoteCancel)
		run.RemoteCleanup = cloneRemoteOperationReceipt(run.RemoteCleanup)
		if err := update(&run); err != nil {
			return Info{}, err
		}
		run.UpdatedAt = m.now()
		updated.Executions[i].Run = &run
		updated.UpdatedAt = run.UpdatedAt
		if err := m.store.Save(updated); err != nil {
			return Info{}, err
		}
		m.tasks[taskID] = updated
		return m.project(updated), nil
	}
	return Info{}, fmt.Errorf("execution not found: %s", executionID)
}
