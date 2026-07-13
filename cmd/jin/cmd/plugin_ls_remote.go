package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/takaaki-s/jind-ai/internal/exitcode"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

var pluginLsRemoteCmd = &cobra.Command{
	Use:   "ls-remote",
	Short: "List plugins from the remote registry",
	Long: `List plugins available in the jind-ai plugin registry.

The registry document is cached locally for 24 hours; use --refresh to bypass
the freshness check (the client still sends conditional headers so an unchanged
registry is a cheap 304).`,
	Args:          cobra.NoArgs,
	RunE:          runPluginLsRemote,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	pluginCmd.AddCommand(pluginLsRemoteCmd)
	pluginLsRemoteCmd.Flags().String("registry", "", "Registry URL (default: canonical)")
	pluginLsRemoteCmd.Flags().String("sort", "name", "Sort order: 'name' or 'updated'")
	pluginLsRemoteCmd.Flags().String("search", "", "Filter by substring in name or description")
	pluginLsRemoteCmd.Flags().Bool("refresh", false, "Bypass local cache freshness")
}

func runPluginLsRemote(cmd *cobra.Command, _ []string) error {
	registryURL, _ := cmd.Flags().GetString("registry")
	sortKey, _ := cmd.Flags().GetString("sort")
	search, _ := cmd.Flags().GetString("search")
	refresh, _ := cmd.Flags().GetBool("refresh")

	if sortKey != "name" && sortKey != "updated" {
		return exitcode.Errorf(exitcode.GeneralError, "invalid --sort value %q (expected 'name' or 'updated')", sortKey)
	}

	doc, result, err := loadRegistryDocument(registryURL, refresh)
	if err != nil {
		return exitcode.Wrap(err, exitcode.GeneralError, "fetch registry")
	}
	if result.Outcome == manifest.OutcomeCacheFallback && result.FetchErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: registry fetch failed, using cache: %v\n", result.FetchErr)
	}

	entries := filterRegistryEntries(doc.Plugins, search)
	sortRegistryEntries(entries, sortKey)

	out := cmd.OutOrStdout()
	if jsonOutput {
		return writeJSON(out, entries)
	}
	printLsRemoteTable(out, entries)
	return nil
}

func loadRegistryDocument(url string, refresh bool) (*manifest.RegistryDocument, manifest.LoadResult, error) {
	client, err := manifest.NewClient(manifest.ClientConfig{
		URL:      url,
		CacheDir: filepath.Join(paths.State(), "registry"),
	})
	if err != nil {
		return nil, manifest.LoadResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.Load(ctx, manifest.LoadOptions{Refresh: refresh})
}

// filterRegistryEntries returns entries whose name or description contains
// search (case-insensitive). Callers may sort the result in place. Always
// returns a non-nil slice so `--json` emits `[]` rather than `null`.
func filterRegistryEntries(entries []manifest.RegistryEntry, search string) []manifest.RegistryEntry {
	if search == "" {
		if entries == nil {
			return []manifest.RegistryEntry{}
		}
		return entries
	}
	q := strings.ToLower(search)
	out := make([]manifest.RegistryEntry, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name), q) || strings.Contains(strings.ToLower(e.Description), q) {
			out = append(out, e)
		}
	}
	return out
}

func sortRegistryEntries(entries []manifest.RegistryEntry, key string) {
	if key == "updated" {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
		})
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
}

func printLsRemoteTable(out io.Writer, entries []manifest.RegistryEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(out, "No plugins found.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLATEST\tUPDATED\tREPO")
	for _, e := range entries {
		updated := "-"
		if !e.UpdatedAt.IsZero() {
			updated = e.UpdatedAt.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, e.LatestVersion, updated, e.Repo)
	}
	_ = tw.Flush()
}
