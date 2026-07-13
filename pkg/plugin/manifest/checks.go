package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Severity classifies a Finding. Errors block registration; warnings are
// visible but non-blocking (unless the caller passes --fail-on-warning).
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "ERROR"
	case SeverityWarning:
		return "WARN"
	default:
		return "UNKNOWN"
	}
}

// RuleID identifies a validation rule from the table in
// docs/plugin-registry.md. Values match the numbering in that table so
// diagnostics and CI annotations line up with the spec.
type RuleID int

const (
	RuleManifestExists      RuleID = 1
	RuleYAMLValid           RuleID = 2
	RuleSchemaVersion       RuleID = 3
	RuleRequiredFields      RuleID = 4
	RuleNamePattern         RuleID = 5
	RuleVersionSemver       RuleID = 6
	RuleJinRangeParses      RuleID = 7
	RuleInstallXOR          RuleID = 8
	RuleNameOwnership       RuleID = 9
	RuleVersionMonotonic    RuleID = 10
	RuleLicenseFile         RuleID = 11
	RuleReadmeMinimal       RuleID = 12
	RuleBuildRuns           RuleID = 13
	RuleEntrypointExists    RuleID = 14
	RuleOnMatcher           RuleID = 15
	RulePopupBounds         RuleID = 16
	RuleUnknownFieldWarning RuleID = 100 // synthetic; forward-compat WARN
)

// Finding is one validation result. Field points at the offending YAML key
// (dot-notation, e.g. "install.source.entrypoint") when applicable.
type Finding struct {
	Rule     RuleID
	Severity Severity
	Message  string
	Field    string
}

func (f Finding) String() string {
	if f.Field != "" {
		return fmt.Sprintf("[%s R%d %s] %s", f.Severity, f.Rule, f.Field, f.Message)
	}
	return fmt.Sprintf("[%s R%d] %s", f.Severity, f.Rule, f.Message)
}

// RegistryLookup answers uniqueness and monotonic-version questions used by
// rules #9 and #10. Implementations live in J2 (registry client) and in
// test fakes; passing nil skips these two rules.
type RegistryLookup interface {
	// Lookup returns the current owner repo (e.g. "user/repo") and latest
	// version registered under name. Both are empty if the name is not
	// registered. An error signals a lookup failure — the caller decides
	// whether that becomes a WARN or a skipped rule.
	Lookup(name string) (owner string, latestVersion string, err error)
}

