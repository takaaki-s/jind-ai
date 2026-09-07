package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/agent/agenttest"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// TestHandleNew_UnknownAgentKind exercises the validation branch of
// handleNew without spinning a full Server. Only the registry is touched;
// unknown kinds must produce Response.Success=false with a message that
// names the requested kind and lists the available ones.
//
// The test uses Server{} directly: handleNew's unknown-kind branch returns
// before it touches configMgr / manager, so the zero-value Server is safe.
func TestHandleNew_UnknownAgentKind(t *testing.T) {
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()

	agent.Register(&agenttest.StubAgent{KindStr: "claude"})

	s := &Server{}
	data, _ := json.Marshal(NewRequest{AgentKind: "codex", WorkDir: "/tmp"})
	resp := s.handleNew(data)

	if resp.Success {
		t.Fatalf("expected Success=false, got success with data=%s", resp.Data)
	}
	if !strings.Contains(resp.Error, "unknown agent kind: codex") {
		t.Errorf("Error = %q, want to contain 'unknown agent kind: codex'", resp.Error)
	}
	if !strings.Contains(resp.Error, "claude") {
		t.Errorf("Error = %q, want to list 'claude' as available", resp.Error)
	}
}

// newAsyncTestServer builds a Server backed by a real session.Manager over temp
// dirs, wired for handler tests. The tmux client, hook runner, and plugin
// dispatcher are left nil — handlers that need them error, which the test
// can pin without spinning them up.
func newAsyncTestServer(t *testing.T) *Server {
	t.Helper()
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()
	agent.Register(&agenttest.StubAgent{KindStr: "claude"})

	configDir := t.TempDir()
	stateDir := t.TempDir()
	sessionsDir := t.TempDir()

	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	stateMgr, err := config.NewStateManager(stateDir)
	if err != nil {
		t.Fatalf("config.NewStateManager: %v", err)
	}
	// A socket no test dials: these handlers never reach a daemon, but the
	// Manager passes the value on to the agents it starts, so it must not be
	// one that could collide with a real one.
	identity := jinenv.Identity{SocketPath: "/nonexistent/handler-test.sock"}
	mgr, err := session.NewManager(sessionsDir, stateDir, identity, configMgr)
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	// Mirrors NewServer: handlers that read a transcript through the manager
	// (handleGet) resolve the adapter from here, not from agent.Lookup, so a
	// fixture without it stops standing in for the server it claims to build.
	mgr.SetAgentResolver(agentResolverAdapter{})
	return &Server{
		manager:   mgr,
		configMgr: configMgr,
		stateMgr:  stateMgr,
	}
}

// waitFor polls fn every 5ms up to a fixed timeout so async assertions
// converge without hardcoded sleeps that would either flake or slow the
// suite. Fails the test with reason on timeout.
func waitFor(t *testing.T, timeout time.Duration, reason string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, reason)
}

// waitForHandleNewGoroutine blocks until the handleNew goroutine has
// released createMu, guaranteeing its final store.Save has landed. Without
// this, t.TempDir cleanup can race with the atomic rename inside Save and
// fail with "directory not empty" — the goroutine's manager map write
// happens before Save, so waiting on GetInfo alone is not enough.
func waitForHandleNewGoroutine(t *testing.T, s *Server) {
	t.Helper()
	waitFor(t, 3*time.Second, "handleNew goroutine to release createMu", func() bool {
		if !s.createMu.TryLock() {
			return false
		}
		s.createMu.Unlock()
		return true
	})
}

