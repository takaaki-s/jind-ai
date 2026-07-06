package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/takaaki-s/jindaiko/internal/exitcode"
	"github.com/takaaki-s/jindaiko/internal/session"
)

// fixture used by most cases; a small pool spanning distinct IDs and descriptions.
func resolveFixture() []session.Info {
	return []session.Info{
		{ID: "aaaa1111-2222-3333-4444-555555555555", Description: "auth refactor"},
		{ID: "bbbb2222-3333-4444-5555-666666666666", Description: "billing"},
		{ID: "cccc3333-4444-5555-6666-777777777777", Description: "TEST harness"},
		{ID: "dddd4444-5555-6666-7777-888888888888", Description: "test-runner"},
	}
}

func TestResolveSelectorFromList(t *testing.T) {
	tests := []struct {
		name      string
		sessions  []session.Info
		selector  string
		wantID    string
		wantCode  int    // 0 = success
		wantSubst string // for error messages
	}{
		{
			name:     "id exact match",
			sessions: resolveFixture(),
			selector: "bbbb2222-3333-4444-5555-666666666666",
			wantID:   "bbbb2222-3333-4444-5555-666666666666",
		},
		{
			name:     "id prefix unique (4 chars)",
			sessions: resolveFixture(),
			selector: "aaaa",
			wantID:   "aaaa1111-2222-3333-4444-555555555555",
		},
		{
			// Selector shorter than idPrefixMinLen skips Stage 2 and falls
			// through to Stage 4, where "au" matches "auth refactor" only.
			name:     "short selector skips id prefix and hits description substring",
			sessions: resolveFixture(),
			selector: "au",
			wantID:   "aaaa1111-2222-3333-4444-555555555555",
		},
		{
			name: "id prefix ambiguous",
			sessions: []session.Info{
				{ID: "cafebabe-1111-2222-3333-444444444444", Description: "one"},
				{ID: "cafefeed-1111-2222-3333-444444444444", Description: "two"},
			},
			selector:  "cafe",
			wantCode:  exitcode.AmbiguousSelector,
			wantSubst: "ambiguous selector 'cafe'",
		},
		{
			name: "description exact unique",
			sessions: []session.Info{
				{ID: "id1", Description: "billing"},
				{ID: "id2", Description: "billing service refactor"},
			},
			selector: "billing",
			wantID:   "id1",
		},
		{
			name: "description exact ambiguous",
			sessions: []session.Info{
				{ID: "id1", Description: "billing"},
				{ID: "id2", Description: "billing"},
			},
			selector: "billing",
			wantCode: exitcode.AmbiguousSelector,
		},
		{
			name:     "description substring case-insensitive unique",
			sessions: resolveFixture(),
			selector: "REFACTOR",
			wantID:   "aaaa1111-2222-3333-4444-555555555555",
		},
		{
			name:     "description substring ambiguous",
			sessions: resolveFixture(),
			selector: "test", // matches "TEST harness" and "test-runner"
			wantCode: exitcode.AmbiguousSelector,
		},
		{
			name:      "not found",
			sessions:  resolveFixture(),
			selector:  "no-such-thing",
			wantCode:  exitcode.SessionNotFound,
			wantSubst: "no session matches selector: no-such-thing",
		},
		{
			name:      "empty selector",
			sessions:  resolveFixture(),
			selector:  "",
			wantCode:  exitcode.GeneralError,
			wantSubst: "selector is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSelectorFromList(tt.sessions, tt.selector)

			if tt.wantCode != 0 {
				if err == nil {
					t.Fatalf("expected error with code %d, got session %+v", tt.wantCode, got)
				}
				var ee *exitcode.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("expected *exitcode.ExitError, got %T: %v", err, err)
				}
				if ee.Code != tt.wantCode {
					t.Errorf("exit code = %d, want %d (message=%q)", ee.Code, tt.wantCode, ee.Message)
				}
				if tt.wantSubst != "" && !strings.Contains(ee.Message, tt.wantSubst) {
					t.Errorf("message %q does not contain %q", ee.Message, tt.wantSubst)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("expected a session, got nil")
				return
			}
			if got.ID != tt.wantID {
				t.Errorf("resolved ID = %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}

// TestResolveSelectorFromList_IDPrefixBeatsDescription verifies Stage 2 short-circuits
// when the ID prefix has a unique hit — description-based stages are not consulted.
func TestResolveSelectorFromList_IDPrefixBeatsDescription(t *testing.T) {
	sessions := []session.Info{
		{ID: "abcdef00-0000-0000-0000-000000000001", Description: "unrelated"},
		{ID: "99999999-0000-0000-0000-000000000002", Description: "abcdef"}, // Stage 4 hit if Stage 2 missed
	}
	got, err := resolveSelectorFromList(sessions, "abcdef")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "abcdef00-0000-0000-0000-000000000001" {
		t.Errorf("expected ID-prefix winner, got %q", got.ID)
	}
}

func TestAmbiguousError_TruncatesCandidates(t *testing.T) {
	var sessions []session.Info
	var hits []int
	for i := 0; i < 15; i++ {
		sessions = append(sessions, session.Info{
			ID:          "id" + string(rune('a'+i)) + "0000000000",
			Description: "match",
		})
		hits = append(hits, i)
	}
	err := ambiguousError("match", sessions, hits)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "... 5 more") {
		t.Errorf("expected truncation marker '... 5 more' in message, got:\n%s", msg)
	}
}
