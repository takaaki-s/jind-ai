package cmd

import (
	"fmt"
	"strings"

	"github.com/takaaki-s/jindaiko/internal/daemon"
	"github.com/takaaki-s/jindaiko/internal/exitcode"
	"github.com/takaaki-s/jindaiko/internal/session"
)

// idPrefixMinLen is the minimum selector length required for ID-prefix
// matching (Stage 2). Shorter selectors skip Stage 2 and fall through to
// description-based stages.
const idPrefixMinLen = 4

// ambiguousCandidateLimit caps the number of candidates listed in an
// ambiguous-selector error message.
const ambiguousCandidateLimit = 10

// resolveSelector resolves a user-facing selector to a single session.
//
// Resolution order:
//  1. ID exact match
//  2. ID prefix (>= 4 chars, unique match)
//  3. Description exact match (unique)
//  4. Description substring match, case-insensitive (unique)
//
// An empty selector returns GeneralError. Multiple matches at any stage
// return AmbiguousSelector with a candidate listing. No match at any stage
// returns SessionNotFound.
func resolveSelector(client *daemon.Client, selector string) (*session.Info, error) {
	if selector == "" {
		return nil, exitcode.Errorf(exitcode.GeneralError, "selector is required")
	}
	sessions, err := client.List()
	if err != nil {
		return nil, err
	}
	return resolveSelectorFromList(sessions, selector)
}

// resolveSelectorFromList is the pure resolution routine backing resolveSelector.
// It is exported to package-internal tests so the four-stage logic can be
// exercised without a live daemon.
func resolveSelectorFromList(sessions []session.Info, selector string) (*session.Info, error) {
	if selector == "" {
		return nil, exitcode.Errorf(exitcode.GeneralError, "selector is required")
	}

	// Stage 1: ID exact.
	for i := range sessions {
		if sessions[i].ID == selector {
			return &sessions[i], nil
		}
	}

	// Stage 2: ID prefix (only when the selector is long enough to be
	// unambiguous in intent — a 2-char selector is more likely a description
	// substring than a UUID prefix).
	if len(selector) >= idPrefixMinLen {
		var hits []int
		for i := range sessions {
			if strings.HasPrefix(sessions[i].ID, selector) {
				hits = append(hits, i)
			}
		}
		if len(hits) == 1 {
			return &sessions[hits[0]], nil
		}
		if len(hits) > 1 {
			return nil, ambiguousError(selector, sessions, hits)
		}
	}

	// Stage 3: Description exact.
	var exact []int
	for i := range sessions {
		if sessions[i].Description == selector {
			exact = append(exact, i)
		}
	}
	if len(exact) == 1 {
		return &sessions[exact[0]], nil
	}
	if len(exact) > 1 {
		return nil, ambiguousError(selector, sessions, exact)
	}

	// Stage 4: Description substring, case-insensitive.
	needle := strings.ToLower(selector)
	var sub []int
	for i := range sessions {
		if strings.Contains(strings.ToLower(sessions[i].Description), needle) {
			sub = append(sub, i)
		}
	}
	if len(sub) == 1 {
		return &sessions[sub[0]], nil
	}
	if len(sub) > 1 {
		return nil, ambiguousError(selector, sessions, sub)
	}

	return nil, exitcode.Errorf(exitcode.SessionNotFound, "no session matches selector: %s", selector)
}

// ambiguousError formats the AmbiguousSelector error with up to
// ambiguousCandidateLimit candidates, followed by "... N more" when
// truncated.
func ambiguousError(selector string, sessions []session.Info, hits []int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous selector '%s'; candidates:\n", selector)
	for i, idx := range hits {
		if i >= ambiguousCandidateLimit {
			fmt.Fprintf(&b, "  ... %d more\n", len(hits)-ambiguousCandidateLimit)
			break
		}
		fmt.Fprintf(&b, "  %s  %s\n", shortID(sessions[idx].ID), sessions[idx].Description)
	}
	return exitcode.Errorf(exitcode.AmbiguousSelector, "%s", b.String())
}

// shortID truncates a session ID to at most 8 characters for display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
