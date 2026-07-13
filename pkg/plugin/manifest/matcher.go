package manifest

import (
	"fmt"
	"strings"
)

// EventStatusChanged is the only event a matcher may target in the current
// schema; a matcher is either that name alone or that name followed by a
// specific status ("status_changed:idle").
const EventStatusChanged = "status_changed"

// ValidateMatcher reports whether an `on:` entry is well-formed. A matcher is
// either "status_changed" (every status_changed event) or
// "status_changed:<status>" (only that status), where <status> is non-empty.
func ValidateMatcher(matcher string) error {
	name, status, hasStatus := strings.Cut(matcher, ":")
	if name != EventStatusChanged {
		return fmt.Errorf("unknown event %q (only %q is supported)", name, EventStatusChanged)
	}
	if hasStatus && status == "" {
		return fmt.Errorf("matcher %q has an empty status after ':'", matcher)
	}
	return nil
}

// MatcherMatches reports whether a matcher selects the given event and status.
// A bare "status_changed" matches any status; "status_changed:<status>"
// matches only when status equals the suffix. Callers pass matchers that have
// already passed ValidateMatcher.
func MatcherMatches(matcher, event, status string) bool {
	name, want, hasStatus := strings.Cut(matcher, ":")
	if name != event {
		return false
	}
	if !hasStatus {
		return true
	}
	return want == status
}
