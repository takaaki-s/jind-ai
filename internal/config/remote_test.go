package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteConfigLoadsAndResolvesExplicitMapping(t *testing.T) {
	dir := t.TempDir()
	config := `remote:
  targets:
    build:
      ssh_host: build-host
      repositories:
        jind-ai: product
  serve:
    enabled: true
    repositories:
      product: /srv/jind-ai
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	target, repository, revision, err := manager.ResolveRemoteTarget("build", "jind-ai")
	if err != nil {
		t.Fatalf("ResolveRemoteTarget: %v", err)
	}
	if target.SSHHost != "build-host" || target.JinPath != "jin" || repository != "product" || revision == "" {
		t.Fatalf("resolved target = %+v, %q, %q", target, repository, revision)
	}
	serve := manager.GetRemoteServeConfig()
	if !serve.Enabled || serve.Repositories["product"] != "/srv/jind-ai" {
		t.Fatalf("serve config = %+v", serve)
	}
}

func TestRemoteTargetRevisionIsDeterministicAndTracksConnectionConfig(t *testing.T) {
	left := RemoteTargetConfig{
		SSHHost: "build-host", JinPath: "jin",
		Repositories: map[string]string{"beta": "remote-b", "alpha": "remote-a"},
	}
	right := RemoteTargetConfig{
		SSHHost: "build-host", JinPath: "jin",
		Repositories: map[string]string{"alpha": "remote-a", "beta": "remote-b"},
	}
	if remoteTargetRevision("build", left) != remoteTargetRevision("build", right) {
		t.Fatal("target revision depends on map iteration order")
	}
	right.SSHHost = "other-host"
	if remoteTargetRevision("build", left) == remoteTargetRevision("build", right) {
		t.Fatal("target revision ignored SSH host change")
	}
}

func TestRemoteConfigSnapshotsCannotMutateManager(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.config.Remote = RemoteConfig{
		Targets: map[string]RemoteTargetConfig{
			"build": {SSHHost: "build-host", Repositories: map[string]string{"local": "remote"}},
		},
		Serve: RemoteServeConfig{Enabled: true, Repositories: map[string]string{"remote": "/srv/repo"}},
	}
	manager.mu.Unlock()

	target, _, _, err := manager.ResolveRemoteTarget("build", "local")
	if err != nil {
		t.Fatal(err)
	}
	target.Repositories["local"] = "changed"
	serve := manager.GetRemoteServeConfig()
	serve.Repositories["remote"] = "/changed"
	all := manager.Get()
	all.Remote.Targets["build"] = RemoteTargetConfig{}
	all.Remote.Serve.Repositories["remote"] = "/also-changed"

	target, repository, _, err := manager.ResolveRemoteTarget("build", "local")
	if err != nil {
		t.Fatal(err)
	}
	if repository != "remote" || target.SSHHost != "build-host" || manager.GetRemoteServeConfig().Repositories["remote"] != "/srv/repo" {
		t.Fatal("remote config was mutated through a returned snapshot")
	}
}

func TestResolveRemoteTargetRejectsAmbiguousLabels(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.config.Remote.Targets = map[string]RemoteTargetConfig{
		"build": {SSHHost: "build-host", Repositories: map[string]string{"repo": "remote"}},
	}
	manager.mu.Unlock()
	for _, test := range []struct{ target, repository string }{
		{target: "Build", repository: "repo"},
		{target: "build", repository: "Repo"},
		{target: "build;touch", repository: "repo"},
	} {
		if _, _, _, err := manager.ResolveRemoteTarget(test.target, test.repository); err == nil {
			t.Fatalf("ResolveRemoteTarget(%q, %q) succeeded", test.target, test.repository)
		}
	}
}
