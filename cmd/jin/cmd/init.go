package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// configTemplate is the default config.yaml written by `jin init`
const configTemplate = `# jin configuration
# See https://github.com/takaaki-s/jindaiko for details

# Customize keybindings (defaults are used when omitted)
keybindings:
  # Session list view
  up: ["up", "k"]
  down: ["down", "j"]
  attach: ["enter"]
  new: ["n"]
  kill: ["x"]
  delete: ["d"]
  refresh: ["r"]
  search: ["/"]
  vscode: ["v"]
  notifications: ["!"]
  quit: ["q", "ctrl+c"]
  help: ["?"]
  # Session creation form
  next_field: ["tab"]
  prev_field: ["shift+tab"]
  submit: ["enter"]
  cancel_form: ["esc"]
  # While attached
  # Supported keys: ctrl+^, ctrl+], ctrl+\, ctrl+g
  detach: ["ctrl+]"]
`

var forceInit bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize jin configuration",
	Long: `Create the jin configuration directory and a default config.yaml.

If config.yaml already exists, this command does nothing unless --force is specified.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir := getConfigDir()
		configFile := filepath.Join(configDir, "config.yaml")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			return fmt.Errorf("failed to create config directory: %w", err)
		}

		if !forceInit {
			if _, err := os.Stat(configFile); err == nil {
				fmt.Printf("Config already exists: %s\n", configFile)
				fmt.Println("Use --force to overwrite.")
				return nil
			}
		}

		if err := os.WriteFile(configFile, []byte(configTemplate), 0644); err != nil {
			return fmt.Errorf("failed to write config: %w", err)
		}

		fmt.Printf("Created: %s\n", configFile)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().BoolVar(&forceInit, "force", false, "Overwrite existing config.yaml")
}
