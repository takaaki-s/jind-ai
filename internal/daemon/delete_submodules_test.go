package daemon

import (
	"errors"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestDeleteClientPreservesSubmoduleConfirmation(t *testing.T) {
	sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Error: session.ErrWorktreeSubmodules.Error()})
	if err := NewClient(sock).Delete("session", true, false); !errors.Is(err, session.ErrWorktreeSubmodules) {
		t.Fatalf("lost confirmation reason: %v", err)
	}
}
