package manifest

import (
	"errors"
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
	for _, f := range findings {
		if f.Rule == rule && f.Severity == sev {
			return true
		}
	}
	return false
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

func TestCheckRuntimeFullPasses(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_runtime_full.yaml")
	findings := Check(m, CheckOptions{})
	if HasErrors(findings) {
		t.Errorf("expected no ERROR findings, got:\n%s", findingsSummary(findings))
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

func TestCheckRunBuildStub(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	findings := Check(m, CheckOptions{RunBuild: true})
	if !hasFinding(findings, RuleBuildRuns, SeverityError) {
		t.Errorf("run-build without PluginDir should ERROR, got:\n%s", findingsSummary(findings))
	}

	dir := t.TempDir()
	findings = Check(m, CheckOptions{PluginDir: dir, RunBuild: true})
	if !hasFinding(findings, RuleBuildRuns, SeverityWarning) {
		t.Errorf("run-build stub should emit WARN, got:\n%s", findingsSummary(findings))
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
