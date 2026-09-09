package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/internal/tmux"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Manage jin plugins",
	Long: `Install, remove, update, and list jin plugins.

Plugins are user-installed programs that react to session events. Each lives in
its own directory with a jind-ai-plugin.yaml manifest and is recorded in the
plugin lock file.`,
}

var pluginInstallCmd = &cobra.Command{
	Use:   "install [source]",
	Short: "Install a plugin",
	Long: `Install a plugin from the registry, a git source, or a local directory.

Prefer the registry name for anything listed by 'jin plugin ls-remote': it is
the NAME column, resolves through the plugin registry, and pins the install to
the commit SHA the crawler recorded for the requested version (default: the
entry's latest_version). --pin/-v selects a specific version, --refresh
bypasses the 24-hour registry cache, and --force lets the install proceed when
the plugin's jin compat range does not match this binary.

For anything not in the registry — or when you need to point at a specific
branch, tag, or commit — pass a git source: <host>/<owner>/<repo>[@ref], or
any URL 'git clone' accepts. The repository is cloned, its manifest is shown
for confirmation, and — when the manifest declares a build — the build runs
before the plugin is placed. The REPO column of 'ls-remote' is also a valid
git source; pasting it works but bypasses the registry's SHA pinning, so
prefer the NAME path when the entry appears in the registry.

With --link <path>, the local directory at <path> is symlinked into the plugins
directory (its manifest name decides the install name) so edits take effect
without reinstalling. A linked plugin is trusted outright and is not built.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPluginInstall,
}

var pluginUpdateCmd = &cobra.Command{
	Use:               "update <name>",
	Short:             "Update an installed plugin to its source's latest commit",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePluginNames,
	RunE:              runPluginUpdate,
}

var pluginRemoveCmd = &cobra.Command{
	Use:               "remove <name>",
	Short:             "Remove an installed plugin",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePluginNames,
	RunE:              runPluginRemove,
}

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed plugins",
	Args:  cobra.NoArgs,
	RunE:  runPluginList,
}

var pluginRunCmd = &cobra.Command{
	Use:   "run <name> [action] [--session <selector>]",
	Short: "Run a plugin action on demand",
	Long: `Run a plugin action immediately, bypassing event matching and debounce.
Without [action] the plugin's default action (the first one its manifest
declares) runs; naming an unknown action is an error listing the available
ones. The plugin receives JIN_EVENT=action, plus the session's current
snapshot when --session is given; without it the run is a global action and
all session fields are empty. When invoked from inside tmux, the caller's
server socket and pane travel with the run as
JIN_CALLER_TMUX_SOCKET/JIN_CALLER_TMUX_PANE. The run is asynchronous; follow
its output in the plugin log.`,
	Args:              cobra.RangeArgs(1, 2),
	ValidArgsFunction: completePluginRunArgs,
	RunE:              runPluginRun,
}

func init() {
	rootCmd.AddCommand(pluginCmd)
	pluginCmd.AddCommand(pluginInstallCmd, pluginUpdateCmd, pluginRemoveCmd, pluginListCmd, pluginRunCmd)
	pluginInstallCmd.Flags().String("link", "", "Install a local plugin directory as a symlink")
	pluginInstallCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	pluginInstallCmd.Flags().StringP("pin", "v", "", "Version to install from the registry (default: latest_version)")
	pluginInstallCmd.Flags().Bool("force", false, "Install even when the plugin's jin compat range is unsatisfied")
	pluginInstallCmd.Flags().Bool("refresh", false, "Bypass the registry cache freshness check")
	pluginInstallCmd.Flags().String("registry", "", "Registry URL (default: canonical)")
	pluginUpdateCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	pluginRunCmd.Flags().StringP("session", "s", "", "Session selector (ID prefix or description substring); omit for a global action")
}

func runPluginRun(cmd *cobra.Command, args []string) error {
	name := args[0]
	// An empty action ID means the plugin's default action (actions[0]); the
	// daemon owns that resolution so old and new CLIs behave identically.
	actionID := ""
	if len(args) >= 2 {
		actionID = args[1]
	}

	client := daemon.NewClient(getSocketPath())

	selector, _ := cmd.Flags().GetString("session")
	sessionID, sessionDesc, err := resolvePluginRunSession(client, cmd.Flags().Changed("session"), selector)
	if err != nil {
		return err
	}

	// jinenv.EnvDepth is set when this CLI is invoked from inside a plugin run;
	// forwarding it lets the daemon reject a plugin chaining another plugin run.
	// An unset or malformed value is treated as depth 0 (a top-level invocation),
	// and so is the empty one every pane jind-ai opens carries — jinenv writes it
	// there precisely so a depth stranded in a tmux server cannot be read as this
	// caller's.
	depth, _ := strconv.Atoi(os.Getenv(jinenv.EnvDepth))

	err = client.PluginRun(daemon.PluginRunRequest{
		Plugin:           name,
		Action:           actionID,
		SessionID:        sessionID,
		Depth:            depth,
		CallerTmuxSocket: tmux.SocketPathFromEnv(os.Getenv("TMUX")),
		CallerTmuxPane:   os.Getenv("TMUX_PANE"),
	})
	if err != nil {
		return err
	}
	// The started message names the action only when the caller did — a
	// default run keeps the plugin-level phrasing older scripts may grep for.
	started := name
	if actionID != "" {
		started = name + ":" + actionID
	}
	if sessionID == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Started plugin %s (global)\n", started)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Started plugin %s for session %s\n", started, sessionDesc)
	}
	return nil
}

// resolvePluginRunSession maps the --session flag state to a run target: a
// never-passed flag means a global action (empty ID, no resolution), while an
// explicitly passed value — even an empty one — always goes through selector
// resolution so a mistyped `--session ""` fails loudly instead of silently
// becoming a global run.
func resolvePluginRunSession(client *daemon.Client, flagChanged bool, selector string) (sessionID, sessionDesc string, err error) {
	if !flagChanged {
		return "", "", nil
	}
	return resolveSession(client, selector)
}

func runPluginInstall(cmd *cobra.Command, args []string) error {
	if link, _ := cmd.Flags().GetString("link"); link != "" {
		return runPluginInstallLink(cmd, link)
	}

	if len(args) == 0 {
		return errors.New("plugin source required (a registry name, github.com/owner/repo, or --link <path>)")
	}

	arg := args[0]
	// A registry name matches NamePattern's grammar (^[a-z][a-z0-9-]{1,63}$),
	// which forbids '/' and ':', so it never collides with a git URL or an
	// scp-style remote. Anything else is handed to the git-clone path.
	if manifest.NamePattern.MatchString(arg) {
		return runPluginInstallByName(cmd, arg)
	}
	return runPluginInstallBySource(cmd, arg)
}

func runPluginInstallLink(cmd *cobra.Command, link string) error {
	m, err := plugin.Link(link, paths.Plugins(), getStateDir())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "name: %s\n", m.Name)
	for i := range m.Actions {
		printActionLine(out, "action ", &m.Actions[i])
	}
	fmt.Fprintln(out, "linked")
	return nil
}

// printActionLine renders one manifest action as "<prefix><id>: <entrypoint>
// (on: ...)", omitting the on-clause for actions without event matchers.
// Shared by the link confirmation and the install/update consent screen so
// both surfaces describe exactly what the dispatcher will execute.
func printActionLine(out io.Writer, prefix string, a *manifest.Action) {
	fmt.Fprintf(out, "%s%s: %s", prefix, a.ID, a.Entrypoint)
	if a.Handoff {
		fmt.Fprint(out, " (handoff)")
	}
	if len(a.On) > 0 {
		fmt.Fprintf(out, " (on: %s)", strings.Join(a.On, ", "))
	}
	fmt.Fprintln(out)
}

func runPluginInstallBySource(cmd *cobra.Command, arg string) error {
	out := cmd.OutOrStdout()

	src, err := plugin.ParseSource(arg)
	if err != nil {
		return err
	}
	// --force is scoped to the registry install path, so the git-URL path
	// keeps the historical fail-closed compat behaviour.
	// `@ref` on the CLI = the user explicitly picked a version, so `plugin
	// update` must not silently move them off it later.
	plan, err := plugin.Fetch(src, paths.Plugins(), getStateDir(), plugin.FetchOptions{
		Pinned: src.Ref != "",
	})
	if err != nil {
		return err
	}
	defer plan.Abort() // no-op after a successful Commit; discards staging otherwise

	m := plan.Manifest()
	printPluginPlan(out, m, src.Raw, plan.CommitSHA())

	if yes, _ := cmd.Flags().GetBool("yes"); !yes && !confirmYN(cmd, "Install? [y/N]: ") {
		fmt.Fprintln(out, "aborted")
		return nil
	}

	buildTimeout, err := pluginBuildTimeout()
	if err != nil {
		return err
	}
	if cmds := m.BuildCommands(); len(cmds) > 0 {
		fmt.Fprintf(out, "Running build: %s\n", strings.Join(cmds, " && "))
	}
	if err := plan.Commit(buildTimeout); err != nil {
		return err
	}
	fmt.Fprintf(out, "Installed %s @ %s\n", m.Name, shortSHA(plan.CommitSHA()))
	return nil
}

