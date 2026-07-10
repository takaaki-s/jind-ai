package session

import (
	"strings"
	"testing"
)

func TestDeriveWorktreeName(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		override  string
		want      string
	}{
		{"override wins", "3f9a2b4c-1111-2222-3333-444444444444", "custom-wt", "custom-wt"},
		{"no override, canonical UUID", "3f9a2b4c-1111-2222-3333-444444444444", "", "jin-3f9a2b4c"},
		{"session id shorter than 8 chars", "abc", "", "jin-abc"},
		{"exactly 8 char id", "12345678", "", "jin-12345678"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveWorktreeName(tc.sessionID, tc.override); got != tc.want {
				t.Errorf("deriveWorktreeName(%q, %q) = %q, want %q",
					tc.sessionID, tc.override, got, tc.want)
			}
		})
	}
}

func TestDeriveBranchName(t *testing.T) {
	cases := []struct {
		name         string
		worktreeName string
		prefix       string
		override     string
		want         string
	}{
		{"default prefix strips jin-", "jin-3f9a2b4c", "jin/", "", "jin/3f9a2b4c"},
		{"custom prefix strips jin-", "jin-3f9a2b4c", "topic/", "", "topic/3f9a2b4c"},
		{"empty prefix strips jin-", "jin-3f9a2b4c", "", "", "3f9a2b4c"},
		{"non jin- worktree name preserved", "custom-wt", "jin/", "", "jin/custom-wt"},
		{"override wins", "jin-3f9a2b4c", "jin/", "feat/xyz", "feat/xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveBranchName(tc.worktreeName, tc.prefix, tc.override); got != tc.want {
				t.Errorf("deriveBranchName(%q, %q, %q) = %q, want %q",
					tc.worktreeName, tc.prefix, tc.override, got, tc.want)
			}
		})
	}
}

func TestExpandBaseDir(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("XDG_STATE_HOME", "/state")

	cases := []struct {
		name         string
		template     string
		worktreeName string
		repoBasename string
		wantPath     string
		wantErr      bool
	}{
		{
			name:         "empty template uses XDG_STATE_HOME + worktrees/{name}",
			template:     "",
			worktreeName: "jin-abc",
			repoBasename: "myrepo",
			wantPath:     "/state/jind-ai/worktrees/jin-abc",
		},
		{
			name:         "explicit {name} substitution",
			template:     "/tmp/wt/{name}",
			worktreeName: "jin-xyz",
			repoBasename: "myrepo",
			wantPath:     "/tmp/wt/jin-xyz",
		},
		{
			name:         "explicit {repo}/{name} substitution",
			template:     "/tmp/{repo}/{name}",
			worktreeName: "wt1",
			repoBasename: "myrepo",
			wantPath:     "/tmp/myrepo/wt1",
		},
		{
			name:         "env var expansion",
			template:     "${HOME}/.wt/{name}",
			worktreeName: "wt1",
			repoBasename: "r",
			wantPath:     "/home/testuser/.wt/wt1",
		},
		{
			name:         "unknown template variable errors",
			template:     "/tmp/{unknown}/{name}",
			worktreeName: "wt1",
			repoBasename: "r",
			wantErr:      true,
		},
		{
			name:         "relative path errors",
			template:     "relative/{name}",
			worktreeName: "wt1",
			repoBasename: "r",
			wantErr:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandBaseDir(tc.template, tc.worktreeName, tc.repoBasename)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expandBaseDir(%q) expected error, got %q", tc.template, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandBaseDir(%q) unexpected error: %v", tc.template, err)
			}
			if got != tc.wantPath {
				t.Errorf("expandBaseDir(%q) = %q, want %q", tc.template, got, tc.wantPath)
			}
		})
	}
}

func TestFindAvailableWorktreeName_NoCollision(t *testing.T) {
	got, err := findAvailableWorktreeName("jin-abc", func(string) bool { return false })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "jin-abc" {
		t.Errorf("got %q, want %q", got, "jin-abc")
	}
}

func TestFindAvailableWorktreeName_FirstCollisionSuffixed(t *testing.T) {
	taken := map[string]bool{"jin-abc": true}
	got, err := findAvailableWorktreeName("jin-abc", func(c string) bool { return taken[c] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "jin-abc-2" {
		t.Errorf("got %q, want %q", got, "jin-abc-2")
	}
}

func TestFindAvailableWorktreeName_ExhaustsAllAttempts(t *testing.T) {
	_, err := findAvailableWorktreeName("jin-abc", func(string) bool { return true })
	if err == nil {
		t.Fatal("expected error when every candidate collides")
	}
	if !strings.Contains(err.Error(), "jin-abc") {
		t.Errorf("error message %q should mention base name", err.Error())
	}
}

func TestFindAvailableWorktreeName_ThirdSuffix(t *testing.T) {
	taken := map[string]bool{
		"jin-abc":   true,
		"jin-abc-2": true,
	}
	got, err := findAvailableWorktreeName("jin-abc", func(c string) bool { return taken[c] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "jin-abc-3" {
		t.Errorf("got %q, want %q", got, "jin-abc-3")
	}
}
