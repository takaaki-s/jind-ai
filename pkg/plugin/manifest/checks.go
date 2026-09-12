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
	RuleBuildExec           RuleID = 13 // opt-in via `plugin validate --run-build`
	RuleEntrypointExists    RuleID = 14 // opt-in via `plugin validate --run-build`
	RuleOnMatcher           RuleID = 15
	RulePopupBounds         RuleID = 16
	RuleActionID            RuleID = 17
	RuleActionDuplicateID   RuleID = 18
	RuleActionEntrypoint    RuleID = 19
	RuleV2Constraint        RuleID = 20  // schema v2 forbids top-level entrypoint / on / popup
	RuleActionsRequired     RuleID = 21  // schema v2 requires at least one action
	RuleListenerRequiresOn  RuleID = 22  // listener: true actions must declare a non-empty on
	RuleHandoffActionOnly   RuleID = 23  // handoff actions are explicit synchronous endpoints
	RuleUnknownFieldWarning RuleID = 100 // synthetic; forward-compat WARN, out of the spec table range
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
// checks), the caller's own repo identity for rule #9, and any unknown
// fields carried over from Parse. The opt-in build execution (rules #13/#14)
// runs in the `plugin validate --run-build` command, which appends its own
// findings on top of Check's output.
type CheckOptions struct {
	PluginDir     string
	Registry      RegistryLookup
	OwnerRepo     string
	UnknownFields []string
}

// NamePattern is the grammar plugin names must match. Exported so
// installers, remove paths, and any external tooling enforce the exact
// same rule as manifest validation.
var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

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
	findings = append(findings, checkV2Constraints(m)...)
	findings = append(findings, checkActionsRequired(m)...)
	findings = append(findings, checkActionIDs(m)...)
	findings = append(findings, checkActionEntrypoints(m)...)
	findings = append(findings, checkActionsOn(m)...)
	findings = append(findings, checkActionsPopup(m)...)
	findings = append(findings, checkListenerRequiresOn(m)...)
	findings = append(findings, checkHandoffActions(m)...)
	findings = append(findings, checkOn(m)...)
	findings = append(findings, checkPopup(m)...)

	if opts.Registry != nil && NamePattern.MatchString(m.Name) {
		findings = append(findings, checkRegistry(m, opts.Registry, opts.OwnerRepo)...)
	}

	if opts.PluginDir != "" {
		findings = append(findings, checkLicenseFile(opts.PluginDir)...)
		findings = append(findings, checkReadme(opts.PluginDir)...)
	}

	return findings
}

