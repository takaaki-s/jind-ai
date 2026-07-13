package plugin

// This file owns installation and removal of plugins on disk: the `--link` path
// (symlink a local directory) and the git-clone install/update path. Cloning is
// split into a two-phase API — Fetch/FetchUpdate stage into a scratch dir and
// return an InstallPlan, then Commit builds and atomically places it — so the
// CLI can show the plan (name, commit SHA, build) for confirmation before
// anything durable happens.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// Link installs the plugin at srcDir by symlinking it into pluginsDir and
// recording a linked lock entry. It returns the loaded manifest.
//
// The manifest's declared name — not srcDir's basename — decides the install
// path, because a linked plugin is authoritative about its own name. Link is
// fail-closed: an unloadable manifest or a jin compat mismatch aborts before
// anything is written.
func Link(srcDir, pluginsDir, stateDir string) (*manifest.Manifest, error) {
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, fmt.Errorf("resolve source dir: %w", err)
	}

	m, err := loadManifest(absSrc)
	if err != nil {
		return nil, err
	}
	if err := checkJinCompat(m); err != nil {
		return nil, err
	}

	dest := filepath.Join(pluginsDir, m.Name)
	// Lstat, not Stat, so a dangling symlink also counts as "already there".
	if _, err := os.Lstat(dest); err == nil {
		return nil, fmt.Errorf("plugin %q is already installed at %s", m.Name, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat install path: %w", err)
	}

	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir plugins dir: %w", err)
	}
	if err := os.Symlink(absSrc, dest); err != nil {
		return nil, fmt.Errorf("symlink plugin: %w", err)
	}

	lock, err := LoadLock(stateDir)
	if err != nil {
		_ = os.Remove(dest)
		return nil, err
	}
	entry := LockEntry{
		Source:      absSrc,
		Linked:      true,
		InstalledAt: time.Now(),
	}
	// If the lock write fails, roll back the symlink so no half-installed
	// plugin is left on disk without a lock record.
	if err := lock.Set(m.Name, entry); err != nil {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("record lock entry: %w", err)
	}
	return m, nil
}

