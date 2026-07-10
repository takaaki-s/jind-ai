package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/transcript"
)

func TestSmartTruncate(t *testing.T) {
	const max = 60

	// A 70-byte ASCII string with word boundaries at 20 and 50 bytes.
	// "aaaa aaaa aaaa aaaa bbbbb ccccc ddddd eeeee fffff ggggg hhhhh iiiii jjjjjj"
	long := "abcdefghij klmnopqrst uvwxyzABCD EFGHIJKLMN OPQRSTUVWX YZ0123456789abcd"

	// 70-byte ASCII string with no whitespace at all — forces hard cut.
	dense := strings.Repeat("x", 70)

	// Long UTF-8 string with no word boundaries: 21 copies of "あ" = 63 bytes.
	// A naive byte cut at 60 would land inside the 20th rune (bytes 57..59 form
	// rune 20; bytes 60..62 form rune 21). Cutting at byte 60 is exactly at a
	// rune boundary, so the utf8 back-off must not fire. Add an extra byte at
	// the end to guarantee the input exceeds the budget.
	jp := strings.Repeat("あ", 21) // 63 bytes

	tests := []struct {
		name       string
		in         string
		wantExact  string // if non-empty, exact match required
		wantMaxLen int    // otherwise, len(result) <= wantMaxLen (bytes, including ellipsis)
		wantSuffix string // optional suffix check (e.g. "…")
	}{
		{
			name:      "short input is returned unchanged",
			in:        "hello",
			wantExact: "hello",
		},
		{
			name:      "exactly maxBytes is returned unchanged",
			in:        strings.Repeat("a", max),
			wantExact: strings.Repeat("a", max),
		},
		{
			name:      "newline splits and takes the first line",
			in:        "first line\nsecond line goes here too",
			wantExact: "first line",
		},
		{
			name:      "whitespace only after newline strip yields empty",
			in:        "\n\n\n",
			wantExact: "",
		},
		{
			name:       "long ASCII with boundary cuts at whitespace",
			in:         long,
			wantMaxLen: max + len("…"),
			wantSuffix: "…",
		},
		{
			name:       "no whitespace performs hard cut",
			in:         dense,
			wantMaxLen: max + len("…"),
			wantSuffix: "…",
		},
		{
			name:       "utf8 hard cut backs off to a rune boundary",
			in:         jp,
			wantMaxLen: max + len("…"),
			wantSuffix: "…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := smartTruncate(tc.in, max)
			if tc.wantExact != "" || tc.wantMaxLen == 0 {
				if got != tc.wantExact {
					t.Errorf("got = %q, want %q", got, tc.wantExact)
				}
				return
			}
			if len(got) > tc.wantMaxLen {
				t.Errorf("len(got) = %d, want <= %d (got=%q)", len(got), tc.wantMaxLen, got)
			}
			if tc.wantSuffix != "" && !strings.HasSuffix(got, tc.wantSuffix) {
				t.Errorf("got = %q, want suffix %q", got, tc.wantSuffix)
			}
			if !utf8.ValidString(got) {
				t.Errorf("got = %q is not valid UTF-8", got)
			}
		})
	}
}

// newEnhancer builds a CCDescriptionEnhancer bound to a fresh, empty temp dir
// set as HOME, and returns that dir so callers can populate a fake transcript
// via writeAITitle or a fake session name file via writeSessionName. Owning
// HOME setup here (rather than leaving it to each subtest) guarantees the
// lookup never accidentally matches real files on the developer's machine.
func newEnhancer(t *testing.T) (*CCDescriptionEnhancer, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return &CCDescriptionEnhancer{
		reader:     transcript.NewReader(),
		nameReader: NewCCSessionNameReader(),
	}, home
}

// writeAITitle writes a fake ~/.claude/projects/<encoded>/<sessionID>.jsonl
// containing a single `type:"ai-title"` entry, mirroring what Claude Code
// emits when it names the conversation. workDir must match the sess.WorkDir
// used in the corresponding TryGenerate call (or its CurrentWorkDir), since
// the transcript reader locates files by the workDir→encoded-path mapping.
func writeAITitle(t *testing.T, home, workDir, sessionID, title string) {
	t.Helper()
	encoded := strings.ReplaceAll(workDir, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	line := `{"type":"ai-title","aiTitle":` + jsonString(t, title) + `,"sessionId":` + jsonString(t, sessionID) + "}\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal string: %v", err)
	}
	return string(raw)
}