func checkHandoffActions(m *Manifest) []Finding {
	var findings []Finding
	for i, a := range m.Actions {
		if !a.Handoff && !a.MergeHandoff {
			continue
		}
		ref := actionRef(i, a.ID)
		if a.Handoff && a.MergeHandoff {
			findings = append(findings, Finding{
				Rule: RuleHandoffActionOnly, Severity: SeverityError,
				Field: ref + ".merge_handoff", Message: "an action cannot provide both PR and merge handoff contracts",
			})
		}
		capabilityField := ref + ".handoff"
		if a.MergeHandoff && !a.Handoff {
			capabilityField = ref + ".merge_handoff"
		}
		if a.Listener {
			findings = append(findings, Finding{
				Rule: RuleHandoffActionOnly, Severity: SeverityError,
				Field: capabilityField, Message: "handoff actions cannot also be listeners",
			})
		}
		if len(a.On) != 0 {
			findings = append(findings, Finding{
				Rule: RuleHandoffActionOnly, Severity: SeverityError,
				Field: ref + ".on", Message: "handoff actions cannot subscribe to events",
			})
		}
		if a.Popup != nil {
			findings = append(findings, Finding{
				Rule: RuleHandoffActionOnly, Severity: SeverityError,
				Field: ref + ".popup", Message: "handoff actions cannot open interactive popups",
			})
		}
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
			Message: fmt.Sprintf("schema_version %d not supported (this build accepts %d-%d)",
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
	if !NamePattern.MatchString(m.Name) {
		return []Finding{{
			Rule:     RuleNamePattern,
			Severity: SeverityError,
			Message: fmt.Sprintf("name %q must match %s (lowercase, starts with letter, 2-64 chars)",
				m.Name, NamePattern.String()),
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
		// v2 moves the entrypoint to actions[]; a v2 source install with only
		// build steps is legitimate. Top-level entrypoint on v2 is forbidden by
		// checkV2Constraints, and per-action entrypoint requirement is enforced
		// by checkActionEntrypoints, so no source-level check applies here.
		if m.SchemaVersion >= 2 {
			return nil
		}
		return checkSourceInstall(m.Install.Source)
	case hasAsset:
		return checkReleaseAsset(m.Install.ReleaseAsset)
	default:
		return nil // required-field ERROR already reported
	}
}

func checkSourceInstall(s *SourceInstall) []Finding {
	if s.Entrypoint == "" {
		return []Finding{{
			Rule:     RuleRequiredFields,
			Severity: SeverityError,
			Message:  "install.source.entrypoint is required",
			Field:    "install.source.entrypoint",
		}}
	}
	return nil
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
	return onMatcherFindings(m.On, "on")
}

// onMatcherFindings runs ValidateMatcher over every matcher and emits an ERROR
// per invalid entry. field scopes Finding.Field so callers can distinguish
// top-level `on` from per-action `actions[<id>].on`.
func onMatcherFindings(on []string, field string) []Finding {
	var findings []Finding
	for _, matcher := range on {
		if err := ValidateMatcher(matcher); err != nil {
			findings = append(findings, Finding{
				Rule:     RuleOnMatcher,
				Severity: SeverityError,
				Message:  fmt.Sprintf("invalid on matcher: %v", err),
				Field:    field,
			})
		}
	}
	return findings
}

func checkPopup(m *Manifest) []Finding {
	return popupBoundsFindings(m.Popup, "popup")
}

// popupBoundsFindings emits per-axis ERRORs for popup dimensions outside 1-100.
// A zero axis is treated as "unset" and skipped. fieldPrefix scopes the
// Finding.Field so callers can distinguish top-level popup from per-action.
func popupBoundsFindings(p *PopupConfig, fieldPrefix string) []Finding {
	if p == nil {
		return nil
	}
	var findings []Finding
	if p.Width != 0 && (p.Width < 1 || p.Width > 100) {
		findings = append(findings, Finding{
			Rule:     RulePopupBounds,
			Severity: SeverityError,
			Message:  fmt.Sprintf("%s.width must be 1-100 (got %d)", fieldPrefix, p.Width),
			Field:    fieldPrefix + ".width",
		})
	}
	if p.Height != 0 && (p.Height < 1 || p.Height > 100) {
		findings = append(findings, Finding{
			Rule:     RulePopupBounds,
			Severity: SeverityError,
			Message:  fmt.Sprintf("%s.height must be 1-100 (got %d)", fieldPrefix, p.Height),
			Field:    fieldPrefix + ".height",
		})
	}
	return findings
}

// actionIDPattern is the grammar for actions[].id: lowercase, starts with a
// letter, up to 32 chars total. Kept intentionally narrower than NamePattern
// because action ids are also used as CLI arg tokens.
var actionIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// checkV2Constraints enforces that a v2 manifest does not carry top-level
// entrypoint / on / popup — those fields belong under actions[] in v2. v1
// manifests are unaffected and continue to use the top-level forms.
func checkV2Constraints(m *Manifest) []Finding {
	if m.SchemaVersion != 2 {
		return nil
	}
	forbid := func(field, msg string) Finding {
		return Finding{Rule: RuleV2Constraint, Severity: SeverityError, Field: field, Message: msg}
	}
	var findings []Finding
	if m.Install.Source != nil && m.Install.Source.Entrypoint != "" {
		findings = append(findings, forbid("install.source.entrypoint",
			"install.source.entrypoint is not allowed in schema_version 2 (move to actions[].entrypoint)"))
	}
	if len(m.On) > 0 {
		findings = append(findings, forbid("on",
			`top-level "on" is not allowed in schema_version 2 (move to actions[].on)`))
	}
	if m.Popup != nil {
		findings = append(findings, forbid("popup",
			`top-level "popup" is not allowed in schema_version 2 (move to actions[].popup)`))
	}
	return findings
}

// checkActionsRequired enforces at least one action for v2 manifests. v1
// manifests reach here with Actions synthesized by normalize(); if that
// synthesis was skipped (missing entrypoint) the required-field check
// already reports the underlying cause.
func checkActionsRequired(m *Manifest) []Finding {
	if m.SchemaVersion != 2 || len(m.Actions) > 0 {
		return nil
	}
	return []Finding{{
		Rule:     RuleActionsRequired,
		Severity: SeverityError,
		Message:  "schema_version 2 requires at least one action",
		Field:    "actions",
	}}
}

func checkActionIDs(m *Manifest) []Finding {
	var findings []Finding
	seen := make(map[string]bool, len(m.Actions))
	for i, a := range m.Actions {
		field := fmt.Sprintf("actions[%d].id", i)
		if !actionIDPattern.MatchString(a.ID) {
			findings = append(findings, Finding{
				Rule:     RuleActionID,
				Severity: SeverityError,
				Message:  fmt.Sprintf("action id %q must match %s", a.ID, actionIDPattern.String()),
				Field:    field,
			})
			continue
		}
		if seen[a.ID] {
			findings = append(findings, Finding{
				Rule:     RuleActionDuplicateID,
				Severity: SeverityError,
				Message:  fmt.Sprintf("duplicate action id %q", a.ID),
				Field:    field,
			})
			continue
		}
		seen[a.ID] = true
	}
	return findings
}

// actionRef returns the field-path prefix for an action ("actions[<id>]" when
// the action has an id, "actions[<index>]" otherwise). Diagnostics that name
// an action route through this helper so the id-vs-index fallback is defined
// in one place — Message-side rendering follows the same rule inline.
func actionRef(i int, id string) string {
	if id != "" {
		return "actions[" + id + "]"
	}
	return fmt.Sprintf("actions[%d]", i)
}

func checkActionEntrypoints(m *Manifest) []Finding {
	var findings []Finding
	for i, a := range m.Actions {
		if a.Entrypoint != "" {
			continue
		}
		ref := actionRef(i, a.ID)
		message := fmt.Sprintf("action at %s is missing entrypoint", ref)
		if a.ID != "" {
			message = fmt.Sprintf("action %q is missing entrypoint", a.ID)
		}
		findings = append(findings, Finding{
			Rule:     RuleActionEntrypoint,
			Severity: SeverityError,
			Message:  message,
			Field:    ref + ".entrypoint",
		})
	}
	return findings
}

func checkActionsOn(m *Manifest) []Finding {
	var findings []Finding
	for i, a := range m.Actions {
		findings = append(findings, onMatcherFindings(a.On, actionRef(i, a.ID)+".on")...)
	}
	return findings
}

func checkActionsPopup(m *Manifest) []Finding {
	var findings []Finding
	for i, a := range m.Actions {
		if a.Popup == nil {
			continue
		}
		findings = append(findings, popupBoundsFindings(a.Popup, actionRef(i, a.ID)+".popup")...)
	}
	return findings
}

// checkListenerRequiresOn rejects listener actions that declare no events to
// subscribe to. A listener with `on: []` has no runtime purpose — it is
// hidden from every user-facing surface, so it can never be invoked either.
func checkListenerRequiresOn(m *Manifest) []Finding {
	var findings []Finding
	for i, a := range m.Actions {
		if !a.Listener || len(a.On) > 0 {
			continue
		}
		findings = append(findings, Finding{
			Rule:     RuleListenerRequiresOn,
			Severity: SeverityError,
			Message:  "listener actions must declare at least one on: matcher (hidden from every user-facing surface, so an empty on has no runtime purpose)",
			Field:    actionRef(i, a.ID) + ".listener",
		})
	}
	return findings
}

// checkRegistry consults the registry for rules #9 (name ownership) and #10
// (monotonic version). Callers must gate this on a valid name (see Check);
// this function assumes m.Name has already passed rule #5 grammar, so it
// avoids a wasted network round-trip on names that could never be accepted.
func checkRegistry(m *Manifest, reg RegistryLookup, ownerRepo string) []Finding {
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
