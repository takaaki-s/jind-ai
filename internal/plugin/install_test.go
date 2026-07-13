package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

const validSource = `schema_version: 1
name: notifier
version: 0.1.0
description: notifier test fixture
jin: ">=0.0.0"
install:
  source:
    build:
      - "true"
    entrypoint: ./notify.sh
on:
  - status_changed
`

const testBuildTimeout = 30 * time.Second

// writeManifest writes content as the plugin manifest into a fresh temp dir.
// Runtime tests use this for the linked-install path so they can hand Link a
// self-contained directory.
func writeManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

// lockHas reports whether stateDir's lock currently records name.
func lockHas(t *testing.T, stateDir, name string) bool {
	t.Helper()
	lock, err := LoadLock(stateDir)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	_, ok := lock.Get(name)
	return ok
}

func TestLink_Success(t *testing.T) {
	src := writeManifest(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	m, err := Link(src, pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if m.Name != "notifier" {
		t.Errorf("Name = %q, want notifier", m.Name)
	}

	dest := filepath.Join(pluginsDir, "notifier")
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("lstat dest: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("dest mode = %v, want a symlink", fi.Mode())
	}
	target, err := os.Readlink(dest)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != src {
		t.Errorf("symlink target = %q, want %q", target, src)
	}

	lock, err := LoadLock(stateDir)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	entry, ok := lock.Get("notifier")
	if !ok {
		t.Fatal("lock entry missing after Link")
	}
	if !entry.Linked {
		t.Error("entry.Linked = false, want true")
	}
	if entry.Source != src {
		t.Errorf("entry.Source = %q, want %q", entry.Source, src)
	}
	if entry.Commit != "" {
		t.Errorf("entry.Commit = %q, want empty for a linked plugin", entry.Commit)
	}
	if entry.InstalledAt.IsZero() {
		t.Error("entry.InstalledAt is zero")
	}
}

func TestLink_RejectsInvalidManifest(t *testing.T) {
	// name missing
	src := writeManifest(t, `schema_version: 1
version: 0.1.0
description: no name
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: ./run.sh
`)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	if _, err := Link(src, pluginsDir, stateDir); err == nil {
		t.Fatal("Link with invalid manifest: want error, got nil")
	}
	if lock, _ := LoadLock(stateDir); len(lock.All()) != 0 {
		t.Error("Link must not write a lock entry when the manifest is invalid")
	}
}

func TestLink_RejectsIncompatibleJinRange(t *testing.T) {
	// Impossible constraint against any real jin version; dev builds skip
	// the check via CheckJinCompat, so pin an explicit Version to force
	// the incompatibility path.
	restore := setJinVersionForTest(t, "0.5.0")
	defer restore()

	src := writeManifest(t, `schema_version: 1
name: notifier
version: 0.1.0
description: needs jin 99+
jin: ">=99.0.0"
install:
  source:
    build: ["true"]
    entrypoint: ./run.sh
on:
  - status_changed
`)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	if _, err := Link(src, pluginsDir, stateDir); err == nil {
		t.Fatal("Link with unsatisfied jin range: want error, got nil")
	}
	if _, err := os.Lstat(filepath.Join(pluginsDir, "notifier")); !os.IsNotExist(err) {
		t.Error("Link must not create a symlink when jin range is unsatisfied")
	}
}

func TestLink_RejectsDoubleInstall(t *testing.T) {
	src := writeManifest(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	if _, err := Link(src, pluginsDir, stateDir); err != nil {
		t.Fatalf("first Link: %v", err)
	}
	if _, err := Link(src, pluginsDir, stateDir); err == nil {
		t.Fatal("second Link: want error, got nil")
	}
}

func TestRemove_LinkedKeepsSource(t *testing.T) {
	src := writeManifest(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	if _, err := Link(src, pluginsDir, stateDir); err != nil {
		t.Fatalf("Link: %v", err)
	}

	if err := Remove("notifier", pluginsDir, stateDir); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(pluginsDir, "notifier")); !os.IsNotExist(err) {
		t.Error("symlink should be removed")
	}
	if _, err := os.Stat(filepath.Join(src, manifest.Filename)); err != nil {
		t.Errorf("removing a linked plugin must not touch the source tree: %v", err)
	}
	if lockHas(t, stateDir, "notifier") {
		t.Error("lock entry should be removed")
	}
}

func TestRemove_NonLinkedDirRemoved(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	name := "cloned"

	dir := filepath.Join(pluginsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.Filename), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	lock, _ := LoadLock(stateDir)
	if err := lock.Set(name, LockEntry{Source: "github.com/owner/cloned", InstalledAt: time.Now()}); err != nil {
		t.Fatalf("lock Set: %v", err)
	}

	if err := Remove(name, pluginsDir, stateDir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("non-linked plugin directory should be removed")
	}
	if lockHas(t, stateDir, name) {
		t.Error("lock entry should be removed")
	}
}

func TestRemove_NotInstalledErrors(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	if err := Remove("ghost", pluginsDir, stateDir); err == nil {
		t.Fatal("Remove of unknown plugin: want error, got nil")
	}
}

func TestRemove_MissingDirStillClearsLock(t *testing.T) {
	src := writeManifest(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	if _, err := Link(src, pluginsDir, stateDir); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := os.Remove(filepath.Join(pluginsDir, "notifier")); err != nil {
		t.Fatalf("out-of-band remove: %v", err)
	}

	if err := Remove("notifier", pluginsDir, stateDir); err != nil {
		t.Fatalf("Remove with missing dir: %v", err)
	}
	if lockHas(t, stateDir, "notifier") {
		t.Error("lock entry should be cleared even when the directory is gone")
	}
}

func TestRemove_RejectsTraversalName(t *testing.T) {
	base := t.TempDir()
	pluginsDir := filepath.Join(base, "plugins")
	victim := filepath.Join(base, "victim")
	for _, dir := range []string{pluginsDir, victim} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"../victim", "../../etc", "a/b", "..", "."} {
		if err := Remove(name, pluginsDir, t.TempDir()); err == nil {
			t.Errorf("Remove(%q): want error, got nil", name)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("sibling directory must survive traversal attempts: %v", err)
	}
}

func TestRemove_OrphanDirWithoutLock(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	// A directory with no lock entry: the state left behind when an install's
	// lock write fails after the rename. Remove must reclaim it.
	orphan := filepath.Join(pluginsDir, "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Remove("orphan", pluginsDir, stateDir); err != nil {
		t.Fatalf("Remove of orphan dir: %v", err)
	}
	if _, err := os.Lstat(orphan); err == nil {
		t.Error("orphan directory should be removed")
	}
}

func TestRemove_OrphanSymlinkKeepsSource(t *testing.T) {
	src := writeManifest(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	// An orphan symlink must be unlinked without following it.
	if err := os.Symlink(src, filepath.Join(pluginsDir, "orphan")); err != nil {
		t.Fatal(err)
	}

	if err := Remove("orphan", pluginsDir, stateDir); err != nil {
		t.Fatalf("Remove of orphan symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(pluginsDir, "orphan")); err == nil {
		t.Error("orphan symlink should be removed")
	}
	if _, err := os.Stat(filepath.Join(src, manifest.Filename)); err != nil {
		t.Errorf("link source must survive orphan removal: %v", err)
	}
}

func TestParseSource(t *testing.T) {
	tests := []struct {
		arg     string
		wantURL string
		wantRef string
		wantErr bool
	}{
		{arg: "github.com/o/r", wantURL: "https://github.com/o/r"},
		{arg: "github.com/o/r@v1.2.0", wantURL: "https://github.com/o/r", wantRef: "v1.2.0"},
		{arg: "https://gitlab.com/o/r@main", wantURL: "https://gitlab.com/o/r", wantRef: "main"},
		{arg: "git@github.com:o/r.git", wantURL: "git@github.com:o/r.git"},
		{arg: "git@github.com:o/r.git@v2", wantURL: "git@github.com:o/r.git", wantRef: "v2"},
		{arg: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			s, err := ParseSource(tt.arg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSource(%q): want error, got %+v", tt.arg, s)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSource(%q): %v", tt.arg, err)
			}
			if s.Raw != tt.arg {
				t.Errorf("Raw = %q, want %q", s.Raw, tt.arg)
			}
			if s.CloneURL != tt.wantURL {
				t.Errorf("CloneURL = %q, want %q", s.CloneURL, tt.wantURL)
			}
			if s.Ref != tt.wantRef {
				t.Errorf("Ref = %q, want %q", s.Ref, tt.wantRef)
			}
		})
	}
}

// runGit runs a git subcommand in dir with a hermetic environment (no global or
// system config, no credential prompts) so fixtures behave the same on any CI
// host. It fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// initRepo creates a non-bare git repo in a temp dir with body committed as the
// manifest, and returns its path. Callers can add more commits/tags afterwards.
func initRepo(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeInto(t, filepath.Join(dir, manifest.Filename), body)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func writeInto(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fileSource builds a Source that clones repoDir over the file:// transport. A
// file URL is used (not a bare path) so FetchUpdate, which re-parses the locked
// Source, keeps it as a clone URL instead of prefixing https://.
func fileSource(t *testing.T, repoDir, ref string) Source {
	t.Helper()
	raw := "file://" + repoDir
	if ref != "" {
		raw += "@" + ref
	}
	s, err := ParseSource(raw)
	if err != nil {
		t.Fatalf("ParseSource(%q): %v", raw, err)
	}
	return s
}

// assertNoScratch fails if any staging or parked-update directory lingers under
// pluginsDir, proving fetch/commit clean up after themselves.
func assertNoScratch(t *testing.T, pluginsDir string) {
	t.Helper()
	for _, glob := range []string{".staging-*", ".old-*"} {
		matches, err := filepath.Glob(filepath.Join(pluginsDir, glob))
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		if len(matches) != 0 {
			t.Errorf("leftover scratch dirs %v under %s", matches, pluginsDir)
		}
	}
}

func TestFetchCommit_Success(t *testing.T) {
	repo := initRepo(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	plan, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if plan.Manifest().Name != "notifier" {
		t.Errorf("Manifest name = %q, want notifier", plan.Manifest().Name)
	}
	if plan.CommitSHA() == "" {
		t.Error("CommitSHA is empty")
	}
	if plan.PrevCommitSHA() != "" {
		t.Errorf("PrevCommitSHA = %q, want empty for a fresh install", plan.PrevCommitSHA())
	}

	if err := plan.Commit(testBuildTimeout); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, err := os.Stat(filepath.Join(pluginsDir, "notifier", manifest.Filename)); err != nil {
		t.Errorf("plugin not placed: %v", err)
	}
	assertNoScratch(t, pluginsDir)

	lock, _ := LoadLock(stateDir)
	entry, ok := lock.Get("notifier")
	if !ok {
		t.Fatal("lock entry missing after Commit")
	}
	if entry.Commit != plan.CommitSHA() {
		t.Errorf("lock Commit = %q, want %q", entry.Commit, plan.CommitSHA())
	}
	if entry.Linked {
		t.Error("cloned plugin should not be marked linked")
	}
	if entry.Source != "file://"+repo {
		t.Errorf("lock Source = %q, want %q", entry.Source, "file://"+repo)
	}
}

func TestFetch_CheckoutTag(t *testing.T) {
	repo := initRepo(t, validSource)
	runGit(t, repo, "tag", "v1.0.0")
	writeInto(t, filepath.Join(repo, "post-tag.txt"), "later")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "second")

	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	plan, err := Fetch(fileSource(t, repo, "v1.0.0"), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := plan.Commit(testBuildTimeout); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, "notifier", "post-tag.txt")); !os.IsNotExist(err) {
		t.Error("checkout of v1.0.0 must not include the post-tag file")
	}
}

func TestFetch_RejectsInvalidManifest(t *testing.T) {
	// name missing
	repo := initRepo(t, `schema_version: 1
version: 0.1.0
description: no name
jin: ">=0.0.0"
install:
  source:
    build: ["true"]
    entrypoint: ./run.sh
`)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	if _, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir); err == nil {
		t.Fatal("Fetch with invalid manifest: want error, got nil")
	}
	assertNoScratch(t, pluginsDir)
}

func TestFetch_RejectsIncompatibleJinRange(t *testing.T) {
	restore := setJinVersionForTest(t, "0.5.0")
	defer restore()

	repo := initRepo(t, `schema_version: 1
name: notifier
version: 0.1.0
description: needs jin 99
jin: ">=99.0.0"
install:
  source:
    build: ["true"]
    entrypoint: ./run.sh
on:
  - status_changed
`)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	if _, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir); err == nil {
		t.Fatal("Fetch with unsatisfied jin range: want error, got nil")
	}
	if _, err := os.Lstat(filepath.Join(pluginsDir, "notifier")); !os.IsNotExist(err) {
		t.Error("incompatible plugin must not be placed")
	}
	assertNoScratch(t, pluginsDir)
}

func TestFetch_RejectsDoubleInstall(t *testing.T) {
	repo := initRepo(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	plan, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if err := plan.Commit(testBuildTimeout); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir); err == nil {
		t.Fatal("second Fetch of the same name: want error, got nil")
	}
	assertNoScratch(t, pluginsDir)
}

func TestCommit_RunsBuild(t *testing.T) {
	repo := initRepo(t, `schema_version: 1
name: notifier
version: 0.1.0
description: build artifact
jin: ">=0.0.0"
install:
  source:
    build:
      - echo built > artifact.txt
    entrypoint: ./run.sh
on:
  - status_changed
`)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	plan, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := plan.Commit(testBuildTimeout); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, "notifier", "artifact.txt")); err != nil {
		t.Errorf("build artifact missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "plugin-logs", "notifier-build.log")); err != nil {
		t.Errorf("build log missing: %v", err)
	}
}

func TestCommit_BuildFailureRollsBack(t *testing.T) {
	repo := initRepo(t, `schema_version: 1
name: notifier
version: 0.1.0
description: build fails
jin: ">=0.0.0"
install:
  source:
    build:
      - exit 1
    entrypoint: ./run.sh
on:
  - status_changed
`)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	plan, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := plan.Commit(testBuildTimeout); err == nil {
		t.Fatal("Commit with failing build: want error, got nil")
	}
	if _, err := os.Lstat(filepath.Join(pluginsDir, "notifier")); !os.IsNotExist(err) {
		t.Error("plugin must not be placed when the build fails")
	}
	if lockHas(t, stateDir, "notifier") {
		t.Error("lock must not record a plugin whose build failed")
	}
	assertNoScratch(t, pluginsDir)
}

func TestCommit_BuildEnvInjectsIgnoreScripts(t *testing.T) {
	repo := initRepo(t, `schema_version: 1
name: notifier
version: 0.1.0
description: env injection
jin: ">=0.0.0"
install:
  source:
    build:
      - printenv npm_config_ignore_scripts > env.txt
    entrypoint: ./run.sh
on:
  - status_changed
`)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	plan, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := plan.Commit(testBuildTimeout); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(pluginsDir, "notifier", "env.txt"))
	if err != nil {
		t.Fatalf("read env.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "true" {
		t.Errorf("npm_config_ignore_scripts = %q, want true", strings.TrimSpace(string(got)))
	}
}

func TestFetchUpdate_PicksUpNewCommit(t *testing.T) {
	repo := initRepo(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	plan, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	oldSHA := plan.CommitSHA()
	if err := plan.Commit(testBuildTimeout); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	writeInto(t, filepath.Join(repo, "v2.txt"), "v2")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "second")

	up, err := FetchUpdate("notifier", pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("FetchUpdate: %v", err)
	}
	if up.CommitSHA() == oldSHA {
		t.Error("update should pick up a new commit SHA")
	}
	if up.PrevCommitSHA() != oldSHA {
		t.Errorf("PrevCommitSHA = %q, want %q", up.PrevCommitSHA(), oldSHA)
	}
	if err := up.Commit(testBuildTimeout); err != nil {
		t.Fatalf("update Commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, "notifier", "v2.txt")); err != nil {
		t.Errorf("updated content missing: %v", err)
	}
	assertNoScratch(t, pluginsDir)

	lock, _ := LoadLock(stateDir)
	if entry, _ := lock.Get("notifier"); entry.Commit != up.CommitSHA() {
		t.Errorf("lock Commit = %q, want %q", entry.Commit, up.CommitSHA())
	}
}

func TestFetchUpdate_RejectsIncompatibleNewVersion(t *testing.T) {
	repo := initRepo(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()

	plan, err := Fetch(fileSource(t, repo, ""), pluginsDir, stateDir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := plan.Commit(testBuildTimeout); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	installedSHA := plan.CommitSHA()

	// Second commit bumps jin range to something no realistic build can
	// satisfy so FetchUpdate's compat check hard-fails.
	restore := setJinVersionForTest(t, "0.5.0")
	defer restore()

	writeInto(t, filepath.Join(repo, manifest.Filename), `schema_version: 1
name: notifier
version: 0.2.0
description: bump
jin: ">=99.0.0"
install:
  source:
    build: ["true"]
    entrypoint: ./run.sh
on:
  - status_changed
`)
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "bump jin")

	if _, err := FetchUpdate("notifier", pluginsDir, stateDir); err == nil {
		t.Fatal("FetchUpdate with unsatisfied jin range: want error, got nil")
	}
	assertNoScratch(t, pluginsDir)

	m, _, err := manifest.LoadFile(filepath.Join(pluginsDir, "notifier"))
	if err != nil {
		t.Fatalf("existing plugin unreadable after failed update: %v", err)
	}
	if m.Version != "0.1.0" {
		t.Errorf("installed version = %q, want unchanged 0.1.0", m.Version)
	}
	lock, _ := LoadLock(stateDir)
	if entry, _ := lock.Get("notifier"); entry.Commit != installedSHA {
		t.Errorf("lock Commit = %q, want unchanged %q", entry.Commit, installedSHA)
	}
}

func TestFetchUpdate_RejectsLinked(t *testing.T) {
	src := writeManifest(t, validSource)
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	if _, err := Link(src, pluginsDir, stateDir); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if _, err := FetchUpdate("notifier", pluginsDir, stateDir); err == nil {
		t.Fatal("FetchUpdate of a linked plugin: want error, got nil")
	}
}