// TestHandleNew_ReturnsBeforeProvisioning verifies the handler acknowledges
// a non-worktree new request in well under the request/response cost of the
// provisioning goroutine — non-worktree ProvisionAsync is a no-op but the
// goroutine still runs SetStatus(Stopped) after the response is out. The
// response must carry StatusCreating (the reservation state), and a
// short-window poll must see Status transition to Stopped.
func TestHandleNew_ReturnsBeforeProvisioning(t *testing.T) {
	s := newAsyncTestServer(t)
	workDir := t.TempDir()
	data, _ := json.Marshal(NewRequest{WorkDir: workDir, AgentKind: "claude"})

	start := time.Now()
	resp := s.handleNew(data)
	elapsed := time.Since(start)
	if !resp.Success {
		t.Fatalf("Success = false: %s", resp.Error)
	}
	if elapsed > time.Second {
		t.Errorf("handleNew took %s, want <1s (async path)", elapsed)
	}

	var out NewResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Info.Status != session.StatusCreating {
		t.Errorf("initial Status = %q, want %q", out.Info.Status, session.StatusCreating)
	}
	if out.Info.ID == "" {
		t.Fatal("Info.ID empty, want a reserved session id")
	}

	// Goroutine transitions Status to Stopped (Start=false path).
	waitFor(t, 2*time.Second, "status to reach Stopped", func() bool {
		info, ok := s.manager.GetInfo(out.Info.ID)
		return ok && info.Status == session.StatusStopped
	})
	waitForHandleNewGoroutine(t, s)
}

// TestHandleNew_ProvisionFailure_LeavesSessionInErrorState uses a scripted
// git runner that fails `worktree add` to trigger a provisioning error, then
// asserts the async goroutine leaves the session at Status=Stopped with a
// populated ErrorMessage — the state a `get` after acceptance must observe.
func TestHandleNew_ProvisionFailure_LeavesSessionInErrorState(t *testing.T) {
	s := newAsyncTestServer(t)
	workDir := t.TempDir()
	// Make it look like a git repo so ProvisionAsync gets past the
	// IsGitRoot check and reaches the runner-driven git subprocess.
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	s.manager.SetGitClient(git.NewClientWithRunner(&failingAddWorktreeRunner{}))

	data, _ := json.Marshal(NewRequest{
		WorkDir:   workDir,
		AgentKind: "claude",
		Worktree:  true,
	})
	resp := s.handleNew(data)
	if !resp.Success {
		t.Fatalf("Success = false: %s", resp.Error)
	}

	var out NewResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	waitFor(t, 2*time.Second, "session to reach Stopped with ErrorMessage", func() bool {
		info, ok := s.manager.GetInfo(out.Info.ID)
		if !ok {
			return false
		}
		return info.Status == session.StatusStopped && info.ErrorMessage != ""
	})
	waitForHandleNewGoroutine(t, s)

	info, _ := s.manager.GetInfo(out.Info.ID)
	if !strings.Contains(info.ErrorMessage, "worktree add") {
		t.Errorf("ErrorMessage = %q, want it to mention worktree add failure", info.ErrorMessage)
	}
}

// TestHandleNew_ConcurrentSerializedByCreateMu pins the createMu contract:
// two concurrent worktree news must never have overlapping provisioning
// goroutines. The scripted git runner reports overlap by incrementing an
// in-flight counter around the `worktree add` call — a counter > 1 fails
// the test regardless of which call site "won".
func TestHandleNew_ConcurrentSerializedByCreateMu(t *testing.T) {
	s := newAsyncTestServer(t)
	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	runner := &overlapDetectingRunner{}
	s.manager.SetGitClient(git.NewClientWithRunner(runner))

	data, _ := json.Marshal(NewRequest{
		WorkDir:   workDir,
		AgentKind: "claude",
		Worktree:  true,
	})

	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			s.handleNew(data)
			done <- struct{}{}
		}()
	}

	// Wait for both goroutines' provisioning to complete. handleNew itself
	// returns quickly (that's the async property), so <-done pairs with the
	// handler return, not the goroutine completion — poll the manager
	// instead for the runner's final call count.
	<-done
	<-done
	waitFor(t, 3*time.Second, "both provisioning goroutines to finish", func() bool {
		return atomic.LoadInt64(&runner.completed) >= 2
	})
	// Second goroutine may still be in its `defer createMu.Unlock()` when
	// completed++ fires; wait for the createMu to be released so t.TempDir
	// cleanup does not race with a lingering store.Save.
	waitForHandleNewGoroutine(t, s)

	if max := atomic.LoadInt64(&runner.maxInFlight); max > 1 {
		t.Errorf("max concurrent worktree add = %d, want <=1 (createMu contract)", max)
	}
}