// CheckOptions bundles the moving parts of a check run: the plugin directory
// (needed for file-existence rules), a registry lookup (nil = skip network
// checks), the caller's own repo identity for rule #9, whether to actually
// execute the build (rules #13/#14, opt-in), and any unknown fields carried
// over from Parse.
type CheckOptions struct {
	PluginDir     string
	Registry      RegistryLookup
	OwnerRepo     string
	RunBuild      bool
	UnknownFields []string
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

// NamePattern is exposed so callers outside this package (installers, remove
// paths) can enforce the same grammar without duplicating the regex.
var NamePattern = namePattern

// Check runs every validation rule that applies to an already-parsed
// manifest. It is safe to call with a partially populated Manifest; missing
// fields surface as R4 (required-field) or per-field findings.
func Check(m *Manifest, opts CheckOptions) []Finding {
	var findings []Finding

	for _, f := range opts.UnknownFields {
		findings = append(findings, Finding{
			Rule:     RuleUnknownFieldWarning,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("unknown field %q (ignored for forward compatibility)", f),
			Field:    f,
		})
	}

	findings = append(findings, checkSchemaVersion(m)...)
	findings = append(findings, checkRequired(m)...)
	findings = append(findings, checkName(m)...)
	findings = append(findings, checkVersion(m)...)
	findings = append(findings, checkJinRange(m)...)
	findings = append(findings, checkInstallXOR(m)...)
	findings = append(findings, checkOn(m)...)
	findings = append(findings, checkPopup(m)...)

	if opts.Registry != nil {
		findings = append(findings, checkRegistry(m, opts.Registry, opts.OwnerRepo)...)
	}

	if opts.PluginDir != "" {
		findings = append(findings, checkLicenseFile(opts.PluginDir)...)
		findings = append(findings, checkReadme(opts.PluginDir)...)
	}

	if opts.RunBuild {
		findings = append(findings, checkBuildRuns(m, opts.PluginDir)...)
	}

	return findings
}

// Validate is a convenience for callers that just want a hard error from the
// rules that block install (matcher grammar, install shape, name, semver,
// popup bounds) without threading Findings through their code. It skips
// network and file-system rules; use Check for the full report.
func Validate(m *Manifest) error {
	for _, f := range Check(m, CheckOptions{}) {
		if f.Severity == SeverityError {
			return errors.New(f.Message)
		}
	}
	return nil
}

func checkSchemaVersion(m *Manifest) []Finding {
	if m.SchemaVersion < MinSchemaVersion || m.SchemaVersion > CurrentSchemaVersion {
		return []Finding{{
			Rule:     RuleSchemaVersion,
			Severity: SeverityError,
			Message: fmt.Sprintf("schema_version %d not supported (this build accepts %d..%d)",
				m.SchemaVersion, MinSchemaVersion, CurrentSchemaVersion),
			Field: "schema_version",
		}}
	}
	return nil
}

func checkRequired(m *Manifest) []Finding {
	var findings []Finding
	missing := func(field string) Finding {
		return Finding{
			Rule:     RuleRequiredFields,
			Severity: SeverityError,
			Message:  fmt.Sprintf("required field %q is missing", field),
			Field:    field,
		}
	}
	if m.Name == "" {
		findings = append(findings, missing("name"))
	}
	if m.Version == "" {
		findings = append(findings, missing("version"))
	}
	if m.Description == "" {
		findings = append(findings, Finding{
			Rule:     RuleRequiredFields,
			Severity: SeverityWarning,
			Message:  "description is empty",
			Field:    "description",
		})
	}
	if m.Jin == "" {
		findings = append(findings, missing("jin"))
	}
	if m.Install.Source == nil && m.Install.ReleaseAsset == nil {
		findings = append(findings, missing("install"))
	}
	return findings
}

func checkName(m *Manifest) []Finding {
	if m.Name == "" {
		return nil // already reported by required-fields
	}
	if !namePattern.MatchString(m.Name) {
		return []Finding{{
			Rule:     RuleNamePattern,
			Severity: SeverityError,
			Message: fmt.Sprintf("name %q must match %s (lowercase, starts with letter, 2-64 chars)",
				m.Name, namePattern.String()),
			Field: "name",
		}}
	}
	return nil
}

func checkVersion(m *Manifest) []Finding {
	if m.Version == "" {
		return nil
	}
	if _, err := semver.StrictNewVersion(m.Version); err != nil {
		return []Finding{{
			Rule:     RuleVersionSemver,
			Severity: SeverityError,
			Message:  fmt.Sprintf("version %q is not valid semver: %v", m.Version, err),
			Field:    "version",
		}}
	}
	return nil
}

func checkJinRange(m *Manifest) []Finding {
	if m.Jin == "" {
		return nil
	}
	if _, err := semver.NewConstraint(m.Jin); err != nil {
		return []Finding{{
			Rule:     RuleJinRangeParses,
			Severity: SeverityError,
			Message:  fmt.Sprintf("jin %q is not a valid semver constraint: %v", m.Jin, err),
			Field:    "jin",
		}}
	}
	return nil
}

func checkInstallXOR(m *Manifest) []Finding {
	hasSource := m.Install.Source != nil
	hasAsset := m.Install.ReleaseAsset != nil
	switch {
	case hasSource && hasAsset:
		return []Finding{{
			Rule:     RuleInstallXOR,
			Severity: SeverityError,
			Message:  "install.source and install.release_asset are mutually exclusive; pick one",
			Field:    "install",
		}}
	case hasSource:
		return checkSourceInstall(m.Install.Source)
	case hasAsset:
		return checkReleaseAsset(m.Install.ReleaseAsset)
	default:
		return nil // required-field ERROR already reported
	}
}

func checkSourceInstall(s *SourceInstall) []Finding {
	var findings []Finding
	if len(s.Build) == 0 {
		findings = append(findings, Finding{
			Rule:     RuleRequiredFields,
			Severity: SeverityError,
			Message:  "install.source.build must have at least one command",
			Field:    "install.source.build",
		})
	}
	if s.Entrypoint == "" {
		findings = append(findings, Finding{
			Rule:     RuleRequiredFields,
			Severity: SeverityError,
			Message:  "install.source.entrypoint is required",
			Field:    "install.source.entrypoint",
		})
	}
	return findings
}

var releaseAssetPlaceholder = regexp.MustCompile(`\{([a-zA-Z_]+)\}`)

func checkReleaseAsset(a *ReleaseAssetInstall) []Finding {
	if a.Pattern == "" {
		return []Finding{{
			Rule:     RuleRequiredFields,
			Severity: SeverityError,
			Message:  "install.release_asset.pattern is required",
			Field:    "install.release_asset.pattern",
		}}
	}
	var findings []Finding
	for _, m := range releaseAssetPlaceholder.FindAllStringSubmatch(a.Pattern, -1) {
		switch m[1] {
		case "os", "arch":
			// allowed
		default:
			findings = append(findings, Finding{
				Rule:     RuleRequiredFields,
				Severity: SeverityWarning,
				Message: fmt.Sprintf("install.release_asset.pattern uses unknown placeholder %q (only {os} and {arch} are supported)",
					"{"+m[1]+"}"),
				Field: "install.release_asset.pattern",
			})
		}
	}
	return findings
}

func checkOn(m *Manifest) []Finding {
	var findings []Finding
	for _, matcher := range m.On {
		if err := ValidateMatcher(matcher); err != nil {
			findings = append(findings, Finding{
				Rule:     RuleOnMatcher,
				Severity: SeverityError,
				Message:  fmt.Sprintf("invalid on matcher: %v", err),
				Field:    "on",
			})
		}
	}
	return findings
}

func checkPopup(m *Manifest) []Finding {
	if m.Popup == nil {
		return nil
	}
	var findings []Finding
	if m.Popup.Width != 0 && (m.Popup.Width < 1 || m.Popup.Width > 100) {
		findings = append(findings, Finding{
			Rule:     RulePopupBounds,
			Severity: SeverityError,
			Message:  fmt.Sprintf("popup.width must be 1-100 (got %d)", m.Popup.Width),
			Field:    "popup.width",
		})
	}
	if m.Popup.Height != 0 && (m.Popup.Height < 1 || m.Popup.Height > 100) {
		findings = append(findings, Finding{
			Rule:     RulePopupBounds,
			Severity: SeverityError,
			Message:  fmt.Sprintf("popup.height must be 1-100 (got %d)", m.Popup.Height),
			Field:    "popup.height",
		})
	}
	return findings
}

func checkRegistry(m *Manifest, reg RegistryLookup, ownerRepo string) []Finding {
	if m.Name == "" {
		return nil
	}
	owner, latest, err := reg.Lookup(m.Name)
	if err != nil {
		return []Finding{{
			Rule:     RuleNameOwnership,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("could not consult registry for name %q: %v", m.Name, err),
			Field:    "name",
		}}
	}
	var findings []Finding
	if owner != "" && ownerRepo != "" && owner != ownerRepo {
		findings = append(findings, Finding{
			Rule:     RuleNameOwnership,
			Severity: SeverityError,
			Message: fmt.Sprintf("name %q is already registered to %s; pick a different name",
				m.Name, owner),
			Field: "name",
		})
	}
	if latest != "" && m.Version != "" {
		cur, err1 := semver.StrictNewVersion(m.Version)
		prev, err2 := semver.StrictNewVersion(latest)
		if err1 == nil && err2 == nil && !cur.GreaterThan(prev) {
			findings = append(findings, Finding{
				Rule:     RuleVersionMonotonic,
				Severity: SeverityWarning,
				Message: fmt.Sprintf("version %s is not greater than currently registered %s",
					m.Version, latest),
				Field: "version",
			})
		}
	}
	return findings
}

func checkLicenseFile(pluginDir string) []Finding {
	for _, candidate := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING"} {
		if _, err := os.Stat(filepath.Join(pluginDir, candidate)); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return []Finding{{
				Rule:     RuleLicenseFile,
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("could not stat %s: %v", candidate, err),
			}}
		}
	}
	return []Finding{{
		Rule:     RuleLicenseFile,
		Severity: SeverityWarning,
		Message:  "no LICENSE file found (LICENSE, LICENSE.md, LICENSE.txt, COPYING)",
	}}
}

