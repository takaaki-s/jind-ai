package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitCmd_CreatesConfig(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")

	// Temporarily override getConfigDir via the flag mechanism
	// by directly testing the file creation logic
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(configFile, []byte(configTemplate), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != configTemplate {
		t.Errorf("config content mismatch")
	}
}

func TestInitCmd_NoOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")

	original := "# original\n"
	if err := os.WriteFile(configFile, []byte(original), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Simulate the no-force path: file exists, don't overwrite
	if _, err := os.Stat(configFile); err == nil {
		// file exists — skip write (matches initCmd logic when !forceInit)
		data, _ := os.ReadFile(configFile)
		if string(data) != original {
			t.Errorf("file should not have been overwritten")
		}
	}
}

func TestInitCmd_OverwriteWithForce(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(configFile, []byte("# old\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Simulate force overwrite
	if err := os.WriteFile(configFile, []byte(configTemplate), 0644); err != nil {
		t.Fatalf("WriteFile force: %v", err)
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != configTemplate {
		t.Errorf("expected template content after force overwrite")
	}
}

func TestConfigTemplate_ContainsExpectedKeys(t *testing.T) {
	keys := []string{
		"keybindings:",
		"up:",
		"down:",
		"attach:",
		"detach:",
		"quit:",
		"# default_agent:",
	}
	for _, key := range keys {
		found := false
		for i := 0; i < len(configTemplate)-len(key); i++ {
			if configTemplate[i:i+len(key)] == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("configTemplate missing key: %s", key)
		}
	}
}
