package tmux

import (
	"reflect"
	"strings"
	"testing"
)

func TestServerRefValidateAndClientArgs(t *testing.T) {
	tests := []struct {
		name string
		ref  ServerRef
		want []string
	}{
		{name: "default", ref: ServerRef{Kind: ServerDefault}, want: nil},
		{name: "named", ref: ServerRef{Kind: ServerName, Value: "work"}, want: []string{"-L", "work"}},
		{name: "path", ref: ServerRef{Kind: ServerPath, Value: "/tmp/tmux/default"}, want: []string{"-S", "/tmp/tmux/default"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.ref.Validate(); err != nil {
				t.Fatal(err)
			}
			client := &Client{tmuxPath: "tmux"}
			switch tt.ref.Kind {
			case ServerName:
				client.socketName = tt.ref.Value
			case ServerPath:
				client.socketPath = tt.ref.Value
			}
			if got := client.baseArgs(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("baseArgs = %v, want %v", got, tt.want)
			}
			if got := client.ServerRef(); got != tt.ref {
				t.Errorf("ServerRef = %+v, want %+v", got, tt.ref)
			}
		})
	}
	for _, bad := range []ServerRef{{}, {Kind: ServerDefault, Value: "x"}, {Kind: ServerName}, {Kind: ServerPath}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) succeeded", bad)
		}
	}
}

func TestParseProcessDescendants_ParentBeforeChildrenAndBoundedToPane(t *testing.T) {
	input := []byte("1 0 init\n42 1 zsh\n45 42 node helper\n44 42 claude\n46 44 rg\n99 1 unrelated\n")
	got, err := parseProcessDescendants(input, 42)
	if err != nil {
		t.Fatal(err)
	}
	want := []PaneProcess{
		{PID: 42, PPID: 1, Command: "zsh"},
		{PID: 44, PPID: 42, Command: "claude"},
		{PID: 45, PPID: 42, Command: "node helper"},
		{PID: 46, PPID: 44, Command: "rg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("process tree = %+v, want %+v", got, want)
	}
	if _, err := parseProcessDescendants(input, 404); err == nil || !strings.Contains(err.Error(), "no longer present") {
		t.Fatalf("missing-root error = %v", err)
	}
}

func TestPaneInfoCanonicalTarget(t *testing.T) {
	p := PaneInfo{SessionName: "work", WindowIndex: 3, PaneIndex: 2}
	if got := p.CanonicalTarget(); got != "work:3.2" {
		t.Errorf("CanonicalTarget = %q", got)
	}
}
