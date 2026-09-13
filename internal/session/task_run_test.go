package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultWorktreeIdentityIsDeterministic(t *testing.T) {
	got := DefaultWorktreeIdentity("12345678-1234-4234-8234-123456789abc", "task/")
	if got.Name != "jin-12345678" || got.Branch != "task/12345678" || got.Path != "" {
		t.Fatalf("identity = %+v", got)
	}
}

func TestReserveCreationAcceptsCanonicalReservedIDOnce(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	id := "12345678-1234-4234-8234-123456789abc"
	opts := CreateOptions{ReservedID: id, WorkDir: t.TempDir(), Description: "reserved"}
	sess, info, err := mgr.ReserveCreation(opts)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != id || info.ID != id {
		t.Fatalf("reserved identities: session=%s info=%s", sess.ID, info.ID)
	}
	if _, _, err := mgr.ReserveCreation(opts); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, _, err := mgr.ReserveCreation(CreateOptions{ReservedID: "not-a-uuid", WorkDir: t.TempDir()}); err == nil {
		t.Fatal("invalid reserved ID was accepted")
	}
}

func TestSetInitialWorkDirPersistsContainedDirectory(t *testing.T) {
	sessionsDir := t.TempDir()
	mgr, _, _ := newTestManagerIn(t, sessionsDir, testIdentity())
	root := t.TempDir()
	subdir := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: root, Description: "subdir"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetInitialWorkDir(sess.ID, filepath.Join("services", "api")); err != nil {
		t.Fatal(err)
	}
	info, ok := mgr.GetInfo(sess.ID)
	if !ok || info.CurrentWorkDir != subdir {
		t.Fatalf("current workdir = %q", info.CurrentWorkDir)
	}

	restarted, _, _ := newTestManagerIn(t, sessionsDir, testIdentity())
	persisted, ok := restarted.GetInfo(sess.ID)
	if !ok || persisted.CurrentWorkDir != subdir {
		t.Fatalf("persisted current workdir = %q", persisted.CurrentWorkDir)
	}
}

func TestSetInitialWorkDirRejectsEscapeAndNonDirectory(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: root, Description: "bounds"})
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"../escape", "outside", "file", "missing"} {
		if err := mgr.SetInitialWorkDir(sess.ID, relative); err == nil {
			t.Fatalf("accepted workdir %q", relative)
		}
	}
	info, _ := mgr.GetInfo(sess.ID)
	if info.CurrentWorkDir != "" {
		t.Fatalf("failed attempts changed workdir to %q", info.CurrentWorkDir)
	}
}