// runPluginInstallByName resolves a registry name to a repo+SHA, stages the
// clone, prints the consent screen (unverified marker, jin compat verdict,
// build commands, install path), and — after the user confirms — commits.
// A jin compat mismatch aborts unless --force was passed; the screen shows
// the mismatch verdict either way so the user can see what --force overrides.
func runPluginInstallByName(cmd *cobra.Command, name string) error {
	out := cmd.OutOrStdout()

	versionPin, _ := cmd.Flags().GetString("pin")
	force, _ := cmd.Flags().GetBool("force")
	refresh, _ := cmd.Flags().GetBool("refresh")
	registryURL, _ := cmd.Flags().GetString("registry")
	yes, _ := cmd.Flags().GetBool("yes")

	doc, result, err := loadRegistryDocument(registryURL, refresh)
	if err != nil {
		return err
	}
	if result.Outcome == manifest.OutcomeCacheFallback && result.FetchErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: registry fetch failed, using cache: %v\n", result.FetchErr)
	}

	resolution, err := plugin.ResolveRemote(name, versionPin, doc)
	if err != nil {
		return err
	}

	// Always let Fetch return the plan so the consent screen is printed even
	// when the manifest's jin range does not match this build. Whether to
	// proceed after the screen is a CLI-side decision — with --force we go
	// on and commit, without it we surface the error the user just saw.
	// `-v <ver>` on the CLI = the user pinned; a bare `install <name>`
	// stays on the "follow latest" track under `plugin update`.
	plan, err := plugin.Fetch(resolution.Source(), paths.Plugins(), getStateDir(),
		plugin.FetchOptions{
			AllowIncompatibleJin: true,
			Pinned:               versionPin != "",
		})
	if err != nil {
		return err
	}
	defer plan.Abort() // no-op after a successful Commit; discards staging otherwise

	m := plan.Manifest()
	// paths.Plugins() is XDG-anchored and already absolute, so a plain Join
	// produces the absolute install path the consent screen wants to show.
	dest := filepath.Join(paths.Plugins(), m.Name)
	printRemotePluginPlan(out, resolution, m, plan.CommitSHA(), dest, plan.CompatErr())

	if plan.CompatErr() != nil && !force {
		return fmt.Errorf("%v (pass --force to override)", plan.CompatErr())
	}
	if !yes && !confirmYN(cmd, "Continue? [y/N]: ") {
		fmt.Fprintln(out, "aborted")
		return nil
	}

	buildTimeout, err := pluginBuildTimeout()
	if err != nil {
		return err
	}
	if cmds := m.BuildCommands(); len(cmds) > 0 {
		fmt.Fprintf(out, "Running build: %s\n", strings.Join(cmds, " && "))
	}
	if err := plan.Commit(buildTimeout); err != nil {
		return err
	}
	fmt.Fprintf(out, "Installed %s @ %s\n", m.Name, shortSHA(plan.CommitSHA()))
	return nil
}

