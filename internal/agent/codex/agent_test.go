package codex

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestAgent_Kind(t *testing.T) {
	if got := New().Kind(); got != "codex" {
		t.Errorf("Kind() = %q, want %q", got, "codex")
	}
}

func TestAgent_Setup_NoFileWrites(t *testing.T) {
	// The Codex adapter must not touch the user's global config files —
	// hooks are injected per-invocation via -c (§3.3). Setup with a fresh
	// HOME + CODEX_HOME and verify nothing lands under either.
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	a := New()
	if err := a.Setup(agent.SetupContext{
		StateDir: t.TempDir(),
		ExecPath: "/usr/local/bin/jin",
		WorkDir:  t.TempDir(),
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	for _, root := range []string{home, codexHome} {
		count := 0
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				count++
				t.Errorf("unexpected file written under %s: %s", root, path)
			}
			return nil
		})
		if count > 0 {
			t.Errorf("Setup wrote %d files under %s; expected 0 (hooks are injected via -c, §3.3)", count, root)
		}
	}
}

func TestAgent_Setup_CapturesExecPath(t *testing.T) {
	a := New()
	if err := a.Setup(agent.SetupContext{ExecPath: "/opt/jin"}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	plan := a.SpawnCommand(agent.SpawnOptions{})
	if !strings.Contains(plan.Command, "/opt/jin hook") {
		t.Errorf("SpawnCommand does not use captured ExecPath: %q", plan.Command)
	}
}

func TestAgent_Setup_IsIdempotent(t *testing.T) {
	// setupOnce means only the first Setup wins — a second Setup with a
	// different ExecPath (unrealistic in practice, but possible if the
	// daemon reconfigures) must not silently swap the value out under a
	// live SpawnCommand caller.
	a := New()
	if err := a.Setup(agent.SetupContext{ExecPath: "/first/jin"}); err != nil {
		t.Fatalf("Setup(first): %v", err)
	}
	if err := a.Setup(agent.SetupContext{ExecPath: "/second/jin"}); err != nil {
		t.Fatalf("Setup(second): %v", err)
	}
	plan := a.SpawnCommand(agent.SpawnOptions{})
	if !strings.Contains(plan.Command, "/first/jin hook") {
		t.Errorf("second Setup overwrote first — Command=%q", plan.Command)
	}
	if strings.Contains(plan.Command, "/second/jin") {
		t.Errorf("second Setup leaked into Command=%q", plan.Command)
	}
}

func TestAgent_Setup_ConcurrentSafe(t *testing.T) {
	// The Agent contract allows Setup and SpawnCommand from parallel
	// goroutines. setupOnce + immutable fields make this safe; the test
	// hammers 32 concurrent starters to check the race detector agrees.
	a := New()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = a.Setup(agent.SetupContext{ExecPath: "/race/jin"})
			plan := a.SpawnCommand(agent.SpawnOptions{})
			if !strings.Contains(plan.Command, "codex") {
				t.Errorf("goroutine %d got empty Command: %q", i, plan.Command)
			}
		}(i)
	}
	wg.Wait()
}

func TestAgent_SpawnCommand_BeforeSetup(t *testing.T) {
	// If SpawnCommand fires before Setup (defensive), execPath is empty
	// and HookArgs returns nil, yielding a bare `codex`. This ensures the
	// session still starts even if Setup was skipped or failed silently.
	a := New()
	plan := a.SpawnCommand(agent.SpawnOptions{})
	if plan.Command != "codex" {
		t.Errorf("pre-Setup Command = %q, want %q", plan.Command, "codex")
	}
}

func TestAgent_StatusSource_NonNil(t *testing.T) {
	if src := New().StatusSource(); src == nil {
		t.Error("StatusSource() = nil, want *HookStatusSource")
	}
}

func TestAgent_Description_NonNil(t *testing.T) {
	if enh := New().Description(); enh == nil {
		t.Error("Description() = nil, want *DescriptionEnhancer")
	}
}

func TestAgent_ImplementsInterface(t *testing.T) {
	// Compile-time interface check: Agent must satisfy session.Agent so
	// register.go can hand it to agent.Register().
	var _ session.Agent = (*Agent)(nil)
	var _ session.Agent = New()
}
