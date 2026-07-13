package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/takaaki-s/jind-ai/internal/exitcode"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

var pluginValidateCmd = &cobra.Command{
	Use:   "validate [path]",
	Short: "Validate a plugin manifest",
	Long: `Validate a plugin manifest against the schema and registry rules.

The target defaults to the current directory. If it names a directory the
manifest is read from jind-ai-plugin.yaml; if it names a file the file is
treated as the manifest and its directory is the plugin dir.

Findings are ERROR (blocks install) or WARN (visible, non-blocking). Exit
code is 1 on any ERROR, or on any WARN with --fail-on-warning.`,
	Args:          cobra.MaximumNArgs(1),
	RunE:          runPluginValidate,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	pluginCmd.AddCommand(pluginValidateCmd)
	pluginValidateCmd.Flags().String("manifest", "", "Explicit manifest file path (overrides positional resolution)")
	pluginValidateCmd.Flags().String("registry", "", "Registry URL to check name uniqueness against (default: canonical)")
	pluginValidateCmd.Flags().Bool("skip-uniqueness", false, "Skip registry rules #9/#10 (name ownership, monotonic version)")
	pluginValidateCmd.Flags().Bool("run-build", false, "Run install.source.build and verify entrypoint (rules #13/#14)")
	pluginValidateCmd.Flags().Bool("fail-on-warning", false, "Exit 1 if any WARN is emitted")
	pluginValidateCmd.Flags().Bool("github-actions", false, "Emit GitHub Actions annotations and $GITHUB_STEP_SUMMARY")
}

func runPluginValidate(cmd *cobra.Command, args []string) error {
	manifestFlag, _ := cmd.Flags().GetString("manifest")
	registryURL, _ := cmd.Flags().GetString("registry")
	skipUniq, _ := cmd.Flags().GetBool("skip-uniqueness")
	runBuild, _ := cmd.Flags().GetBool("run-build")
	failOnWarn, _ := cmd.Flags().GetBool("fail-on-warning")
	gha, _ := cmd.Flags().GetBool("github-actions")

	target := "."
	if len(args) > 0 {
		target = args[0]
	}

	manifestPath, pluginDir, err := resolveValidateTarget(target, manifestFlag)
	if err != nil {
		return exitcode.Wrap(err, exitcode.GeneralError, "resolve validate target")
	}

	out := cmd.OutOrStdout()
	findings := collectValidateFindings(out, manifestPath, pluginDir, registryURL, skipUniq, runBuild)

	if gha {
		printGithubAnnotations(out, findings, manifestPath)
		if err := writeGithubStepSummary(findings, manifestPath); err != nil {
			fmt.Fprintf(out, "warning: failed to write $GITHUB_STEP_SUMMARY: %v\n", err)
		}
	} else {
		printFindingsTable(out, findings)
	}

	if manifest.HasErrors(findings) {
		return exitcode.Errorf(exitcode.GeneralError, "validation failed")
	}
	if failOnWarn && manifest.HasWarnings(findings) {
		return exitcode.Errorf(exitcode.GeneralError, "validation produced warnings")
	}
	return nil
}

func collectValidateFindings(out io.Writer, manifestPath, pluginDir, registryURL string, skipUniq, runBuild bool) []manifest.Finding {
	data, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		msg := fmt.Sprintf("cannot read manifest at %s: %v", manifestPath, readErr)
		if errors.Is(readErr, fs.ErrNotExist) {
			msg = fmt.Sprintf("%s not found at %s", manifest.Filename, manifestPath)
		}
		return []manifest.Finding{{
			Rule:     manifest.RuleManifestExists,
			Severity: manifest.SeverityError,
			Message:  msg,
		}}
	}

	m, unknown, parseErr := manifest.Parse(data)
	if parseErr != nil {
		return []manifest.Finding{{
			Rule:     manifest.RuleYAMLValid,
			Severity: manifest.SeverityError,
			Message:  parseErr.Error(),
		}}
	}

	opts := manifest.CheckOptions{
		PluginDir:     pluginDir,
		UnknownFields: unknown,
		OwnerRepo:     os.Getenv("GITHUB_REPOSITORY"),
	}
	var findings []manifest.Finding
	if !skipUniq {
		reg, err := loadRegistryLookup(registryURL)
		if err != nil {
			findings = append(findings, manifest.Finding{
				Rule:     manifest.RuleNameOwnership,
				Severity: manifest.SeverityWarning,
				Message:  fmt.Sprintf("registry lookup unavailable (uniqueness rules skipped): %v", err),
				Field:    "name",
			})
		} else {
			opts.Registry = reg
		}
	}
	findings = append(findings, manifest.Check(m, opts)...)

	if runBuild {
		findings = append(findings, runBuildChecks(out, m, pluginDir)...)
	}
	return findings
}