func runPluginUpdate(cmd *cobra.Command, args []string) error {
	name := args[0]
	out := cmd.OutOrStdout()

	// Guard before we touch git: linked and pinned entries are handled here,
	// not deep in FetchUpdate, so the user sees a clean early return instead
	// of a wasted clone.
	lock, err := plugin.LoadLock(getStateDir())
	if err != nil {
		return err
	}
	entry, ok := lock.Get(name)
	if !ok {
		return fmt.Errorf("plugin %q is not installed", name)
	}
	if entry.Linked {
		return errors.New("linked plugins are updated in place; re-link instead")
	}
	if entry.Pinned {
		fmt.Fprintf(out, "%s is pinned to %s (installed as %s). To move it, reinstall with a different version pin.\n",
			name, shortSHA(entry.Commit), entry.Source)
		return nil
	}

	// Not pinned: resolve the plugin's latest release. Consult the registry
	// first when the entry parses as `<registry-name>@<sha>` shape (bare
	// install); fall back to `git ls-remote --tags` for arbitrary git URLs.
	updateSrc, err := resolveUpdateSource(name, entry, cmd)
	if err != nil {
		return err
	}

	plan, err := plugin.FetchUpdate(name, paths.Plugins(), getStateDir(), plugin.FetchOptions{
		UpdateSource: updateSrc,
	})
	if err != nil {
		return err
	}
	defer plan.Abort() // no-op after a successful Commit; discards staging otherwise

	if plan.PrevCommitSHA() == plan.CommitSHA() {
		fmt.Fprintf(out, "%s is already up to date\n", name)
		return nil
	}

	m := plan.Manifest()
	printPluginPlan(out, m, entry.Source, plan.CommitSHA())
	fmt.Fprintf(out, "Update: %s -> %s\n", shortSHA(plan.PrevCommitSHA()), shortSHA(plan.CommitSHA()))

	if yes, _ := cmd.Flags().GetBool("yes"); !yes && !confirmYN(cmd, "Update? [y/N]: ") {
		fmt.Fprintln(out, "aborted")
		return nil
	}

	buildTimeout, err := pluginBuildTimeout()
	if err != nil {
		return err
	}
	if cmds := m.BuildCommands(); len(cmds) > 0 {
		fmt.Fprintf(out, "Running build: %s\n", strings.Join(cmds, " && "))
	}
	if err := plan.Commit(buildTimeout); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s: %s -> %s\n", name, shortSHA(plan.PrevCommitSHA()), shortSHA(plan.CommitSHA()))
	return nil
}

