package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/daemon"
	"github.com/takaaki-s/jindaiko/internal/paths"
	"github.com/takaaki-s/jindaiko/internal/plugin"
	"github.com/takaaki-s/jindaiko/internal/tmux"
)

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Manage jin plugins",
	Long: `Install, remove, update, and list jin plugins.

Plugins are user-installed programs that react to session events. Each lives in
its own directory with a jin-plugin.yaml manifest and is recorded in the plugin
lock file.`,
}

var pluginInstallCmd = &cobra.Command{
	Use:   "install [source]",
	Short: "Install a plugin",
	Long: `Install a plugin from a git source or a local directory.

A git source is <host>/<owner>/<repo>[@ref] (or any URL 'git clone' accepts).
The repository is cloned, its manifest is shown for confirmation, and — when the
manifest declares a build — the build runs before the plugin is placed.

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
	Use:   "run <name> [--session <selector>]",
	Short: "Run a plugin on demand",
	Long: `Run a plugin immediately, bypassing event matching and debounce. The
plugin receives JIN_EVENT=action, plus the session's current snapshot when
--session is given; without it the run is a global action and all session
fields are empty. When invoked from inside tmux, the caller's server socket
and pane travel with the run as JIN_CALLER_TMUX_SOCKET/JIN_CALLER_TMUX_PANE.
The run is asynchronous; follow its output in the plugin log.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePluginNames,
	RunE:              runPluginRun,
}

func init() {
	rootCmd.AddCommand(pluginCmd)
	pluginCmd.AddCommand(pluginInstallCmd, pluginUpdateCmd, pluginRemoveCmd, pluginListCmd, pluginRunCmd)
	pluginInstallCmd.Flags().String("link", "", "Install a local plugin directory as a symlink")
	pluginInstallCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	pluginUpdateCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	pluginRunCmd.Flags().StringP("session", "s", "", "Session selector (ID prefix or description substring); omit for a global action")
}

func runPluginRun(cmd *cobra.Command, args []string) error {
	name := args[0]

	client := daemon.NewClient(getSocketPath())

	selector, _ := cmd.Flags().GetString("session")
	sessionID, sessionDesc, err := resolvePluginRunSession(client, cmd.Flags().Changed("session"), selector)
	if err != nil {
		return err
	}

	// JIN_PLUGIN_DEPTH is set when this CLI is invoked from inside a plugin run;
	// forwarding it lets the daemon reject a plugin chaining another plugin run.
	// An unset or malformed value is treated as depth 0 (a top-level invocation).
	depth, _ := strconv.Atoi(os.Getenv("JIN_PLUGIN_DEPTH"))

	err = client.PluginRun(daemon.PluginRunRequest{
		Plugin:           name,
		SessionID:        sessionID,
		Depth:            depth,
		CallerTmuxSocket: tmux.SocketPathFromEnv(os.Getenv("TMUX")),
		CallerTmuxPane:   os.Getenv("TMUX_PANE"),
	})
	if err != nil {
		return err
	}
	if sessionID == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Started plugin %s (global)\n", name)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Started plugin %s for session %s\n", name, sessionDesc)
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
	out := cmd.OutOrStdout()

	if link, _ := cmd.Flags().GetString("link"); link != "" {
		m, err := plugin.Link(link, paths.Plugins(), getStateDir())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "name: %s\n", m.Name)
		fmt.Fprintf(out, "on: %s\n", strings.Join(m.On, ", "))
		fmt.Fprintf(out, "run: %s\n", m.Run)
		fmt.Fprintln(out, "linked")
		return nil
	}

	if len(args) == 0 {
		return errors.New("plugin source required (e.g. github.com/owner/repo), or use --link <path>")
	}

	src, err := plugin.ParseSource(args[0])
	if err != nil {
		return err
	}
	plan, err := plugin.Fetch(src, paths.Plugins(), getStateDir())
	if err != nil {
		return err
	}
	defer plan.Abort() // no-op after a successful Commit; discards staging otherwise

	m := plan.Manifest()
	printPluginPlan(out, m, src.Raw, plan.CommitSHA())

	if yes, _ := cmd.Flags().GetBool("yes"); !yes && !confirmPlugin(cmd, "Install? [y/N]: ") {
		fmt.Fprintln(out, "aborted")
		return nil
	}

	buildTimeout, err := pluginBuildTimeout()
	if err != nil {
		return err
	}
	if m.Build != "" {
		fmt.Fprintf(out, "Running build: %s\n", m.Build)
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

	plan, err := plugin.FetchUpdate(name, paths.Plugins(), getStateDir())
	if err != nil {
		return err
	}
	defer plan.Abort() // no-op after a successful Commit; discards staging otherwise

	if plan.PrevCommitSHA() == plan.CommitSHA() {
		fmt.Fprintf(out, "%s is already up to date\n", name)
		return nil
	}

	lock, err := plugin.LoadLock(getStateDir())
	if err != nil {
		return err
	}
	entry, _ := lock.Get(name) // present: FetchUpdate already verified the plugin is installed

	m := plan.Manifest()
	printPluginPlan(out, m, entry.Source, plan.CommitSHA())
	fmt.Fprintf(out, "Update: %s -> %s\n", shortSHA(plan.PrevCommitSHA()), shortSHA(plan.CommitSHA()))

	if yes, _ := cmd.Flags().GetBool("yes"); !yes && !confirmPlugin(cmd, "Update? [y/N]: ") {
		fmt.Fprintln(out, "aborted")
		return nil
	}

	buildTimeout, err := pluginBuildTimeout()
	if err != nil {
		return err
	}
	if m.Build != "" {
		fmt.Fprintf(out, "Running build: %s\n", m.Build)
	}
	if err := plan.Commit(buildTimeout); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s: %s -> %s\n", name, shortSHA(plan.PrevCommitSHA()), shortSHA(plan.CommitSHA()))
	return nil
}

func runPluginRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	if err := plugin.Remove(name, paths.Plugins(), getStateDir()); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed: %s\n", name)
	return nil
}

// printPluginPlan renders the confirmation block shared by install and update.
// source is the raw install argument (install) or the locked source (update).
func printPluginPlan(out io.Writer, m *plugin.Manifest, source, commitSHA string) {
	fmt.Fprintf(out, "Plugin: %s (api %d)\n", m.Name, m.APIVersion)
	fmt.Fprintf(out, "Source: %s\n", source)
	fmt.Fprintf(out, "Commit: %s\n", shortSHA(commitSHA))
	fmt.Fprintf(out, "Events: %s\n", strings.Join(m.On, ", "))
	fmt.Fprintf(out, "Run:    %s\n", m.Run)
	if m.Build != "" {
		fmt.Fprintf(out, "Build:  %s\n", m.Build)
	}
}

func confirmPlugin(cmd *cobra.Command, prompt string) bool {
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

// pluginBuildTimeout resolves plugins.build_timeout (seconds) into a Duration
// for InstallPlan.Commit.
func pluginBuildTimeout() (time.Duration, error) {
	mgr, err := config.NewManager(getConfigDir())
	if err != nil {
		return 0, err
	}
	return time.Duration(mgr.GetPluginsConfig().BuildTimeout) * time.Second, nil
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

// pluginListItem is the JSON shape for `jin plugin list --json`. plugin.Entry
// itself is not JSON-friendly (State is an int enum, Err is an interface), so
// the CLI projects it onto stable field names.
type pluginListItem struct {
	Name       string `json:"name"`
	APIVersion int    `json:"api_version"`
	State      string `json:"state"`
	Linked     bool   `json:"linked"`
	Source     string `json:"source"`
	Ref        string `json:"ref,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Error      string `json:"error,omitempty"`
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
				item.APIVersion = e.Manifest.APIVersion
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
	fmt.Fprintln(tw, "NAME\tAPI\tSTATE\tSOURCE")
	for _, e := range entries {
		api := "-"
		if e.Manifest != nil {
			api = strconv.Itoa(e.Manifest.APIVersion)
		}
		state := e.State.String()
		if e.Lock.Linked {
			state += " (linked)"
		}
		if e.Err != nil {
			state += ": " + truncateStr(e.Err.Error(), 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, api, state, e.Lock.Source)
	}
	return tw.Flush()
}
