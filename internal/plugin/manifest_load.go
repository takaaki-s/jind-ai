// Package plugin owns the runtime side of jind-ai plugins — installation,
// registry classification, event dispatch, and per-run execution. The
// manifest itself lives in pkg/plugin/manifest (the single source of truth
// shared with the registry crawler); this file exposes a load helper that
// wraps parse + Validate so callers get an error-shaped API.
package plugin

import (
	"fmt"

	"github.com/takaaki-s/jind-ai/internal/version"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// loadManifest reads and validates the manifest at pluginDir. Unknown fields
// are dropped here on purpose — they are advisory WARNs for the validate
// command, not blockers for the runtime. Every ERROR-severity rule turns
// into a wrapped error so callers can bubble it up unchanged.
func loadManifest(pluginDir string) (*manifest.Manifest, error) {
	m, _, err := manifest.LoadFile(pluginDir)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if err := manifest.Validate(m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// checkJinCompat reports whether the running jin binary satisfies m's jin
// compat range. Development builds (Version == "dev" or unset) are treated
// as satisfying every range so local plugin development is unblocked.
func checkJinCompat(m *manifest.Manifest) error {
	return manifest.CheckJinCompat(m.Jin, version.Version)
}