// resolveUpdateSource decides which ref `plugin update` should re-clone for
// an unpinned entry. The registry is consulted first — a bare `install
// <registry-name>` locked the plugin under a `<owner/name>@<sha>` shape and
// the registry is the source of truth for that plugin's latest release. If
// the plugin is not in the registry (a direct git-URL install), fall back
// to the highest semver tag advertised by `git ls-remote --tags`. When
// neither route surfaces a candidate, return a nil *Source, which asks
// FetchUpdate to reuse the locked ref — the "no newer release to move to"
// case, indistinguishable at the source from a legitimately-current entry.
func resolveUpdateSource(name string, entry plugin.LockEntry, cmd *cobra.Command) (*plugin.Source, error) {
	parsed, err := plugin.ParseSource(entry.Source)
	if err != nil {
		return nil, fmt.Errorf("parse locked source %q: %w", entry.Source, err)
	}

	// 1) Registry lookup by plugin name — a match wins because the registry
	// carries the plugin author's declared "latest release" for us; --refresh
	// bypasses the cache-freshness check when the user wants the newest read.
	registryURL, _ := cmd.Flags().GetString("registry")
	refresh, _ := cmd.Flags().GetBool("refresh")
	doc, _, docErr := loadRegistryDocument(registryURL, refresh)
	if docErr == nil {
		if resolution, err := plugin.ResolveRemote(name, "", doc); err == nil {
			src := resolution.Source()
			return &src, nil
		}
	}

	// 2) `git ls-remote --tags` on the locked clone URL — for plugins that
	// were installed from a raw git URL and are not in the registry.
	tag, err := plugin.GitLatestSemverTag(parsed.CloneURL)
	if err != nil {
		return nil, fmt.Errorf("resolve latest tag for %s: %w", parsed.CloneURL, err)
	}
	if tag == "" {
		// No semver tags advertised — the historical behaviour (re-clone
		// the locked ref) is the least-surprising thing to do.
		return nil, nil
	}
	newSrc := plugin.Source{
		Raw:      trimAtRef(entry.Source) + "@" + tag,
		CloneURL: parsed.CloneURL,
		Ref:      tag,
	}
	return &newSrc, nil
}