// Remove deletes the plugin's directory (or symlink) under pluginsDir and its
// lock entry. For a linked plugin the symlink is deleted with os.Remove, which
// never follows the link, so the source tree is left untouched.
//
// The lock entry is cleared even when the on-disk directory is already gone,
// so Remove doubles as a repair for a plugin whose files vanished but whose
// lock record lingered. The converse repair also works: a directory with no
// lock entry (an install whose lock write failed) is still deleted, since
// otherwise it would block both re-install and remove with no way out short
// of hand-deleting files.
func Remove(name, pluginsDir, stateDir string) error {
	// The orphan branch below deletes whatever filepath.Join resolves to, so
	// name must never smuggle path separators or ".." out of pluginsDir.
	// Locked names already passed manifest validation; enforce the same
	// grammar for everything.
	if !manifest.NamePattern.MatchString(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	lock, err := LoadLock(stateDir)
	if err != nil {
		return err
	}
	dest := filepath.Join(pluginsDir, name)
	entry, locked := lock.Get(name)
	if !locked {
		fi, statErr := os.Lstat(dest)
		if statErr != nil {
			return fmt.Errorf("plugin %q is not installed", name)
		}
		// Orphan: no lock entry to consult, so decide symlink-vs-directory
		// from the filesystem itself.
		entry.Linked = fi.Mode()&os.ModeSymlink != 0
	}

	if entry.Linked {
		err = os.Remove(dest)
	} else {
		err = os.RemoveAll(dest)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plugin dir: %w", err)
	}

	if !locked {
		return nil
	}
	return lock.Remove(name)
}

// Source is a parsed plugin install target. Raw is the original argument (stored
// verbatim in the lock so an update can re-derive the same clone), CloneURL is
// what `git clone` receives, and Ref is the branch/tag/commit to check out (""
// means the remote's default branch).
type Source struct {
	Raw      string
	CloneURL string
	Ref      string
}

// ParseSource splits an install argument into a clone URL and optional ref.
//
// A trailing "@<ref>" is recognised only when the '@' falls after the last '/',
// so the '@' in an scp-style URL (git@github.com:o/r) is never mistaken for a
// ref separator. The clone URL is then taken as-is when it already carries a
// scheme ("://") or is scp-style ("git@..."); otherwise a bare host/owner/repo
// is prefixed with "https://".
func ParseSource(arg string) (Source, error) {
	if arg == "" {
		return Source{}, errors.New("plugin source is empty")
	}

	base, ref := arg, ""
	slash := strings.LastIndexByte(arg, '/')
	if at := strings.IndexByte(arg[slash+1:], '@'); at >= 0 {
		cut := slash + 1 + at
		base, ref = arg[:cut], arg[cut+1:]
	}
	if base == "" {
		return Source{}, fmt.Errorf("plugin source %q has no repository before @%s", arg, ref)
	}

	cloneURL := base
	switch {
	case strings.Contains(base, "://"), strings.HasPrefix(base, "git@"):
		// Already a full clone URL (https://…, git@host:…): pass through.
	default:
		cloneURL = "https://" + base
	}

	return Source{Raw: arg, CloneURL: cloneURL, Ref: ref}, nil
}

// InstallPlan is a fetched-but-not-yet-committed install: the clone is staged
// under pluginsDir and its manifest and commit SHA are known, but nothing has
// been placed at the plugin's final path. The CLI shows the plan to the user,
// then calls Commit to build and install it or Abort to discard the staging dir.
type InstallPlan struct {
	src        Source
	pluginsDir string
	stateDir   string
	staging    string
	manifest   *manifest.Manifest
	commitSHA  string
	prevCommit string // update only: the SHA being replaced
	isUpdate   bool
}

// Manifest returns the loaded, validated manifest of the staged plugin.
func (p *InstallPlan) Manifest() *manifest.Manifest { return p.manifest }

// CommitSHA returns the commit that was staged (the tip of Ref, or of the
// default branch when Ref is empty).
func (p *InstallPlan) CommitSHA() string { return p.commitSHA }

// PrevCommitSHA returns the commit being replaced by an update, or "" for a
// fresh install.
func (p *InstallPlan) PrevCommitSHA() string { return p.prevCommit }

// Fetch clones src into a staging directory under pluginsDir, validates its
// manifest (fail-closed on a jin compat mismatch), and rejects the install
// if the plugin's declared name is already installed. On any failure the staging
// directory is removed so a failed fetch leaves no clutter.
func Fetch(src Source, pluginsDir, stateDir string) (*InstallPlan, error) {
	staging, m, sha, err := fetchToStaging(src, pluginsDir)
	if err != nil {
		return nil, err
	}
	p := &InstallPlan{
		src: src, pluginsDir: pluginsDir, stateDir: stateDir,
		staging: staging, manifest: m, commitSHA: sha,
	}

	dest := filepath.Join(pluginsDir, m.Name)
	if _, err := os.Lstat(dest); err == nil {
		p.Abort()
		return nil, fmt.Errorf("plugin %q is already installed at %s", m.Name, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		p.Abort()
		return nil, fmt.Errorf("stat install path: %w", err)
	}
	return p, nil
}

// FetchUpdate re-clones an installed plugin from its locked source so the update
// can be validated before the current version is touched. Linked plugins have no
// clone to update and are rejected. The updated manifest must keep the same name
// so the atomic swap in Commit targets the right directory.
func FetchUpdate(name, pluginsDir, stateDir string) (*InstallPlan, error) {
	lock, err := LoadLock(stateDir)
	if err != nil {
		return nil, err
	}
	entry, ok := lock.Get(name)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not installed", name)
	}
	if entry.Linked {
		return nil, errors.New("linked plugins are updated in place; re-link instead")
	}

	parsed, err := ParseSource(entry.Source)
	if err != nil {
		return nil, fmt.Errorf("parse locked source %q: %w", entry.Source, err)
	}
	src := Source{Raw: entry.Source, CloneURL: parsed.CloneURL, Ref: entry.Ref}

	staging, m, sha, err := fetchToStaging(src, pluginsDir)
	if err != nil {
		return nil, err
	}
	if m.Name != name {
		_ = os.RemoveAll(staging)
		return nil, fmt.Errorf("updated manifest name %q does not match installed name %q", m.Name, name)
	}
	return &InstallPlan{
		src: src, pluginsDir: pluginsDir, stateDir: stateDir,
		staging: staging, manifest: m, commitSHA: sha,
		prevCommit: entry.Commit, isUpdate: true,
	}, nil
}

// Commit builds the staged plugin (when the manifest declares install.source.build),
// places it at its final path atomically, and records the lock entry. A build
// failure or a placement failure discards the staging directory and, for an
// update, leaves the current version untouched. buildTimeout bounds the whole
// build sequence.
func (p *InstallPlan) Commit(buildTimeout time.Duration) error {
	dest := filepath.Join(p.pluginsDir, p.manifest.Name)

	if cmds := p.manifest.BuildCommands(); len(cmds) > 0 {
		if err := runBuilds(p.staging, p.stateDir, p.manifest.Name, cmds, buildTimeout); err != nil {
			p.Abort()
			return err
		}
	}

	if err := p.place(dest); err != nil {
		p.Abort()
		return err
	}
	p.staging = "" // ownership moved to dest; Abort must not touch it now

	lock, err := LoadLock(p.stateDir)
	if err != nil {
		return err
	}
	entry := LockEntry{
		Source:      p.src.Raw,
		Ref:         p.src.Ref,
		Commit:      p.commitSHA,
		Linked:      false,
		InstalledAt: time.Now(),
	}
	if err := lock.Set(p.manifest.Name, entry); err != nil {
		return fmt.Errorf("record lock entry: %w", err)
	}
	return nil
}

// place moves the staging directory to dest. A fresh install is a single
// rename. An update parks the current directory at ".old-<name>" first and
// restores it if the swap fails, so a failed update never destroys the working
// version.
func (p *InstallPlan) place(dest string) error {
	if !p.isUpdate {
		if err := os.Rename(p.staging, dest); err != nil {
			return fmt.Errorf("install plugin: %w", err)
		}
		return nil
	}

	old := filepath.Join(p.pluginsDir, ".old-"+p.manifest.Name)
	_ = os.RemoveAll(old) // clear any leftover from a previous interrupted update
	if err := os.Rename(dest, old); err != nil {
		return fmt.Errorf("move current plugin aside: %w", err)
	}
	if err := os.Rename(p.staging, dest); err != nil {
		_ = os.Rename(old, dest) // restore the version we parked
		return fmt.Errorf("install updated plugin: %w", err)
	}
	_ = os.RemoveAll(old)
	return nil
}

// Abort discards the staging directory. It is safe to call more than once and
// after a successful Commit (which clears p.staging), so the CLI can defer it
// unconditionally.
func (p *InstallPlan) Abort() {
	if p.staging == "" {
		return
	}
	_ = os.RemoveAll(p.staging)
	p.staging = ""
}

// fetchToStaging clones src into a fresh staging dir under pluginsDir, checks out
// Ref when set, loads and jin-compat-checks the manifest, and reads the HEAD
// commit. Any failure removes the staging dir so callers never see a partial
// clone.
func fetchToStaging(src Source, pluginsDir string) (staging string, m *manifest.Manifest, sha string, err error) {
	if err = os.MkdirAll(pluginsDir, 0o755); err != nil {
		return "", nil, "", fmt.Errorf("mkdir plugins dir: %w", err)
	}
	staging = filepath.Join(pluginsDir, ".staging-"+uuid.New().String())

	defer func() {
		if err != nil {
			_ = os.RemoveAll(staging)
			staging = ""
		}
	}()

	if err = gitClone(src.CloneURL, staging); err != nil {
		return
	}
	if src.Ref != "" {
		if err = gitCheckout(staging, src.Ref); err != nil {
			return
		}
	}
	if m, err = loadManifest(staging); err != nil {
		return
	}
	if err = checkJinCompat(m); err != nil {
		return
	}
	sha, err = gitHeadSHA(staging)
	return
}

// runBuilds runs each of the manifest's install.source.build commands via
// `bash -c` in the staging dir, in order. buildTimeout bounds the entire
// sequence so a wedged step cannot outlive the caller-supplied window. Each
// step gets its own process group so an escalated SIGKILL sweeps grandchildren.
// The environment is the curated base plus npm_config_ignore_scripts=true,
// a supply-chain guard the author can override inside their own build command.
// Output from every step is truncated into
// <stateDir>/plugin-logs/<name>-build.log and teed to stderr so an interactive
// install shows build progress.
func runBuilds(stagingDir, stateDir, name string, cmds []string, timeout time.Duration) error {
	logPath := filepath.Join(stateDir, "plugin-logs", name+"-build.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("mkdir plugin log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open build log: %w", err)
	}
	defer logFile.Close()

	out := io.MultiWriter(logFile, os.Stderr)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for i, cmdStr := range cmds {
		fmt.Fprintf(out, "\n--- build step %d/%d: %s ---\n", i+1, len(cmds), cmdStr)

		cmd := exec.CommandContext(ctx, "bash", "-c", cmdStr)
		cmd.Dir = stagingDir
		cmd.Env = append(curatedEnv(), "npm_config_ignore_scripts=true")
		cmd.Stdout = out
		cmd.Stderr = out
		setProcessGroupKill(cmd)

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start build step %q: %w", cmdStr, err)
		}
		runErr := cmd.Wait()

		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("build timed out after %s (log: %s)", timeout, logPath)
		}
		if runErr != nil {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				return fmt.Errorf("build step %q failed: exit status %d (log: %s)", cmdStr, exitErr.ExitCode(), logPath)
			}
			return fmt.Errorf("run build step %q: %w (log: %s)", cmdStr, runErr, logPath)
		}
	}
	return nil
}

// gitClone clones cloneURL into dest as a full clone (no --depth) so a later
// checkout of an arbitrary ref and an update's SHA history both have the objects
// they need.
func gitClone(cloneURL, dest string) error {
	out, err := exec.Command("git", "clone", cloneURL, dest).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone %s: %s", cloneURL, gitErr(out, err))
	}
	return nil
}

func gitCheckout(repoDir, ref string) error {
	out, err := exec.Command("git", "-C", repoDir, "checkout", ref).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git checkout %s: %s", ref, gitErr(out, err))
	}
	return nil
}

func gitHeadSHA(repoDir string) (string, error) {
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitErr picks the most useful text for a failed git command: git's own
// combined output when present (it carries the human-readable reason), else the
// process error.
func gitErr(out []byte, err error) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return err.Error()
}