// resolveValidateTarget maps the CLI-provided path (and optional --manifest
// override) to an absolute manifest file and its enclosing plugin directory.
// A missing path is treated as a directory so rule #1 emits a clear "manifest
// not found" instead of a stat error.
func resolveValidateTarget(target, manifestFlag string) (manifestPath, pluginDir string, err error) {
	if manifestFlag != "" {
		abs, err := filepath.Abs(manifestFlag)
		if err != nil {
			return "", "", err
		}
		return abs, filepath.Dir(abs), nil
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	info, statErr := os.Stat(abs)
	if statErr != nil || info.IsDir() {
		return filepath.Join(abs, manifest.Filename), abs, nil
	}
	return abs, filepath.Dir(abs), nil
}

// registryLookupAdapter projects a fetched RegistryDocument onto the
// RegistryLookup interface Check expects. Keeping it local to the CLI keeps
// the pkg-level type free of a repo-vs-owner naming decision that only the
// caller cares about.
type registryLookupAdapter struct {
	doc *manifest.RegistryDocument
}

func (a registryLookupAdapter) Lookup(name string) (owner string, latestVersion string, err error) {
	entry := a.doc.Find(name)
	if entry == nil {
		return "", "", nil
	}
	return entry.Repo, entry.LatestVersion, nil
}

func loadRegistryLookup(url string) (manifest.RegistryLookup, error) {
	doc, _, err := loadRegistryDocument(url, false)
	if err != nil {
		return nil, err
	}
	return registryLookupAdapter{doc: doc}, nil
}

// runBuildChecks executes install.source.build in the plugin directory and
// asserts the declared entrypoint materialises. Any build command failure is
// a hard rule #13 ERROR — the remaining commands are skipped because a broken
// pipeline cannot produce a meaningful entrypoint check.
func runBuildChecks(out io.Writer, m *manifest.Manifest, pluginDir string) []manifest.Finding {
	if m.Install.Source == nil {
		return nil
	}
	timeout := m.EffectiveTimeout()
	if timeout < time.Minute {
		timeout = 5 * time.Minute
	}
	for _, cmdStr := range m.BuildCommands() {
		fmt.Fprintf(out, "[build] %s\n", cmdStr)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		c := exec.CommandContext(ctx, "bash", "-c", cmdStr)
		c.Dir = pluginDir
		c.Stdout = out
		c.Stderr = out
		runErr := c.Run()
		cancel()
		if runErr != nil {
			return []manifest.Finding{{
				Rule:     manifest.RuleBuildExec,
				Severity: manifest.SeverityError,
				Message:  fmt.Sprintf("build command %q failed: %v", cmdStr, runErr),
				Field:    "install.source.build",
			}}
		}
	}
	entry := m.Entrypoint()
	if entry == "" {
		return nil
	}
	entryPath := entry
	if !filepath.IsAbs(entryPath) {
		entryPath = filepath.Join(pluginDir, entryPath)
	}
	if _, err := os.Stat(entryPath); err != nil {
		return []manifest.Finding{{
			Rule:     manifest.RuleEntrypointExists,
			Severity: manifest.SeverityError,
			Message:  fmt.Sprintf("entrypoint %q not found after build: %v", entry, err),
			Field:    "install.source.entrypoint",
		}}
	}
	return nil
}

var (
	validateStyleError = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	validateStyleWarn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	validateStyleOK    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46"))
)

func printFindingsTable(out io.Writer, findings []manifest.Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(out, validateStyleOK.Render("OK")+" no issues found")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tRULE\tFIELD\tMESSAGE")
	for _, f := range findings {
		sev := f.Severity.String()
		switch f.Severity {
		case manifest.SeverityError:
			sev = validateStyleError.Render(sev)
		case manifest.SeverityWarning:
			sev = validateStyleWarn.Render(sev)
		}
		field := f.Field
		if field == "" {
			field = "-"
		}
		fmt.Fprintf(tw, "%s\tR%d\t%s\t%s\n", sev, f.Rule, field, f.Message)
	}
	_ = tw.Flush()
	errN, warnN := countFindings(findings)
	fmt.Fprintf(out, "\n%d ERROR, %d WARN\n", errN, warnN)
}

func countFindings(findings []manifest.Finding) (errN, warnN int) {
	for _, f := range findings {
		switch f.Severity {
		case manifest.SeverityError:
			errN++
		case manifest.SeverityWarning:
			warnN++
		}
	}
	return
}

// printGithubAnnotations writes one workflow command per finding. The line
// slot is omitted because Finding does not carry YAML positions yet; GHA
// still surfaces the annotation, anchored at the top of the file, and the
// title makes the offending field obvious.
func printGithubAnnotations(out io.Writer, findings []manifest.Finding, manifestPath string) {
	file := filepath.Base(manifestPath)
	if file == "" || file == "." {
		file = manifest.Filename
	}
	for _, f := range findings {
		level := "error"
		if f.Severity == manifest.SeverityWarning {
			level = "warning"
		}
		title := fmt.Sprintf("R%d", f.Rule)
		if f.Field != "" {
			title = fmt.Sprintf("R%d %s", f.Rule, f.Field)
		}
		fmt.Fprintf(out, "::%s file=%s,title=%s::%s\n",
			level, file, ghaValueEscaper.Replace(title), ghaValueEscaper.Replace(f.Message))
	}
}

// ghaValueEscaper applies the workflow-command escape sequence. Without it a
// message containing `%`, `\r`, or `\n` splits the annotation or corrupts the
// value in GHA's UI. Order matters for `\r\n`: a lone `\r` and a lone `\n`
// both map to their own escapes, so the pair round-trips as %0D%0A.
var ghaValueEscaper = strings.NewReplacer(
	"%", "%25",
	"\r", "%0D",
	"\n", "%0A",
)

func writeGithubStepSummary(findings []manifest.Finding, manifestPath string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("### `jin plugin validate` — `")
	b.WriteString(filepath.Base(manifestPath))
	b.WriteString("`\n\n")
	if len(findings) == 0 {
		b.WriteString(":white_check_mark: no issues found\n")
	} else {
		b.WriteString("| Severity | Rule | Field | Message |\n")
		b.WriteString("|---|---|---|---|\n")
		for _, f := range findings {
			field := f.Field
			if field == "" {
				field = "-"
			}
			fmt.Fprintf(&b, "| %s | R%d | %s | %s |\n",
				f.Severity.String(), f.Rule, markdownCellEscaper.Replace(field), markdownCellEscaper.Replace(f.Message))
		}
		errN, warnN := countFindings(findings)
		fmt.Fprintf(&b, "\n**%d ERROR, %d WARN**\n", errN, warnN)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(b.String())
	return err
}

// markdownCellEscaper defuses the two characters that break a table row: `|`
// (column separator) and newlines (row separator). `\r\n` is listed first so
// a CRLF collapses to one `<br>` instead of two.
var markdownCellEscaper = strings.NewReplacer(
	"|", `\|`,
	"\r\n", "<br>",
	"\n", "<br>",
)