// writeSessionName writes a fake ~/.claude/sessions/<id>.json file so
// CCSessionNameReader.LookupName can find it by sessionId, mirroring the
// layout CC 2.x writes at SessionStart. Pass nameSource="" to omit the field
// (mirrors older CC versions).
func writeSessionName(t *testing.T, home, sessionID, name, nameSource string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	payload := map[string]string{"sessionId": sessionID, "name": name}
	if nameSource != "" {
		payload["nameSource"] = nameSource
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal session name: %v", err)
	}
	path := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCCDescriptionEnhancer_TryGenerate(t *testing.T) {
	t.Run("nil session returns pending", func(t *testing.T) {
		e, _ := newEnhancer(t)
		if got, _, ok := e.TryGenerate(nil); ok || got != "" {
			t.Fatalf("TryGenerate(nil) = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("empty AgentSessionID returns pending", func(t *testing.T) {
		e, _ := newEnhancer(t)
		sess := &session.Session{WorkDir: "/tmp/foo"}
		if got, _, ok := e.TryGenerate(sess); ok || got != "" {
			t.Fatalf("TryGenerate(empty CC id) = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("missing name file returns pending", func(t *testing.T) {
		e, _ := newEnhancer(t)
		sess := &session.Session{
			WorkDir:        "/tmp/foo",
			AgentSessionID: "cc-missing",
		}
		if got, _, ok := e.TryGenerate(sess); ok || got != "" {
			t.Fatalf("TryGenerate(missing name) = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	// A "derived" name is the tmux hint that jind-ai itself passed CC; we
	// accept it but flag it as the weak Layer C-name sublayer so a later
	// stronger name can overwrite it.
	t.Run("derived name maps to LayerAgentNameDerived", func(t *testing.T) {
		e, home := newEnhancer(t)
		sessionID := "cc-derived"
		writeSessionName(t, home, sessionID, "jin-395bce5c-71", "derived")

		sess := &session.Session{WorkDir: "/tmp/derived", AgentSessionID: sessionID}
		got, layer, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
		}
		if layer != session.DescriptionLayerAgentNameDerived {
			t.Errorf("layer = %d, want %d (DescriptionLayerAgentNameDerived)", layer, session.DescriptionLayerAgentNameDerived)
		}
		if got != "jin-395bce5c-71" {
			t.Errorf("got = %q, want %q", got, "jin-395bce5c-71")
		}
	})

	// A non-derived name (typical value when CC has re-classified the field
	// from the conversation) rides the strong Layer C-name sublayer so it
	// wins over any previously-persisted derived candidate.
	t.Run("non-derived name maps to LayerAgentName", func(t *testing.T) {
		e, home := newEnhancer(t)
		sessionID := "cc-strong"
		writeSessionName(t, home, sessionID, "auth-refactor", "generated")

		sess := &session.Session{WorkDir: "/tmp/strong", AgentSessionID: sessionID}
		got, layer, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
		}
		if layer != session.DescriptionLayerAgentName {
			t.Errorf("layer = %d, want %d (DescriptionLayerAgentName)", layer, session.DescriptionLayerAgentName)
		}
		if got != "auth-refactor" {
			t.Errorf("got = %q, want %q", got, "auth-refactor")
		}
	})

	// A name with no nameSource field (older CC versions, or hand-authored
	// files) is treated as strong: absence of the flag is not the same as
	// an explicit "derived" tag, and refusing to promote it would leave
	// legacy sessions stuck at Baseline.
	t.Run("missing nameSource is treated as strong", func(t *testing.T) {
		e, home := newEnhancer(t)
		sessionID := "cc-legacy"
		writeSessionName(t, home, sessionID, "legacy-name", "")

		sess := &session.Session{WorkDir: "/tmp/legacy", AgentSessionID: sessionID}
		got, layer, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
		}
		if layer != session.DescriptionLayerAgentName {
			t.Errorf("layer = %d, want %d (DescriptionLayerAgentName)", layer, session.DescriptionLayerAgentName)
		}
		if got != "legacy-name" {
			t.Errorf("got = %q, want %q", got, "legacy-name")
		}
	})

	// aiTitle is the transcript-embedded "Session name" CC surfaces in
	// /status. When present it should win over the session-name file at
	// the same DescriptionLayerAgentName tier — the layer's strict-greater
	// promotion guard then keeps whichever landed first, but the ordering
	// here documents the informativeness preference.
	t.Run("aiTitle drives LayerAgentName even when derived name is also present", func(t *testing.T) {
		e, home := newEnhancer(t)
		workDir := "/tmp/ai-title"
		sessionID := "cc-ai-title"
		writeAITitle(t, home, workDir, sessionID, "リポジトリの目的を確認")
		// A derived name is also on disk, but aiTitle must be picked first.
		writeSessionName(t, home, sessionID, "jin-395bce5c-71", "derived")

		sess := &session.Session{WorkDir: workDir, AgentSessionID: sessionID}
		got, layer, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
		}
		if layer != session.DescriptionLayerAgentName {
			t.Errorf("layer = %d, want %d (DescriptionLayerAgentName)", layer, session.DescriptionLayerAgentName)
		}
		if got != "リポジトリの目的を確認" {
			t.Errorf("got = %q, want %q", got, "リポジトリの目的を確認")
		}
	})

	// When the transcript exists but has no aiTitle entry yet (the common
	// state right after SessionStart), we must fall through to the session
	// name file rather than fail closed.
	t.Run("no aiTitle falls through to session name file", func(t *testing.T) {
		e, home := newEnhancer(t)
		workDir := "/tmp/no-ai-title"
		sessionID := "cc-no-ai-title"
		// Transcript file exists but has no ai-title line.
		encoded := strings.ReplaceAll(workDir, "/", "-")
		dir := filepath.Join(home, ".claude", "projects", encoded)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, sessionID+".jsonl")
		if err := os.WriteFile(path,
			[]byte(`{"type":"mode","mode":"normal","sessionId":"`+sessionID+`"}`+"\n"),
			0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		writeSessionName(t, home, sessionID, "jin-derived-name", "derived")

		sess := &session.Session{WorkDir: workDir, AgentSessionID: sessionID}
		got, layer, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
		}
		if layer != session.DescriptionLayerAgentNameDerived {
			t.Errorf("layer = %d, want %d (DescriptionLayerAgentNameDerived)", layer, session.DescriptionLayerAgentNameDerived)
		}
		if got != "jin-derived-name" {
			t.Errorf("got = %q, want %q", got, "jin-derived-name")
		}
	})
}