// trimAtRef returns s with any trailing `@<ref>` removed. Used when composing
// a new lock Source string from a resolved tag: entry.Source already carries
// the previous @<sha>, and we want to swap it for the new tag.
func trimAtRef(s string) string {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return s[:i]
	}
	return s
}

func runPluginRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	if err := plugin.Remove(name, paths.Plugins(), getStateDir()); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed: %s\n", name)
	return nil
}

// printRemotePluginPlan renders the consent screen for a registry-driven
// install. A non-nil compatErr still renders with ✗ (instead of aborting the
// screen) so what --force is about to override is visible before it is.
func printRemotePluginPlan(out io.Writer, r *plugin.RemoteResolution, m *manifest.Manifest, commitSHA, installPath string, compatErr error) {
	entry := r.Entry
	ver := r.Version
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Plugin:  %s @ %s\n", entry.Name, ver.Version)
	fmt.Fprintf(out, "  Source:  %s@%s\n", entry.Repo, shortSHA(commitSHA))
	fmt.Fprintln(out, "  Kind:    (unverified community plugin)")

	compatLine := fmt.Sprintf("jin %s", m.Jin)
	if jv := jinDisplayVersion(); jv != "" {
		compatLine += fmt.Sprintf("  (you have %s)", jv)
	}
	if compatErr == nil {
		compatLine += "  ✓"
	} else {
		compatLine += "  ✗"
	}
	fmt.Fprintf(out, "  Compat:  %s\n", compatLine)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Installation will:")
	fmt.Fprintf(out, "    1. clone %s at %s into\n", entry.Repo, shortSHA(commitSHA))
	fmt.Fprintf(out, "       %s/\n", installPath)
	if cmds := m.BuildCommands(); len(cmds) > 0 {
		for i, c := range cmds {
			fmt.Fprintf(out, "    %d. run: %s\n", i+2, c)
		}
	}
	fmt.Fprintln(out)
}

// jinDisplayVersion is the running jin version rendered for consent screens,
// or "" for a dev build (in which case the compat check is skipped anyway).
// The import path lives here (not in internal/plugin) because the CLI is the
// only consent-screen consumer.
func jinDisplayVersion() string {
	v := plugin.CurrentJinVersion()
	if v == "" || v == "dev" {
		return ""
	}
	return v
}

// printPluginPlan renders the confirmation block shared by install and update.
// source is the raw install argument (install) or the locked source (update).
// Actions are listed one per line so the user consents to every entrypoint
// and matcher set the dispatcher will run, not just the default action.
func printPluginPlan(out io.Writer, m *manifest.Manifest, source, commitSHA string) {
	fmt.Fprintf(out, "Plugin: %s v%s\n", m.Name, m.Version)
	fmt.Fprintf(out, "Source: %s\n", source)
	fmt.Fprintf(out, "Commit: %s\n", shortSHA(commitSHA))
	if m.Jin != "" {
		fmt.Fprintf(out, "Jin:    %s\n", m.Jin)
	}
	for i := range m.Actions {
		printActionLine(out, "Action: ", &m.Actions[i])
	}
	if cmds := m.BuildCommands(); len(cmds) > 0 {
		fmt.Fprintf(out, "Build:  %s\n", strings.Join(cmds, " && "))
	}
}

