package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/git"
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

// TestWorktreeTemplate pins where an unset worktree.base_dir lands; see
// worktreeTemplate for why the state dir is the Manager's and not the global.
func TestWorktreeTemplate(t *testing.T) {
	cases := []struct {
		name       string
		cfgBaseDir string
		stateDir   string
		want       string
	}{
		{
			name:       "unset base_dir resolves under the state dir it is given",
			cfgBaseDir: "",
			stateDir:   "/state/jind-ai",
			want:       "/state/jind-ai/worktrees/{name}",
		},
		{
			name:       "a configured base_dir wins over the state dir",
			cfgBaseDir: "/tmp/wt/{name}",
			stateDir:   "/state/jind-ai",
			want:       "/tmp/wt/{name}",
		},
		{
			name:       "no state dir yields a relative template",
			cfgBaseDir: "",
			stateDir:   "",
			want:       "worktrees/{name}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := worktreeTemplate(tc.cfgBaseDir, tc.stateDir)
			if got != tc.want {
				t.Errorf("worktreeTemplate(%q, %q) = %q, want %q",
					tc.cfgBaseDir, tc.stateDir, got, tc.want)
			}
		})
	}
}

// TestWorktreeTemplate_EmptyStateDirIsRejectedDownstream holds the half of the
// contract that spans both functions: an empty state dir must not produce a
// usable path. expandBaseDir has to refuse the relative template, or a worktree
// lands in whatever the working directory happens to be.
//
// Separate from the table above because it asserts about a different function,
// and a row cannot declare that it carries an extra obligation — a later row
// with an absolute base_dir and no state dir would inherit an assertion that is
// wrong for it.
func TestWorktreeTemplate_EmptyStateDirIsRejectedDownstream(t *testing.T) {
	tmpl := worktreeTemplate("", "")
	if _, err := expandBaseDir(tmpl, "jin-abc", "repo"); err == nil {
		t.Errorf("expandBaseDir(%q) accepted a relative template; it must reject it", tmpl)
	}
}

func TestExpandBaseDir(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")

	cases := []struct {
		name         string
		template     string
		worktreeName string
		repoBasename string
		wantPath     string
		wantErr      bool
	}{
		{
			// The default lives in worktreeTemplate, so an empty template
			// arriving here means a caller skipped it.
			name:         "empty template errors instead of defaulting",
			template:     "",
			worktreeName: "jin-abc",
			repoBasename: "myrepo",
			wantErr:      true,
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

// TestProvisionWorktree_PlacesWorktreesUnderTheStateDirTheManagerWasBuiltOver
// pins the wiring, not just the helper: NewManager's stateDir argument has to
// reach the path `git worktree add` is given.
//
// Resolving that path from the process-wide state dir instead meant a Manager
// built over a temp dir still wrote into the developer's own — two directories
// per full test-suite run, and nothing ever failed on the way. Only a test that
// reads the path back holds it.
func TestProvisionWorktree_PlacesWorktreesUnderTheStateDirTheManagerWasBuiltOver(t *testing.T) {
	configMgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	// Distinct from every other temp dir here, so the assertion below can only
	// pass if this specific argument is what the path was built from.
	stateDir := t.TempDir()
	// Built directly rather than through newTestManager because the state dir
	// is the subject. hookRunner stays nil, which skips the post-create hook —
	// provisionWorktree gets to `worktree add` and returns.
	mgr, err := NewManager(t.TempDir(), stateDir, testIdentity(), configMgr)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	runner := hookHappyPathGitRunner()
	mgr.SetGitClient(git.NewClientWithRunner(runner))

	if _, err := mgr.provisionWorktree("3f9a2b4c-1111-2222-3333-444444444444", CreateOptions{
		WorkDir:  repo,
		Worktree: true,
	}); err != nil {
		t.Fatalf("provisionWorktree: %v", err)
	}

	// `git worktree add -b <branch> <path> <baseRef>`
	addCall := runner.findCall("worktree", "add")
	if addCall == nil {
		t.Fatal("no `git worktree add` call recorded")
	}
	want := filepath.Join(stateDir, "worktrees", "jin-3f9a2b4c")
	if addCall[4] != want {
		t.Errorf("`git worktree add` path = %q, want %q", addCall[4], want)
	}
	if addCall[5] != testReviewBaseOID {
		t.Errorf("`git worktree add` base = %q, want immutable OID %q", addCall[5], testReviewBaseOID)
	}
}
