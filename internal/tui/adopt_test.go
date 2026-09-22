package tui

import (
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

func TestBuildInnerAttachCmdForAdoptedServer(t *testing.T) {
	path := tmux.ServerRef{Kind: tmux.ServerPath, Value: "/tmp/tmux user/default"}
	got := buildInnerAttachCmdForServer(path, "work session")
	if !strings.Contains(got, "-S '/tmp/tmux user/default'") || !strings.Contains(got, "-t 'work session'") {
		t.Fatalf("attach command = %q", got)
	}
	if !strings.Contains(got, "env -u TMUX") || !strings.HasSuffix(got, "; tail -f /dev/null") {
		t.Fatalf("attach command lost nesting/keepalive guards: %q", got)
	}
}

func TestTmuxServerForInfoUsesAdoptedBinding(t *testing.T) {
	server := tmux.ServerRef{Kind: tmux.ServerName, Value: "foreign"}
	info := session.Info{TmuxBinding: session.TmuxBinding{Ownership: session.TmuxOwnershipAdopted, Server: server}}
	if got := tmuxServerForInfo(&info); got != server {
		t.Fatalf("tmuxServerForInfo = %+v, want %+v", got, server)
	}
}