// confirmYN asks prompt and reports whether the answer was an explicit yes.
// Anything else — including a bare Enter — is a no, so a caller can default to
// the safe branch by wording the prompt "[y/N]".
//
// Shared by the plugin and init commands; keep it that way rather than
// rederiving the reader, per the atomicfile note in docs/conventions.md.
func confirmYN(cmd *cobra.Command, prompt string) bool {
	fmt.Fprint(cmd.OutOrStdout(), prompt)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// pluginBuildTimeout resolves this user's plugins.build_timeout into a
// Duration for InstallPlan.Commit. `plugin validate --run-build` deliberately
// does not come through here — see validateBuildTimeout.
func pluginBuildTimeout() (time.Duration, error) {
	mgr, err := config.NewManager(getConfigDir())
	if err != nil {
		return 0, err
	}
	return mgr.GetPluginsConfig().BuildTimeoutDuration(), nil
}

func loadPluginEntries() ([]plugin.Entry, error) {
	mgr, err := config.NewManager(getConfigDir())
	if err != nil {
		return nil, err
	}
	reg := plugin.NewRegistry(paths.Plugins(), getStateDir(), mgr.GetPluginsConfig())
	return reg.Load()
}

// completePluginNames suggests installed plugin names for shell completion.
func completePluginNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	entries, err := loadPluginEntries()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name, toComplete) {
			names = append(names, e.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completePluginRunArgs is the two-stage completion for `jin plugin run`:
// installed plugin names in position 0, that plugin's action IDs in position 1.
func completePluginRunArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return completePluginNames(cmd, args, toComplete)
	case 1:
		return completePluginActions(args[0], toComplete)
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// completePluginActions suggests the action IDs declared by pluginName's
// manifest. Actions with `listener: true` or `handoff: true` are dedicated
// endpoints and are omitted from the suggestion list. Listeners remain
// accepted at runtime for debug invocation; handoff actions require the
// structured `jin session pr-handoff` path. An unknown or
// broken plugin yields no suggestions (cobra then completes nothing) rather
// than an error — completion must never fail loudly.
func completePluginActions(pluginName, toComplete string) ([]string, cobra.ShellCompDirective) {
	entries, err := loadPluginEntries()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	for _, e := range entries {
		if e.Name != pluginName || e.Manifest == nil {
			continue
		}
		var ids []string
		for _, a := range e.Manifest.Actions {
			if a.Listener || a.Handoff {
				continue
			}
			if strings.HasPrefix(a.ID, toComplete) {
				ids = append(ids, a.ID)
			}
		}
		return ids, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// pluginListItem is the JSON shape for `jin plugin list --json`. plugin.Entry
// itself is not JSON-friendly (State is an int enum, Err is an interface), so
// the CLI projects it onto stable field names.
type pluginListItem struct {
	Name          string `json:"name"`
	Version       string `json:"version,omitempty"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	Description   string `json:"description,omitempty"`
	State         string `json:"state"`
	Linked        bool   `json:"linked"`
	Source        string `json:"source"`
	Ref           string `json:"ref,omitempty"`
	Commit        string `json:"commit,omitempty"`
	Error         string `json:"error,omitempty"`
}

func runPluginList(cmd *cobra.Command, args []string) error {
	entries, err := loadPluginEntries()
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if jsonOutput {
		items := make([]pluginListItem, 0, len(entries))
		for _, e := range entries {
			item := pluginListItem{
				Name:   e.Name,
				State:  e.State.String(),
				Linked: e.Lock.Linked,
				Source: e.Lock.Source,
				Ref:    e.Lock.Ref,
				Commit: e.Lock.Commit,
			}
			if e.Manifest != nil {
				item.Version = e.Manifest.Version
				item.SchemaVersion = e.Manifest.SchemaVersion
				item.Description = e.Manifest.Description
			}
			if e.Err != nil {
				item.Error = e.Err.Error()
			}
			items = append(items, item)
		}
		return writeJSON(out, items)
	}

	if len(entries) == 0 {
		fmt.Fprintln(out, "No plugins installed.")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tSTATE\tDESCRIPTION\tSOURCE")
	for _, e := range entries {
		version := "-"
		description := "-"
		if e.Manifest != nil {
			if e.Manifest.Version != "" {
				version = e.Manifest.Version
			}
			if d := manifestDescriptionSingleLine(e.Manifest.Description); d != "" {
				description = d
			}
		}
		state := e.State.String()
		if e.Lock.Linked {
			state += " (linked)"
		}
		if e.Err != nil {
			state += ": " + truncateStr(e.Err.Error(), 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Name, version, state, description, e.Lock.Source)
	}
	return tw.Flush()
}

// manifestDescriptionSingleLine flattens a possibly multi-line manifest
// description into one tab-safe line for the plain-text table. Multi-line
// scalars are legal in the YAML schema, but a table row cannot wrap.
func manifestDescriptionSingleLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Fold any run of whitespace (including embedded newlines) into a single
	// space so the tabwriter never sees a stray \n or \t.
	return strings.Join(strings.Fields(s), " ")
}
