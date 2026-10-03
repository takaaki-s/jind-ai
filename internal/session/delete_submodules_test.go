package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteInitializedSubmodulesRequiresConsent(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "dirty"}[dirty], func(t *testing.T) {
			m, id, _, _ := checkReceiptFixture(t)
			info, _ := m.GetInfo(id)
			sub, _ := reviewTestRepo(t)
			runReviewTestGit(t, info.WorkDir, "-c", "protocol.file.allow=always", "submodule", "add", sub, "sub")
			runReviewTestGit(t, info.WorkDir, "add", ".")
			runReviewTestGit(t, info.WorkDir, "commit", "-m", "add submodule")
			runReviewTestGit(t, info.WorkDir, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
			if dirty {
				if err := os.WriteFile(filepath.Join(info.WorkDir, "sub", "unsaved.txt"), []byte("keep me\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command("git", "-C", info.WorkDir, "worktree", "remove", "--", info.WorkDir).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "submodules") {
				t.Fatalf("expected Git submodule refusal: %s, %v", out, err)
			}
			if err := m.Delete(id, true, false); !errors.Is(err, ErrWorktreeSubmodules) {
				t.Fatalf("without consent: %v", err)
			}
			if _, err := os.Stat(filepath.Join(info.WorkDir, "sub", "base.txt")); err != nil {
				t.Fatal("pre-check removed submodule")
			}
			if dirty {
				if _, err := os.Stat(filepath.Join(info.WorkDir, "sub", "unsaved.txt")); err != nil {
					t.Fatal("lost unsaved file")
				}
			}
			if err := m.Delete(id, true, true); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(info.WorkDir); !os.IsNotExist(err) {
				t.Fatalf("worktree still exists: %v", err)
			}
			if _, ok := m.GetInfo(id); ok {
				t.Fatal("session remains")
			}
			if _, err := os.Stat(filepath.Join(sub, "base.txt")); err != nil {
				t.Fatal("source repository was changed")
			}
		})
	}
}
