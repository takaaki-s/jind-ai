package manifest

import "testing"

func TestValidateMatcher(t *testing.T) {
	valid := []string{"status_changed", "status_changed:idle", "status_changed:permission"}
	for _, m := range valid {
		if err := ValidateMatcher(m); err != nil {
			t.Errorf("ValidateMatcher(%q) = %v, want nil", m, err)
		}
	}
	invalid := []string{"", "status_changed:", "file_changed"}
	for _, m := range invalid {
		if err := ValidateMatcher(m); err == nil {
			t.Errorf("ValidateMatcher(%q) = nil, want error", m)
		}
	}
}

func TestMatcherMatches(t *testing.T) {
	tests := []struct {
		matcher string
		event   string
		status  string
		want    bool
	}{
		{"status_changed", "status_changed", "idle", true},
		{"status_changed", "status_changed", "permission", true},
		{"status_changed:permission", "status_changed", "permission", true},
		{"status_changed:permission", "status_changed", "idle", false},
		{"status_changed:idle", "status_changed", "idle", true},
		{"status_changed", "other_event", "idle", false},
	}
	for _, tt := range tests {
		if got := MatcherMatches(tt.matcher, tt.event, tt.status); got != tt.want {
			t.Errorf("MatcherMatches(%q, %q, %q) = %v, want %v",
				tt.matcher, tt.event, tt.status, got, tt.want)
		}
	}
}
