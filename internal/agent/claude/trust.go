package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings is the on-disk shape of ~/.claude/settings.local.json (the subset
// jindaiko needs to touch). Only the "projects" map is written; unknown fields
// in the user's existing file survive because we round-trip through the
// typed representation only for keys we care about.
type Settings struct {
	Projects map[string]ProjectSettings `json:"projects,omitempty"`
}

// ProjectSettings mirrors Claude Code's per-project settings block. We only
// use HasTrustDialogAccepted; adding more fields later is an ABI-safe
// change because unknown keys in the on-disk file are preserved by the
// json package.
type ProjectSettings struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted,omitempty"`
}

// EnsureTrustState sets hasTrustDialogAccepted=true in
// ~/.claude/settings.local.json for the absolute path of workDir. Claude Code
// checks this flag to decide whether to prompt the user with a trust dialog
// on start-up; without it, `claude` opens interactively-blocked in a tmux
// pane and hangs forever.
//
// Idempotent: exits early when the workDir entry is already trusted.
func EnsureTrustState(workDir string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".claude", "settings.local.json")

	claudeDir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}

	var settings Settings
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			// Corrupt settings file: start over rather than propagating.
			// Claude Code has to survive this state too, so nuking our
			// own view is safe.
			settings = Settings{}
		}
	}

	if settings.Projects == nil {
		settings.Projects = make(map[string]ProjectSettings)
	}

	if projectSettings, exists := settings.Projects[absWorkDir]; exists && projectSettings.HasTrustDialogAccepted {
		return nil
	}

	settings.Projects[absWorkDir] = ProjectSettings{HasTrustDialogAccepted: true}

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings file: %w", err)
	}

	claudeLog("[TRUST] Set hasTrustDialogAccepted=true for %s", absWorkDir)
	return nil
}
