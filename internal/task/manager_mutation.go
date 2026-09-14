package task

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type MutationReservation struct {
	Task     Info
	Mutation Mutation
	Created  bool
}

type mutationLocation struct {
	taskID     string
	mutationID string
}

// ReserveMutation persists running before the caller crosses the provider
// mutation boundary. An existing key is returned only for the exact same
// actor, target, kind, and content digest.
func (m *Manager) ReserveMutation(taskID string, candidate Mutation) (MutationReservation, error) {
	if err := ValidateMutation(candidate); err != nil {
		return MutationReservation{}, err
	}
	if candidate.Status != MutationRunning {
		return MutationReservation{}, fmt.Errorf("new mutation must start as running")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if location, ok := m.mutations[candidate.IdempotencyKey]; ok {
		value := m.tasks[location.taskID]
		existing, ok := mutationByID(value, location.mutationID)
		if !ok {
			return MutationReservation{}, fmt.Errorf("mutation index is inconsistent for key %q", candidate.IdempotencyKey)
		}
		if location.taskID != taskID || !sameMutationRequest(existing, candidate) {
			return MutationReservation{}, fmt.Errorf("idempotency key %q belongs to a different provider mutation", candidate.IdempotencyKey)
		}
		return MutationReservation{Task: m.project(value), Mutation: existing, Created: false}, nil
	}

	current, ok := m.tasks[taskID]
	if !ok {
		return MutationReservation{}, fmt.Errorf("task not found: %s", taskID)
	}
	updated := current
	updated.Mutations = append([]Mutation(nil), current.Mutations...)
	var sequence uint64 = 1
	for _, existing := range updated.Mutations {
		if existing.Sequence >= sequence {
			sequence = existing.Sequence + 1
		}
	}
	now := m.now()
	candidate.ID = m.newID()
	candidate.Sequence = sequence
	candidate.StartedAt = now
	candidate.UpdatedAt = now
	updated.Mutations = append(updated.Mutations, candidate)
	updated.UpdatedAt = now
	if err := m.store.Save(updated); err != nil {
		return MutationReservation{}, err
	}
	m.tasks[taskID] = updated
	m.mutations[candidate.IdempotencyKey] = mutationLocation{taskID: taskID, mutationID: candidate.ID}
	return MutationReservation{Task: m.project(updated), Mutation: candidate, Created: true}, nil
}

// FinishMutation records either provider-confirmed success or an unknown
// outcome. Unknown deliberately retains no provider error text.
func (m *Manager) FinishMutation(taskID, key string, status MutationStatus, result MutationResult, diagnostic string) (Info, Mutation, error) {
	if status != MutationSucceeded && status != MutationUnknown {
		return Info{}, Mutation{}, fmt.Errorf("mutation outcome must be succeeded or unknown")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	location, ok := m.mutations[key]
	if !ok || location.taskID != taskID {
		return Info{}, Mutation{}, fmt.Errorf("mutation not found for key %q", key)
	}
	current := m.tasks[taskID]
	updated := current
	updated.Mutations = append([]Mutation(nil), current.Mutations...)
	var outcome Mutation
	found := false
	for i := range updated.Mutations {
		if updated.Mutations[i].ID != location.mutationID {
			continue
		}
		if updated.Mutations[i].Status == MutationSucceeded {
			return m.project(current), updated.Mutations[i], nil
		}
		mutation := updated.Mutations[i]
		mutation.Status = status
		mutation.Result = MutationResult{}
		mutation.Error = ""
		mutation.Guidance = ""
		if status == MutationSucceeded {
			mutation.Result = result
		} else {
			mutation.Error = boundedMutationString(diagnostic)
			mutation.Guidance = "retry with the same idempotency key to reconcile; no automatic duplicate submission will occur"
		}
		mutation.UpdatedAt = m.now()
		if err := ValidateMutation(mutation); err != nil {
			return Info{}, Mutation{}, err
		}
		updated.Mutations[i] = mutation
		outcome = mutation
		found = true
		break
	}
	if !found {
		return Info{}, Mutation{}, fmt.Errorf("mutation index is inconsistent for key %q", key)
	}
	updated.UpdatedAt = outcome.UpdatedAt
	if err := m.store.Save(updated); err != nil {
		return Info{}, Mutation{}, err
	}
	m.tasks[taskID] = updated
	return m.project(updated), outcome, nil
}

func mutationByID(value Task, id string) (Mutation, bool) {
	for _, mutation := range value.Mutations {
		if mutation.ID == id {
			return mutation, true
		}
	}
	return Mutation{}, false
}

func sameMutationRequest(a, b Mutation) bool {
	return a.IdempotencyKey == b.IdempotencyKey && a.Kind == b.Kind && a.Target == b.Target &&
		a.Actor == b.Actor && a.Request == b.Request
}

func boundedMutationString(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= MaxMutationMessageLength {
		return value
	}
	value = value[:MaxMutationMessageLength]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