// failingAddWorktreeRunner walks the worktree-provisioning git path far
// enough to hit `worktree add`, then fails. Prior calls (prune, base-detect,
// branch pre-check) succeed so we exercise the actual failure branch that
// leads to MarkCreationFailed rather than an early-return failure elsewhere.
type failingAddWorktreeRunner struct{}

const daemonTestReviewBaseOID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func isDaemonTestResolveCommit(args []string) bool {
	return len(args) == 5 && args[0] == "rev-parse" &&
		args[1] == "--verify" && args[2] == "--quiet" &&
		args[3] == "--end-of-options" && args[4] == "origin/main^{commit}"
}

func (failingAddWorktreeRunner) Run(dir string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case joined == "symbolic-ref refs/remotes/origin/HEAD":
		return []byte("refs/remotes/origin/main\n"), nil
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
		return nil, nil
	case isDaemonTestResolveCommit(args):
		return []byte(daemonTestReviewBaseOID + "\n"), nil
	case len(args) >= 1 && args[0] == "rev-parse":
		return nil, errors.New("exit status 1") // branch does not exist
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
		return []byte("simulated failure"), errors.New("exit status 128")
	}
	return nil, fmt.Errorf("unexpected git call: %s", joined)
}

// overlapDetectingRunner reports the maximum concurrent `worktree add`
// invocations across all callers. Non-add calls succeed transparently. The
// add call sleeps briefly so a broken lock has time to expose overlap; on
// success it materializes the .git file so the caller's rollback path
// (which parses it) works if the flow ever reaches removal.
type overlapDetectingRunner struct {
	inFlight    int64
	maxInFlight int64
	completed   int64
}

func (r *overlapDetectingRunner) Run(dir string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case joined == "symbolic-ref refs/remotes/origin/HEAD":
		return []byte("refs/remotes/origin/main\n"), nil
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
		return nil, nil
	case isDaemonTestResolveCommit(args):
		return []byte(daemonTestReviewBaseOID + "\n"), nil
	case len(args) >= 1 && args[0] == "rev-parse":
		return nil, errors.New("exit status 1")
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
		cur := atomic.AddInt64(&r.inFlight, 1)
		for {
			m := atomic.LoadInt64(&r.maxInFlight)
			if cur <= m || atomic.CompareAndSwapInt64(&r.maxInFlight, m, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond) // let the other goroutine reach here if it can
		atomic.AddInt64(&r.inFlight, -1)
		atomic.AddInt64(&r.completed, 1)

		worktreePath := args[4]
		mainGitDir := filepath.Join(dir, ".git", "worktrees", filepath.Base(worktreePath))
		_ = os.MkdirAll(mainGitDir, 0o755)
		_ = os.MkdirAll(worktreePath, 0o755)
		_ = os.WriteFile(filepath.Join(worktreePath, ".git"),
			[]byte("gitdir: "+mainGitDir+"\n"), 0o644)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected git call: %s", joined)
}

// A model dropped here goes unnoticed by everything below: session and the
// adapters are handed whatever CreateOptions holds, and a session that names no
// model is valid all the way down. Reading it back off the create response is
// what pins the mapping.
//
// The request is spelled as wire text rather than marshalled from NewRequest,
// because docs/ipc-protocol.md publishes these key names: a struct that
// round-trips through itself agrees with whatever tag it happens to carry, so
// renaming one would break other clients with the suite still green.
func TestHandleNew_CarriesTheModelOntoTheSession(t *testing.T) {
	s := newAsyncTestServer(t)
	data := json.RawMessage(fmt.Sprintf(
		`{"work_dir":%q,"agent_kind":"claude","model":"opus"}`, t.TempDir()))

	resp := s.handleNew(data)
	if !resp.Success {
		t.Fatalf("Success = false: %s", resp.Error)
	}

	var out NewResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Info.Model != "opus" {
		t.Errorf("Info.Model = %q, want %q — the request's model never reached the session", out.Info.Model, "opus")
	}

	// The response key for the same reason as the request key: `jin session
	// info --json` is documented output that scripts read, and decoding into
	// NewResponse would accept any spelling the struct happens to carry.
	var wire map[string]any
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if wire["model"] != "opus" {
		t.Errorf("response key `model` = %v, want %q", wire["model"], "opus")
	}
	waitForHandleNewGoroutine(t, s)
}
