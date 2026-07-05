# Adding CLI Commands

## Steps

### 1. Create the Command File

Create a new `.go` file in `cmd/jin/cmd/`.

### 2. Define the Cobra Command

```go
package cmd

import "github.com/spf13/cobra"

var myCmd = &cobra.Command{
    Use:   "my-command",
    Short: "Short description",
    RunE: func(cmd *cobra.Command, args []string) error {
        // Implementation
        return nil
    },
}

func init() {
    // For a top-level command:
    rootCmd.AddCommand(myCmd)

    // For a session subcommand:
    // sessionCmd.AddCommand(myCmd)
}
```

### 3. If Daemon Communication Is Needed

```go
import "github.com/takaaki-s/honjin/internal/daemon"

client, err := daemon.NewClient()
if err != nil {
    return fmt.Errorf("daemon not running: %w", err)
}
defer client.Close()

// IPC call
resp, err := client.Send(daemon.Request{
    Action: "my-action",
    Data:   data,
})
```

### 4. Command Hierarchy

```
jin (root)
├─ daemon start/stop/status
├─ session
│   ├─ new
│   ├─ list
│   ├─ kill
│   ├─ delete
│   ├─ attach
│   ├─ edit
│   └─ workdir
├─ worktree
│   ├─ allow
│   ├─ revoke
│   ├─ status
│   └─ list
├─ tui (alias: ui)
├─ hook
├─ cleanup stopped
├─ create-popup   (Hidden, for TUI popup)
├─ help-popup     (Hidden, for TUI popup)
├─ notify-popup   (Hidden, for TUI popup)
└─ (add new commands here)
```

## Reference Files

- Simple commands: `kill.go`, `list.go`
- Commands with flags: `new.go`
- Subcommand groups: `session.go`
