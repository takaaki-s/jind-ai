package debug

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/takaaki-s/jind-ai/internal/paths"
)

// enabled is true when JIN_DEBUG=1 is set.
var enabled = os.Getenv("JIN_DEBUG") == "1"

// NewLogger returns a debug logging function that writes to
// $XDG_STATE_HOME/jind-ai/<filename> (default ~/.local/state/jind-ai/<filename>)
// when JIN_DEBUG=1 is set.
// When debugging is disabled or the state directory cannot be resolved,
// the returned function is a no-op.
func NewLogger(filename string) func(string, ...any) {
	if !enabled {
		return func(string, ...any) {}
	}

	stateDir, ok := paths.StateOrEmpty()
	if !ok {
		return func(string, ...any) {}
	}
	logPath := filepath.Join(stateDir, filename)

	return func(format string, args ...any) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		msg := fmt.Sprintf(format, args...)
		fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05.000"), msg)
	}
}
