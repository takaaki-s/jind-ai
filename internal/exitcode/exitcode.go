// Package exitcode defines exit code constants and the ExitError type for CLI commands.
package exitcode

import "fmt"

// Exit code constants.
const (
	Success          = 0
	GeneralError     = 1
	SessionNotFound  = 2
	DaemonNotRunning = 3
	Timeout          = 4
	WorktreeDirty    = 5
)

// ExitError represents an error with a specific exit code.
type ExitError struct {
	Code    int
	Message string
	Err     error // wrapped error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *ExitError) Unwrap() error {
	return e.Err
}

// Errorf creates a new ExitError with a formatted message.
func Errorf(code int, format string, args ...any) *ExitError {
	return &ExitError{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
	}
}

// Wrap creates a new ExitError wrapping an existing error.
func Wrap(err error, code int, message string) *ExitError {
	return &ExitError{
		Code:    code,
		Message: message,
		Err:     err,
	}
}
