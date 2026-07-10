package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/paths"
)

type daemonStatusResult struct {
	Running bool `json:"running"`
}

var socketPathFlag string

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Start the daemon server",
	Long:  `Start the jin daemon server. This must be running for session management.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check if daemon is already running
		client := daemon.NewClient(getSocketPath())
		if client.IsRunning() {
			return fmt.Errorf("daemon is already running. Use 'jin daemon stop' to stop it first")
		}

		server, err := daemon.NewServer(getSocketPath(), getDataDir(), getConfigDir(), getStateDir())
		if err != nil {
			return err
		}

		return server.Start()
	},
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon in the background",
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())
		if client.IsRunning() {
			fmt.Println("Daemon is already running")
			return nil
		}

		// Start daemon in background
		exe, _ := os.Executable()
		daemonArgs := []string{"daemon"}
		if socketPathFlag != "" {
			daemonArgs = append(daemonArgs, "--socket", socketPathFlag)
		}
		bgCmd := exec.Command(exe, daemonArgs...)
		bgCmd.Env = os.Environ() // Inherit environment variables
		bgCmd.Stdout = nil
		bgCmd.Stderr = nil
		bgCmd.Stdin = nil

		if err := bgCmd.Start(); err != nil {
			return err
		}

		fmt.Printf("Daemon started (PID: %d)\n", bgCmd.Process.Pid)
		return nil
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())
		running := client.IsRunning()
		if jsonOutput {
			return renderDaemonStatusJSON(os.Stdout, daemonStatusResult{Running: running})
		}
		if running {
			fmt.Println("Daemon is running")
		} else {
			fmt.Println("Daemon is not running")
		}
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())
		if !client.IsRunning() {
			fmt.Println("Daemon is not running")
			return nil
		}

		if err := client.Stop(); err != nil {
			return err
		}
		// Wait for daemon to actually exit (handleStop calls os.Exit in a goroutine)
		for range 30 {
			if !client.IsRunning() {
				fmt.Println("Daemon stopped")
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Println("Daemon stopped")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(daemonCmd)
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonStopCmd)

	daemonCmd.PersistentFlags().StringVar(&socketPathFlag, "socket", "", "custom socket path")
}

func renderDaemonStatusJSON(w io.Writer, result daemonStatusResult) error {
	return writeJSON(w, result)
}

func getSocketPath() string {
	if socketPathFlag != "" {
		return socketPathFlag
	}
	if s := os.Getenv("JIN_SOCKET"); s != "" {
		return s
	}
	return paths.Socket()
}
