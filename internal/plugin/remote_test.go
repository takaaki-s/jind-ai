package plugin

import (
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// makeDoc builds a two-entry registry document good enough for ResolveRemote
// tests: one plugin with two versions (0.1.0 and 0.2.0, latest=0.2.0), one
// with no versions to exercise the "no latest_version" branch.
func makeDoc() *manifest.RegistryDocument {
	return &manifest.RegistryDocument{
		SchemaVersion: manifest.CurrentSchemaVersion,
		GeneratedAt:   time.Now(),
		Plugins: []manifest.RegistryEntry{
			{
				Name:          "jind-ai-notifier",
				Repo:          "foo/jind-ai-notifier",
				JinCompat:     ">=0.7.0",
				LatestVersion: "0.2.0",
				Versions: []manifest.RegistryVersion{
					{Version: "0.1.0", SHA: "aaaaaaaaaaaa"},
					{Version: "0.2.0", SHA: "bbbbbbbbbbbb"},
				},
			},
			{
				Name:          "empty-plugin",
				Repo:          "foo/empty-plugin",
				JinCompat:     ">=0.7.0",
				LatestVersion: "",
			},
		},
	}
}

func TestResolveRemote_LatestPicksLatestVersion(t *testing.T) {
	doc := makeDoc()
	r, err := ResolveRemote("jind-ai-notifier", "", doc)
	if err != nil {
		t.Fatalf("ResolveRemote: %v", err)
	}
	if r.Version.Version != "0.2.0" {
		t.Errorf("Version.Version = %q, want 0.2.0", r.Version.Version)
	}
	if r.Version.SHA != "bbbbbbbbbbbb" {
		t.Errorf("Version.SHA = %q, want bbbbbbbbbbbb", r.Version.SHA)
	}
	src := r.Source()
	if src.CloneURL != "https://github.com/foo/jind-ai-notifier" {
		t.Errorf("CloneURL = %q", src.CloneURL)
	}
	if src.Ref != "bbbbbbbbbbbb" {
		t.Errorf("Ref = %q, want bbbbbbbbbbbb", src.Ref)
	}
}

func TestResolveRemote_PinSelectsVersion(t *testing.T) {
	doc := makeDoc()
	r, err := ResolveRemote("jind-ai-notifier", "0.1.0", doc)
	if err != nil {
		t.Fatalf("ResolveRemote: %v", err)
	}
	if r.Version.Version != "0.1.0" {
		t.Errorf("Version.Version = %q, want 0.1.0", r.Version.Version)
	}
	if r.Source().Ref != "aaaaaaaaaaaa" {
		t.Errorf("Ref = %q, want aaaaaaaaaaaa", r.Source().Ref)
	}
}

func TestResolveRemote_UnknownName(t *testing.T) {
	doc := makeDoc()
	if _, err := ResolveRemote("no-such-plugin", "", doc); err == nil {
		t.Fatal("ResolveRemote: expected error for unknown name, got nil")
	} else if !strings.Contains(err.Error(), "not in the registry") {
		t.Errorf("error = %v, want it to mention 'not in the registry'", err)
	}
}

func TestResolveRemote_UnknownVersion(t *testing.T) {
	doc := makeDoc()
	_, err := ResolveRemote("jind-ai-notifier", "9.9.9", doc)
	if err == nil {
		t.Fatal("ResolveRemote: expected error for unknown version, got nil")
	}
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("error = %v, want it to name the missing version", err)
	}
	if !strings.Contains(err.Error(), "available:") {
		t.Errorf("error = %v, want it to list available versions", err)
	}
}

func TestResolveRemote_NoLatestVersion(t *testing.T) {
	doc := makeDoc()
	_, err := ResolveRemote("empty-plugin", "", doc)
	if err == nil {
		t.Fatal("ResolveRemote: expected error when latest_version is empty, got nil")
	}
	if !strings.Contains(err.Error(), "no latest_version") {
		t.Errorf("error = %v, want it to mention 'no latest_version'", err)
	}
}

func TestResolveRemote_NilDoc(t *testing.T) {
	if _, err := ResolveRemote("anything", "", nil); err == nil {
		t.Fatal("ResolveRemote(nil): expected error, got nil")
	}
}