var readmeHeadingPattern = regexp.MustCompile(`(?im)^\s*#{1,6}\s*(install|usage|getting started|quick ?start)\b`)

func checkReadme(pluginDir string) []Finding {
	path := filepath.Join(pluginDir, "README.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Finding{{
				Rule:     RuleReadmeMinimal,
				Severity: SeverityWarning,
				Message:  "README.md not found",
			}}
		}
		return []Finding{{
			Rule:     RuleReadmeMinimal,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("could not read README.md: %v", err),
		}}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return []Finding{{
			Rule:     RuleReadmeMinimal,
			Severity: SeverityWarning,
			Message:  "README.md is empty",
		}}
	}
	if !readmeHeadingPattern.MatchString(string(data)) {
		return []Finding{{
			Rule:     RuleReadmeMinimal,
			Severity: SeverityWarning,
			Message:  "README.md is missing an Install or Usage heading",
		}}
	}
	return nil
}

// checkBuildRuns is the opt-in build execution stub (rules #13/#14). The
// full implementation lives with the plugin validate command (J3) where an
// executor, working directory, and process supervision are available; here
// we return a placeholder WARN so the interface exists and tests can assert
// the rule is dispatched.
func checkBuildRuns(m *Manifest, pluginDir string) []Finding {
	if m.Install.Source == nil {
		return nil
	}
	if pluginDir == "" {
		return []Finding{{
			Rule:     RuleBuildRuns,
			Severity: SeverityError,
			Message:  "--run-build requires a plugin directory",
		}}
	}
	return []Finding{{
		Rule:     RuleBuildRuns,
		Severity: SeverityWarning,
		Message:  "--run-build execution is not yet wired in (stub)",
	}}
}

// HasErrors reports whether the finding list contains at least one ERROR.
// Callers use it as the exit-code discriminator alongside --fail-on-warning.
func HasErrors(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// HasWarnings reports whether the finding list contains at least one WARN.
func HasWarnings(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

// CheckJinCompat checks whether the manifest's jin constraint is satisfied
// by jinVersion. jinVersion may be empty or a development string like "dev";
// in that case the check is skipped (returns nil) so local development
// against an unstamped binary is not blocked. A non-parsable constraint
// returns an error; a valid constraint that jinVersion does not satisfy
// returns an error naming both sides so the caller can produce a useful
// message.
func CheckJinCompat(constraint, jinVersion string) error {
	if constraint == "" {
		return errors.New("plugin manifest is missing a jin compat range")
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return fmt.Errorf("plugin manifest jin %q: %w", constraint, err)
	}
	if jinVersion == "" || jinVersion == "dev" {
		return nil
	}
	v, err := semver.NewVersion(jinVersion)
	if err != nil {
		return nil // unstamped / non-semver dev build: skip the check
	}
	if !c.Check(v) {
		return fmt.Errorf("plugin requires jin %s but this build is %s (update jin or the plugin)",
			constraint, jinVersion)
	}
	return nil
}
