package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustParse(t *testing.T, path string) (*Manifest, []string) {
	t.Helper()
	data := mustRead(t, path)
	m, unknown, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse %s: %v", path, err)
	}
	return m, unknown
}

func hasFinding(findings []Finding, rule RuleID, sev Severity) bool {
	return findFinding(findings, rule, sev) != nil
}

func findingsSummary(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		b.WriteString(f.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func TestCheckValidMinimalPasses(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	findings := Check(m, CheckOptions{})
	if HasErrors(findings) {
		t.Errorf("expected no ERROR findings, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckMergeHandoffActionIsolation(t *testing.T) {
	m := &Manifest{Actions: []Action{{
		ID: "merge", Entrypoint: "./merge", MergeHandoff: true, Handoff: true,
		Listener: true, On: []string{"status_changed"}, Popup: &PopupConfig{Width: 50},
	}}}
	findings := checkHandoffActions(m)
	if len(findings) != 4 {
		t.Fatalf("findings = %v, want capability/listener/on/popup errors", findings)
	}
}

func TestCheckRuntimeFullPasses(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_runtime_full.yaml")
	findings := Check(m, CheckOptions{})
	if HasErrors(findings) {
		t.Errorf("expected no ERROR findings, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckScriptPluginWithoutBuildPasses(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_script.yaml")
	findings := Check(m, CheckOptions{})
	if HasErrors(findings) {
		t.Errorf("expected no ERROR findings for a build-less script plugin, got:\n%s", findingsSummary(findings))
	}
	if len(m.BuildCommands()) != 0 {
		t.Errorf("expected zero build commands, got %v", m.BuildCommands())
	}
}

func TestCheckBadNameEmitsRule5(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_bad_name.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleNamePattern, SeverityError) {
		t.Errorf("expected RuleNamePattern ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckMissingFieldsEmitsRule4(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_missing_field.yaml")
	findings := Check(m, CheckOptions{})
	seen := map[string]bool{}
	for _, f := range findings {
		if f.Rule == RuleRequiredFields && f.Severity == SeverityError {
			seen[f.Field] = true
		}
	}
	if !seen["version"] || !seen["jin"] {
		t.Errorf("expected required-field ERRORs for version and jin, got: %v", seen)
	}
}

func TestCheckBothInstallEmitsRule8(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_both_install.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleInstallXOR, SeverityError) {
		t.Errorf("expected RuleInstallXOR ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckBadOnMatcherEmitsRule15(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_bad_on.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleOnMatcher, SeverityError) {
		t.Errorf("expected RuleOnMatcher ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckPopupBoundsEmitsRule16(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_popup.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RulePopupBounds, SeverityError) {
		t.Errorf("expected RulePopupBounds ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckReleaseAssetPlaceholders(t *testing.T) {
	yamlDoc := []byte(`schema_version: 1
name: hello
version: 0.1.0
description: bad placeholder
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{unknown}"
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	findings := Check(m, CheckOptions{})
	warned := false
	for _, f := range findings {
		if f.Field == "install.release_asset.pattern" && f.Severity == SeverityWarning {
			warned = true
			break
		}
	}
	if !warned {
		t.Errorf("expected WARN about unknown placeholder, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckBadSchemaVersion(t *testing.T) {
	yamlDoc := []byte(`schema_version: 99
name: hello
version: 0.1.0
description: too new
jin: ">=0.7.0"
install:
  source:
    build: [go build]
    entrypoint: ./bin/hello
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleSchemaVersion, SeverityError) {
		t.Errorf("expected RuleSchemaVersion ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckBadSemver(t *testing.T) {
	yamlDoc := []byte(`schema_version: 1
name: hello
version: not-semver
description: bad version
jin: ">=0.7.0"
install:
  source:
    build: [go build]
    entrypoint: ./bin/hello
`)
	m, _, _ := Parse(yamlDoc)
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleVersionSemver, SeverityError) {
		t.Errorf("expected RuleVersionSemver ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckBadJinConstraint(t *testing.T) {
	yamlDoc := []byte(`schema_version: 1
name: hello
version: 0.1.0
description: bad jin
jin: "not a range"
install:
  source:
    build: [go build]
    entrypoint: ./bin/hello
`)
	m, _, _ := Parse(yamlDoc)
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleJinRangeParses, SeverityError) {
		t.Errorf("expected RuleJinRangeParses ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckUnknownFieldEmitsWarning(t *testing.T) {
	m, unknown := mustParse(t, "testdata/manifests/valid_unknown_field.yaml")
	findings := Check(m, CheckOptions{UnknownFields: unknown})
	if !hasFinding(findings, RuleUnknownFieldWarning, SeverityWarning) {
		t.Errorf("expected unknown-field WARN, got:\n%s", findingsSummary(findings))
	}
	if HasErrors(findings) {
		t.Errorf("unknown fields should not be ERRORs, got:\n%s", findingsSummary(findings))
	}
}

func TestValidateReturnsFirstError(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_bad_name.yaml")
	if err := Validate(m); err == nil {
		t.Fatal("Validate: want error for bad name, got nil")
	}
	good, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	if err := Validate(good); err != nil {
		t.Errorf("Validate on valid manifest: %v", err)
	}
}

type fakeRegistry struct {
	owner   string
	latest  string
	lookErr error
}

func (f fakeRegistry) Lookup(_ string) (string, string, error) {
	return f.owner, f.latest, f.lookErr
}

func TestCheckRegistryNameConflict(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	reg := fakeRegistry{owner: "someone-else/hello-plugin"}
	findings := Check(m, CheckOptions{
		Registry:  reg,
		OwnerRepo: "me/hello-plugin",
	})
	if !hasFinding(findings, RuleNameOwnership, SeverityError) {
		t.Errorf("expected RuleNameOwnership ERROR for cross-owner conflict, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckRegistrySameOwnerOK(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	reg := fakeRegistry{owner: "me/hello-plugin"}
	findings := Check(m, CheckOptions{
		Registry:  reg,
		OwnerRepo: "me/hello-plugin",
	})
	if hasFinding(findings, RuleNameOwnership, SeverityError) {
		t.Errorf("same-owner should not trigger ownership ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckRegistryVersionNotMonotonic(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml") // 0.1.0
	reg := fakeRegistry{owner: "me/hello-plugin", latest: "0.2.0"}
	findings := Check(m, CheckOptions{
		Registry:  reg,
		OwnerRepo: "me/hello-plugin",
	})
	if !hasFinding(findings, RuleVersionMonotonic, SeverityWarning) {
		t.Errorf("expected RuleVersionMonotonic WARN, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckRegistryLookupError(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	reg := fakeRegistry{lookErr: errors.New("boom")}
	findings := Check(m, CheckOptions{Registry: reg})
	if !hasFinding(findings, RuleNameOwnership, SeverityWarning) {
		t.Errorf("registry error should surface as WARN, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckLicenseAndReadmeMissing(t *testing.T) {
	dir := t.TempDir()
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	findings := Check(m, CheckOptions{PluginDir: dir})
	if !hasFinding(findings, RuleLicenseFile, SeverityWarning) {
		t.Errorf("expected LICENSE WARN, got:\n%s", findingsSummary(findings))
	}
	if !hasFinding(findings, RuleReadmeMinimal, SeverityWarning) {
		t.Errorf("expected README WARN, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckLicenseAndReadmePresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("MIT"), 0o644); err != nil {
		t.Fatalf("write LICENSE: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello\n\n## Install\n\nblah"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	findings := Check(m, CheckOptions{PluginDir: dir})
	if hasFinding(findings, RuleLicenseFile, SeverityWarning) {
		t.Errorf("LICENSE present but WARN emitted:\n%s", findingsSummary(findings))
	}
	if hasFinding(findings, RuleReadmeMinimal, SeverityWarning) {
		t.Errorf("README present but WARN emitted:\n%s", findingsSummary(findings))
	}
}

func TestCheckReadmeEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("   \n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	findings := Check(m, CheckOptions{PluginDir: dir})
	if !hasFinding(findings, RuleReadmeMinimal, SeverityWarning) {
		t.Errorf("empty README should warn, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckReadmeMissingHeading(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Overview\n\nno install heading"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	findings := Check(m, CheckOptions{PluginDir: dir})
	if !hasFinding(findings, RuleReadmeMinimal, SeverityWarning) {
		t.Errorf("missing Install/Usage heading should warn, got:\n%s", findingsSummary(findings))
	}
}

func TestHasErrorsWarnings(t *testing.T) {
	findings := []Finding{
		{Rule: RuleNamePattern, Severity: SeverityError},
		{Rule: RuleLicenseFile, Severity: SeverityWarning},
	}
	if !HasErrors(findings) {
		t.Errorf("HasErrors should be true")
	}
	if !HasWarnings(findings) {
		t.Errorf("HasWarnings should be true")
	}
	if HasErrors(nil) || HasWarnings(nil) {
		t.Errorf("nil findings should not have errors or warnings")
	}
}

func TestCheckV2MultiActionPasses(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_v2_multi_action.yaml")
	findings := Check(m, CheckOptions{})
	if HasErrors(findings) {
		t.Errorf("expected no ERROR findings, got:\n%s", findingsSummary(findings))
	}
}

// TestCheckV2SourceInstallPasses guards the "one build, many actions" pattern:
// a v2 manifest with install.source (no top-level entrypoint) plus multiple
// per-action entrypoints must validate. Before the fix, checkSourceInstall
// unconditionally required a top-level entrypoint and blocked this case.
func TestCheckV2SourceInstallPasses(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_v2_source_install.yaml")
	findings := Check(m, CheckOptions{})
	if HasErrors(findings) {
		t.Errorf("expected no ERROR findings, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckV2TopLevelEntrypointRejected(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_v2_top_level_entrypoint.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleV2Constraint, SeverityError) {
		t.Errorf("expected RuleV2Constraint ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckV2TopLevelOnRejected(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_v2_top_level_on.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleV2Constraint, SeverityError) {
		t.Errorf("expected RuleV2Constraint ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckV2NoActionsRejected(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_v2_no_actions.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleActionsRequired, SeverityError) {
		t.Errorf("expected RuleActionsRequired ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckV2EmptyActionsArrayRejected(t *testing.T) {
	yamlDoc := []byte(`schema_version: 2
name: hello
version: 0.1.0
description: v2 with an explicit empty actions array
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions: []
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleActionsRequired, SeverityError) {
		t.Errorf("expected RuleActionsRequired ERROR for explicit empty actions, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckActionBadIDRejected(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_v2_bad_action_id.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleActionID, SeverityError) {
		t.Errorf("expected RuleActionID ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckActionDuplicateIDRejected(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_v2_duplicate_action_id.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleActionDuplicateID, SeverityError) {
		t.Errorf("expected RuleActionDuplicateID ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckActionMissingEntrypointRejected(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_v2_action_no_entrypoint.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleActionEntrypoint, SeverityError) {
		t.Errorf("expected RuleActionEntrypoint ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckActionOnBadMatcherRejected(t *testing.T) {
	yamlDoc := []byte(`schema_version: 2
name: hello
version: 0.1.0
description: action declares an invalid on matcher
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions:
  - id: default
    entrypoint: ./bin/hello
    on:
      - file_changed
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleOnMatcher, SeverityError) {
		t.Errorf("expected RuleOnMatcher ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckActionPopupOutOfRangeRejected(t *testing.T) {
	yamlDoc := []byte(`schema_version: 2
name: hello
version: 0.1.0
description: action popup width is out of range
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions:
  - id: default
    entrypoint: ./bin/hello
    popup:
      width: 101
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RulePopupBounds, SeverityError) {
		t.Errorf("expected RulePopupBounds ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckListenerWithOnAccepted(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_v2_listener_action.yaml")
	findings := Check(m, CheckOptions{})
	if hasFinding(findings, RuleListenerRequiresOn, SeverityError) {
		t.Errorf("listener with on: should pass, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckListenerWithoutOnRejected(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/invalid_v2_listener_without_on.yaml")
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleListenerRequiresOn, SeverityError) {
		t.Errorf("expected RuleListenerRequiresOn ERROR, got:\n%s", findingsSummary(findings))
	}
}

func TestCheckV1MinimalStillPassesUnderV2Build(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	if m.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1 (fixture assumption)", m.SchemaVersion)
	}
	findings := Check(m, CheckOptions{})
	if HasErrors(findings) {
		t.Errorf("schema_version 1 should remain accepted under CurrentSchemaVersion=%d, got:\n%s",
			CurrentSchemaVersion, findingsSummary(findings))
	}
}

func TestCheckActionIDLengthBoundary(t *testing.T) {
	cases := []struct {
		name    string
		idLen   int
		wantErr bool
	}{
		{name: "32 chars ok", idLen: 32, wantErr: false},
		{name: "33 chars rejected", idLen: 33, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := strings.Repeat("a", tc.idLen)
			yamlDoc := []byte(fmt.Sprintf(`schema_version: 2
name: hello
version: 0.1.0
description: action id length boundary
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions:
  - id: %s
    entrypoint: ./bin/hello
`, id))
			m, _, err := Parse(yamlDoc)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			findings := Check(m, CheckOptions{})
			if got := hasFinding(findings, RuleActionID, SeverityError); got != tc.wantErr {
				t.Errorf("RuleActionID ERROR = %v, want %v (id len %d), findings:\n%s",
					got, tc.wantErr, tc.idLen, findingsSummary(findings))
			}
		})
	}
}

func TestCheckActionPopupWidthBoundary(t *testing.T) {
	cases := []struct {
		name    string
		width   int
		wantErr bool
	}{
		{name: "width 100 ok", width: 100, wantErr: false},
		{name: "width 101 rejected", width: 101, wantErr: true},
		{name: "width 0 unset skips validation", width: 0, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var popupField string
			if tc.width != 0 {
				popupField = fmt.Sprintf("    popup:\n      width: %d\n", tc.width)
			}
			yamlDoc := []byte(fmt.Sprintf(`schema_version: 2
name: hello
version: 0.1.0
description: action popup width boundary
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions:
  - id: default
    entrypoint: ./bin/hello
%s`, popupField))
			m, _, err := Parse(yamlDoc)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			findings := Check(m, CheckOptions{})
			if got := hasFinding(findings, RulePopupBounds, SeverityError); got != tc.wantErr {
				t.Errorf("RulePopupBounds ERROR = %v, want %v, findings:\n%s", got, tc.wantErr, findingsSummary(findings))
			}
		})
	}
}

func TestCheckJinCompat(t *testing.T) {
	cases := []struct {
		name       string
		constraint string
		jinVersion string
		wantErr    bool
	}{
		{name: "in range", constraint: ">=0.7.0", jinVersion: "0.7.1", wantErr: false},
		{name: "below range", constraint: ">=0.7.0", jinVersion: "0.6.9", wantErr: true},
		{name: "dev skips check", constraint: ">=0.7.0", jinVersion: "dev", wantErr: false},
		{name: "empty skips check", constraint: ">=0.7.0", jinVersion: "", wantErr: false},
		{name: "unparsable version skips check", constraint: ">=0.7.0", jinVersion: "abc", wantErr: false},
		{name: "bad constraint", constraint: "not a range", jinVersion: "0.7.0", wantErr: true},
		{name: "empty constraint", constraint: "", jinVersion: "0.7.0", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckJinCompat(tc.constraint, tc.jinVersion)
			if (err != nil) != tc.wantErr {
				t.Errorf("CheckJinCompat(%q, %q) err=%v, wantErr=%v",
					tc.constraint, tc.jinVersion, err, tc.wantErr)
			}
		})
	}
}

// TestCheckV1OnMatcherReportedTwice pins the current behaviour where an
// invalid v1 top-level `on:` matcher is reported by both checkOn (top) and
// checkActionsOn (via the normalize-synthesized Actions[0].On). The
// duplication is accepted for now rather than deduped; this test freezes
// the count so a future dedup can be detected as a deliberate change rather
// than a silent regression. Adjust `want` when the dedup lands.
//
// Inlined YAML (rather than a shared fixture) so the count assertion stays
// coupled to this test's assumptions: exactly one invalid matcher at top
// level, no per-action `on:`.
func TestCheckV1OnMatcherReportedTwice(t *testing.T) {
	yamlDoc := []byte(`schema_version: 1
name: hello
version: 0.1.0
description: v1 with one invalid top-level on matcher
jin: ">=0.7.0"
install:
  source:
    entrypoint: ./bin/hello
on:
  - "not a valid matcher"
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	findings := Check(m, CheckOptions{})
	count := 0
	for _, f := range findings {
		if f.Rule == RuleOnMatcher && f.Severity == SeverityError {
			count++
		}
	}
	if want := 2; count != want {
		t.Errorf("RuleOnMatcher ERROR count = %d, want %d (top-level + normalize-synthesized Actions[0]); if a dedup landed, update this test", count, want)
	}
}

func TestCheckHandoffActionMustBeExplicitOnly(t *testing.T) {
	m := &Manifest{SchemaVersion: 2, Name: "provider", Version: "0.1.0", Jin: ">=0.11.0",
		Install: Install{Source: &SourceInstall{}}, Actions: []Action{{
			ID: "create", Entrypoint: "./create", Handoff: true, Listener: true,
			On: []string{"status_changed"}, Popup: &PopupConfig{Width: 50},
		}}}
	findings := Check(m, CheckOptions{})
	count := 0
	for _, finding := range findings {
		if finding.Rule == RuleHandoffActionOnly && finding.Severity == SeverityError {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("handoff constraint findings = %d, want 3:\n%s", count, findingsSummary(findings))
	}
}

// TestCheckV2ActionWithEmptyIDAndEntrypoint drives the fallback branches in
// checkActionEntrypoints / checkActionsOn / checkActionsPopup that only fire
// when an action's ID is empty. Without this test the branches are dead
// coverage — the id-populated fixtures never exercise them.
func TestCheckV2ActionWithEmptyIDAndEntrypoint(t *testing.T) {
	yamlDoc := []byte(`schema_version: 2
name: hello
version: 0.1.0
description: v2 with an empty-id, empty-entrypoint action
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions:
  - id: ""
    entrypoint: ""
    on:
      - "not a valid matcher"
    popup:
      width: 200
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	findings := Check(m, CheckOptions{})
	if !hasFinding(findings, RuleActionID, SeverityError) {
		t.Errorf("expected RuleActionID ERROR (empty id fails pattern), got:\n%s", findingsSummary(findings))
	}
	entrypointFinding := findFinding(findings, RuleActionEntrypoint, SeverityError)
	if entrypointFinding == nil {
		t.Fatalf("expected RuleActionEntrypoint ERROR, got:\n%s", findingsSummary(findings))
	}
	if want := "actions[0].entrypoint"; entrypointFinding.Field != want {
		t.Errorf("entrypoint Field = %q, want %q (index fallback when id is empty)", entrypointFinding.Field, want)
	}
	if !strings.Contains(entrypointFinding.Message, "actions[0]") {
		t.Errorf("entrypoint Message = %q, want it to mention actions[0] for the empty-id case", entrypointFinding.Message)
	}
	onFinding := findFinding(findings, RuleOnMatcher, SeverityError)
	if onFinding == nil || onFinding.Field != "actions[0].on" {
		t.Errorf("expected RuleOnMatcher ERROR with Field=actions[0].on, got: %v", onFinding)
	}
	popupFinding := findFinding(findings, RulePopupBounds, SeverityError)
	if popupFinding == nil || popupFinding.Field != "actions[0].popup.width" {
		t.Errorf("expected RulePopupBounds ERROR with Field=actions[0].popup.width, got: %v", popupFinding)
	}
}

func findFinding(findings []Finding, rule RuleID, sev Severity) *Finding {
	for i := range findings {
		if findings[i].Rule == rule && findings[i].Severity == sev {
			return &findings[i]
		}
	}
	return nil
}
