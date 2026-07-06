package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jindaiko/internal/paths"
	"github.com/takaaki-s/jindaiko/internal/worktreehook"
)

// setupWorktreeTest isolates state for one test: a private XDG_STATE_HOME
// (so the allowlist file lives under t.TempDir()) and a fresh repo directory
// with an optional .jin/worktree-post-create.sh.
func setupWorktreeTest(t *testing.T, scriptBody string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := t.TempDir()
	if scriptBody != "" {
		writeHookScript(t, repoDir, scriptBody)
	}
	return repoDir
}

func writeHookScript(t *testing.T, repoDir, body string) {
	t.Helper()
	scriptDir := filepath.Join(repoDir, ".jin")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir .jin: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "worktree-post-create.sh")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
}

// runWorktreeCmd invokes the worktree subcommand tree with args and captured
// I/O. It resets the --yes flag before every run so cross-test state does not
// leak (cobra retains flag values between Execute calls).
//
// Args are set on rootCmd because cobra's Execute always delegates to root;
// calling SetArgs on a child has no effect when the child has a parent.
func runWorktreeCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	_ = worktreeAllowCmd.Flags().Set("yes", "false")

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetIn(strings.NewReader(stdin))
	rootCmd.SetArgs(append([]string{"worktree"}, args...))
	err := rootCmd.Execute()
	return buf.String(), err
}

func TestWorktreeAllow_Yes(t *testing.T) {
	repoDir := setupWorktreeTest(t, "#!/usr/bin/env bash\necho hi\n")

	out, err := runWorktreeCmd(t, "", "allow", "--yes", repoDir)
	if err != nil {
		t.Fatalf("allow --yes: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Allowed:") {
		t.Errorf("expected 'Allowed:' in output, got %q", out)
	}

	al, err := worktreehook.LoadAllowlist(paths.State())
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}
	entry, ok := al.Get(repoDir)
	if !ok {
		t.Fatalf("expected repo %s in allowlist", repoDir)
	}
	if entry.SHA256 == "" {
		t.Errorf("expected non-empty SHA256, got %q", entry.SHA256)
	}
	if entry.AllowedAt.IsZero() {
		t.Errorf("expected non-zero AllowedAt")
	}
}

func TestWorktreeAllow_NoScript(t *testing.T) {
	repoDir := setupWorktreeTest(t, "")

	_, err := runWorktreeCmd(t, "", "allow", "--yes", repoDir)
	if err == nil {
		t.Fatalf("expected error when script is missing, got nil")
	}
	if !strings.Contains(err.Error(), "no hook script") {
		t.Errorf("expected 'no hook script' in error, got %q", err.Error())
	}
}

func TestWorktreeStatus_NoScript(t *testing.T) {
	repoDir := setupWorktreeTest(t, "")

	out, err := runWorktreeCmd(t, "", "status", repoDir)
	if err != nil {
		t.Fatalf("status: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "No hook script.") {
		t.Errorf("expected 'No hook script.', got %q", out)
	}
}

func TestWorktreeStatus_NotAllowed(t *testing.T) {
	repoDir := setupWorktreeTest(t, "echo hi\n")

	out, err := runWorktreeCmd(t, "", "status", repoDir)
	if err != nil {
		t.Fatalf("status: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Not allowed.") {
		t.Errorf("expected 'Not allowed.', got %q", out)
	}
}

func TestWorktreeStatus_Allowed(t *testing.T) {
	repoDir := setupWorktreeTest(t, "echo hi\n")

	if _, err := runWorktreeCmd(t, "", "allow", "--yes", repoDir); err != nil {
		t.Fatalf("allow: %v", err)
	}

	out, err := runWorktreeCmd(t, "", "status", repoDir)
	if err != nil {
		t.Fatalf("status: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Allowed (since") {
		t.Errorf("expected 'Allowed (since', got %q", out)
	}
}

func TestWorktreeStatus_Changed(t *testing.T) {
	repoDir := setupWorktreeTest(t, "echo original\n")

	if _, err := runWorktreeCmd(t, "", "allow", "--yes", repoDir); err != nil {
		t.Fatalf("allow: %v", err)
	}

	writeHookScript(t, repoDir, "echo modified\n")

	out, err := runWorktreeCmd(t, "", "status", repoDir)
	if err != nil {
		t.Fatalf("status: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Script changed since") {
		t.Errorf("expected 'Script changed since', got %q", out)
	}
}

func TestWorktreeRevoke(t *testing.T) {
	repoDir := setupWorktreeTest(t, "echo hi\n")

	if _, err := runWorktreeCmd(t, "", "allow", "--yes", repoDir); err != nil {
		t.Fatalf("allow: %v", err)
	}

	out, err := runWorktreeCmd(t, "", "revoke", repoDir)
	if err != nil {
		t.Fatalf("revoke: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "Revoked:") {
		t.Errorf("expected 'Revoked:', got %q", out)
	}

	statusOut, err := runWorktreeCmd(t, "", "status", repoDir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(statusOut, "Not allowed.") {
		t.Errorf("expected 'Not allowed.' after revoke, got %q", statusOut)
	}
}

func TestWorktreeList_Empty(t *testing.T) {
	_ = setupWorktreeTest(t, "")

	out, err := runWorktreeCmd(t, "", "list")
	if err != nil {
		t.Fatalf("list: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "No repositories in allowlist.") {
		t.Errorf("expected 'No repositories in allowlist.', got %q", out)
	}
}

func TestWorktreeList_WithEntries(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	repoA := t.TempDir()
	writeHookScript(t, repoA, "echo a\n")
	repoB := t.TempDir()
	writeHookScript(t, repoB, "echo b\n")

	if _, err := runWorktreeCmd(t, "", "allow", "--yes", repoA); err != nil {
		t.Fatalf("allow A: %v", err)
	}
	if _, err := runWorktreeCmd(t, "", "allow", "--yes", repoB); err != nil {
		t.Fatalf("allow B: %v", err)
	}

	out, err := runWorktreeCmd(t, "", "list")
	if err != nil {
		t.Fatalf("list: err=%v, out=%q", err, out)
	}
	if !strings.Contains(out, "PATH") || !strings.Contains(out, "ALLOWED_AT") {
		t.Errorf("expected header row in list output, got %q", out)
	}
	if !strings.Contains(out, repoA) {
		t.Errorf("expected repoA %s in list output, got %q", repoA, out)
	}
	if !strings.Contains(out, repoB) {
		t.Errorf("expected repoB %s in list output, got %q", repoB, out)
	}
}
