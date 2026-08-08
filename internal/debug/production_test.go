package debug

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/procgroup"
)

// TestProductionBinaryWritesTheLog is the only test here that can fail on the
// mutation the others are blind to.
//
// The guard in logDir asks whether this process is a test binary, and inside
// one the answer is always yes. So a version that answered yes unconditionally
// — turning debug logging off in the daemon, the hook and every plugin — would
// satisfy every in-process test in this package, including the one that checks
// the guard is wired to something real. The distinction only exists in a
// process that is not a test, which is why this one starts one.
//
// The counterpart is TestNewLogger_WritesNothingFromATestBinary. Together they
// say the whole rule: a real jin writes, a test binary does not.
//
// It also carries the flag itself, for the same reason. `enabled` is read once
// when the package is initialized, so no test can arrange the environment it
// was read from — and a process started from here is the only place the reading
// is repeated.
func TestProductionBinaryWritesTheLog(t *testing.T) {
	// The probe is not a build input of this package, so read it to record it
	// as one — otherwise a cached "ok" can be reported for a probe that has
	// changed — and to report a missing one as itself rather than as a build
	// failure.
	if _, err := os.ReadFile(filepath.Join("testdata", "logprobe", "main.go")); err != nil {
		t.Fatalf("the probe this test depends on is not readable: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		// Skipping hides the mutation above, so say which one.
		t.Skipf("no go tool on PATH; the production branch of NewLogger goes unobserved: %v", err)
	}

	// Two destinations, because the resolver has two ways of arriving at one:
	// XDG_STATE_HOME when set, the home directory otherwise. Both are exercised
	// below, so neither route can be the one nobody ever runs.
	//
	// They are created up front because the logger does not create its own
	// directory, which would make a missing one indistinguishable from a
	// refusal to write — and which of those happened is the whole assertion.
	stateHome := t.TempDir()
	stateDir := stateDirUnder(t, stateHome)
	homeDir := t.TempDir()
	homeStateDir := stateDirUnder(t, filepath.Join(homeDir, ".local", "state"))
	for _, dir := range []string{stateDir, homeStateDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	// Build once, then run the binary, rather than `go run` per case. The probe
	// needs HOME pointed away from the real one — it is the fallback the state
	// directory resolver uses, and this is the only test in the project that
	// runs production logging code in a process of its own — but the go tool
	// keeps its build cache under HOME, so handing it the same environment
	// would recompile the standard library on every run. Measured: ~0.2s built
	// once against ~5s per `go run`.
	probeBin := filepath.Join(t.TempDir(), "logprobe")
	// Bounded because this compiles, and a compile that wedges would otherwise
	// consume the whole package's timeout. Built through procgroup so the
	// deadline reaches what the go tool started, not only the go tool.
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelBuild()
	build := procgroup.CommandContext(buildCtx, "go", "build", "-o", probeBin, "./testdata/logprobe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the probe failed: %v\n%s", err, out)
	}

	// Each case states its whole environment rather than overriding parts of
	// the ambient one, so "the variable is not set at all" is expressible.
	// That is the state every user who has never heard of the flag runs in, and
	// a reading widened to accept it would turn logging on for all of them.
	base := probeEnv()

	for _, tt := range []struct {
		name string
		env  []string
		dir  string
		want bool
	}{{
		name: "flag on",
		env:  []string{"JIN_DEBUG=1", "XDG_STATE_HOME=" + stateHome, "HOME=" + homeDir},
		dir:  stateDir,
		want: true,
	}, {
		// "0" rather than only "off-ish": an operator can name JIN_DEBUG=0 in
		// their own config, which session.Manager passes through to the agent.
		name: "flag set to zero",
		env:  []string{"JIN_DEBUG=0", "XDG_STATE_HOME=" + stateHome, "HOME=" + homeDir},
		dir:  stateDir,
		want: false,
	}, {
		name: "flag not set",
		env:  []string{"XDG_STATE_HOME=" + stateHome, "HOME=" + homeDir},
		dir:  stateDir,
		want: false,
	}, {
		// No XDG_STATE_HOME, so the destination comes from HOME. This is the
		// route that decides whether pointing HOME away from the real one is
		// doing anything: if it were not, this case would write into the
		// developer's own state directory instead of failing.
		name: "destination resolved from the home directory",
		env:  []string{"JIN_DEBUG=1", "HOME=" + homeDir},
		dir:  homeStateDir,
		want: true,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// A name of its own, so a case that expects no file is reading a
			// destination no other case has ever written to. Sharing one and
			// removing it afterwards would make each case depend on the order
			// it ran in.
			logName := strings.ReplaceAll(tt.name, " ", "-") + ".log"
			cmd := procgroup.CommandContext(ctx, probeBin, logName)
			// Cloned rather than appended to in place: base has spare capacity,
			// so every case would otherwise write its overrides into the same
			// backing array. Harmless while these run one after another, which
			// is not a property worth depending on.
			cmd.Env = append(slices.Clone(base), tt.env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("running the probe failed: %v\n%s", err, out)
			}

			data, err := os.ReadFile(filepath.Join(tt.dir, logName))
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("reading the log: %v", err)
			}

			if !tt.want {
				if data != nil {
					t.Errorf("a line was written where none was due: %q", data)
				}
				return
			}
			if data == nil {
				t.Fatal("a process that is not a test binary wrote no log")
			}
			// Asserting the content, not just the file: an empty file created
			// by O_CREATE and never written to would satisfy a stat.
			if got := string(data); !strings.Contains(got, "probe line 42") {
				t.Errorf("log content %q does not carry the line the probe wrote", got)
			}
		})
	}
}

// probeEnv is the ambient environment with everything the probe reads removed,
// so a case that omits one of them is running without it rather than inheriting
// whoever started the suite.
func probeEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "JIN_DEBUG="),
			strings.HasPrefix(kv, "XDG_STATE_HOME="),
			strings.HasPrefix(kv, "HOME="):
			continue
		}
		out = append(out, kv)
	}
	return out
}
