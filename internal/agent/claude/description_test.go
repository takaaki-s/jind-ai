package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/takaaki-s/jindaiko/internal/session"
	"github.com/takaaki-s/jindaiko/internal/transcript"
)

func TestInterpretUserPrompt(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		want   string
		wantOk bool
	}{
		{
			name:   "empty string is pending",
			text:   "",
			wantOk: false,
		},
		{
			name:   "whitespace only is pending",
			text:   "   \t \n ",
			wantOk: false,
		},
		{
			name:   "under three bytes is pending",
			text:   "hi",
			wantOk: false,
		},
		{
			// Two-rune multi-byte string is 6 bytes but must still be treated
			// as pending: the minimum applies in runes, not bytes (F005).
			name:   "two rune multibyte is pending",
			text:   "あい",
			wantOk: false,
		},
		{
			// Three-rune multi-byte string clears the rune-length gate.
			name:   "three rune multibyte is accepted",
			text:   "あいう",
			want:   "あいう",
			wantOk: true,
		},
		{
			// Slash-command with 2-rune args is pending: below the 10-rune
			// threshold even though "認証" is 6 bytes.
			name:   "slash command with short multibyte args is pending",
			text:   "/init 認証",
			wantOk: false,
		},
		{
			// Slash-command with 10-rune args (30 bytes) crosses the gate.
			name:   "slash command with long multibyte args is accepted",
			text:   "/init 認証実装をお願いしたいです",
			want:   "認証実装をお願いしたいです",
			wantOk: true,
		},
		{
			name:   "slash command with no args is pending",
			text:   "/init",
			wantOk: false,
		},
		{
			name:   "slash command with short args is pending",
			text:   "/init abcdefg",
			wantOk: false,
		},
		{
			name:   "slash command with meaningful args uses args",
			text:   "/init auth モジュールをリファクタリング",
			want:   "auth モジュールをリファクタリング",
			wantOk: true,
		},
		{
			name:   "plain text is used verbatim",
			text:   "実装してください",
			want:   "実装してください",
			wantOk: true,
		},
		{
			name:   "multi-line text keeps only the first line",
			text:   "line1\nline2 with more content here",
			want:   "line1",
			wantOk: true,
		},
		{
			name:   "leading and trailing whitespace is trimmed",
			text:   "   hello world   ",
			want:   "hello world",
			wantOk: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := interpretUserPrompt(tc.text)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v (got=%q)", ok, tc.wantOk, got)
			}
			if got != tc.want {
				t.Errorf("got = %q, want %q", got, tc.want)
			}
		})
	}
}

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

// writeTranscript encodes entries as JSONL beneath a fake HOME/.claude tree.
// Mirrors the layout used by transcript.Reader so ReadEntries can locate the
// file via HOME override + the workDir → encoded path mapping.
func writeTranscript(t *testing.T, home, workDir, sessionID string, entries []any) {
	t.Helper()
	encoded := strings.ReplaceAll(workDir, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode entry: %v", err)
		}
	}
}

// newEnhancer builds a CCDescriptionEnhancer bound to a fresh, empty temp dir
// set as HOME, and returns that dir so callers can write a transcript beneath
// it via writeTranscript. Owning HOME setup here (rather than leaving it to
// each subtest) guarantees the name-fallback path never accidentally matches
// a real ~/.claude/sessions/*.json on the developer's machine: nothing but
// writeTranscript's own .claude/projects/ tree exists under this HOME, so
// CCSessionNameReader.LookupName always misses unless a subtest deliberately
// populates .claude/sessions/ itself.
func newEnhancer(t *testing.T) (*CCDescriptionEnhancer, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return &CCDescriptionEnhancer{
		reader:     transcript.NewReader(),
		nameReader: NewCCSessionNameReader(),
	}, home
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

	t.Run("missing transcript returns pending", func(t *testing.T) {
		e, _ := newEnhancer(t)
		sess := &session.Session{
			WorkDir:        "/tmp/foo",
			AgentSessionID: "cc-missing",
		}
		if got, _, ok := e.TryGenerate(sess); ok || got != "" {
			t.Fatalf("TryGenerate(missing transcript) = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("first user turn is a slash command with no args → pending", func(t *testing.T) {
		e, home := newEnhancer(t)
		workDir := "/tmp/pending-slash"
		sessionID := "cc-pending"
		writeTranscript(t, home, workDir, sessionID, []any{
			map[string]any{
				"type":      "user",
				"timestamp": "2026-01-01T00:00:00Z",
				"message": map[string]any{
					"role":    "user",
					"content": "/init",
				},
			},
		})
		sess := &session.Session{WorkDir: workDir, AgentSessionID: sessionID}
		got, _, ok := e.TryGenerate(sess)
		if ok || got != "" {
			t.Fatalf("TryGenerate(pending slash) = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("first user turn plain text is used verbatim", func(t *testing.T) {
		e, home := newEnhancer(t)
		workDir := "/tmp/plain"
		sessionID := "cc-plain"
		writeTranscript(t, home, workDir, sessionID, []any{
			map[string]any{
				"type":      "user",
				"timestamp": "2026-01-01T00:00:00Z",
				"message": map[string]any{
					"role":    "user",
					"content": "implement the auth refactor please",
				},
			},
		})
		sess := &session.Session{WorkDir: workDir, AgentSessionID: sessionID}
		got, _, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
		}
		want := "implement the auth refactor please"
		if got != want {
			t.Errorf("got = %q, want %q", got, want)
		}
	})

	t.Run("skips pending user turn and uses the next non-pending prompt", func(t *testing.T) {
		// F003 regression: the head entry ("/init") is a slash command with no
		// args and must be classified as pending. Prior to the fix, the loop
		// returned pending immediately, so the transcript could never move
		// past "/init" no matter how many prompts followed. The enhancer must
		// keep scanning until a real prompt is found.
		e, home := newEnhancer(t)
		workDir := "/tmp/pending-then-real"
		sessionID := "cc-pending-then-real"
		writeTranscript(t, home, workDir, sessionID, []any{
			map[string]any{
				"type":      "user",
				"timestamp": "2026-01-01T00:00:00Z",
				"message": map[string]any{
					"role":    "user",
					"content": "/init",
				},
			},
			map[string]any{
				"type":      "user",
				"timestamp": "2026-01-01T00:00:01Z",
				"message": map[string]any{
					"role":    "user",
					"content": "実装してください",
				},
			},
		})
		sess := &session.Session{WorkDir: workDir, AgentSessionID: sessionID}
		got, _, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true (should skip pending /init and pick next prompt)", got, ok)
		}
		want := "実装してください"
		if got != want {
			t.Errorf("got = %q, want %q", got, want)
		}
	})

	t.Run("skips leading non-user entries and empty text blocks", func(t *testing.T) {
		e, home := newEnhancer(t)
		workDir := "/tmp/skip"
		sessionID := "cc-skip"
		writeTranscript(t, home, workDir, sessionID, []any{
			// Non-user turn first — should be ignored.
			map[string]any{
				"type":      "assistant",
				"timestamp": "2026-01-01T00:00:00Z",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": "hello from the assistant"},
					},
				},
			},
			// User turn whose only text block is whitespace — also skipped.
			map[string]any{
				"type":      "user",
				"timestamp": "2026-01-01T00:00:01Z",
				"message": map[string]any{
					"role":    "user",
					"content": "   \n\t ",
				},
			},
			// Third entry has the real prompt.
			map[string]any{
				"type":      "user",
				"timestamp": "2026-01-01T00:00:02Z",
				"message": map[string]any{
					"role":    "user",
					"content": "actual first user prompt goes here",
				},
			},
		})
		sess := &session.Session{WorkDir: workDir, AgentSessionID: sessionID}
		got, _, ok := e.TryGenerate(sess)
		if !ok {
			t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
		}
		want := "actual first user prompt goes here"
		if got != want {
			t.Errorf("got = %q, want %q", got, want)
		}
	})
}

// writeSessionName writes a fake ~/.claude/sessions/<id>.json file so
// CCSessionNameReader.LookupName can find it by sessionId, mirroring the
// layout CC 2.x writes at SessionStart.
func writeSessionName(t *testing.T, home, sessionID, name string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	raw, err := json.Marshal(map[string]string{"sessionId": sessionID, "name": name})
	if err != nil {
		t.Fatalf("marshal session name: %v", err)
	}
	path := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCCDescriptionEnhancer_FallbackToName(t *testing.T) {
	e, home := newEnhancer(t)
	sessionID := "cc-name-fallback"
	writeSessionName(t, home, sessionID, "jindaiko-d1")

	sess := &session.Session{WorkDir: "/tmp/name-fallback", AgentSessionID: sessionID}
	got, layer, ok := e.TryGenerate(sess)
	if !ok {
		t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
	}
	if layer != session.DescriptionLayerAgentName {
		t.Errorf("layer = %d, want %d (DescriptionLayerAgentName)", layer, session.DescriptionLayerAgentName)
	}
	want := "jindaiko-d1"
	if got != want {
		t.Errorf("got = %q, want %q", got, want)
	}
}

func TestCCDescriptionEnhancer_TranscriptPreferred(t *testing.T) {
	e, home := newEnhancer(t)
	workDir := "/tmp/transcript-preferred"
	sessionID := "cc-transcript-preferred"
	writeTranscript(t, home, workDir, sessionID, []any{
		map[string]any{
			"type":      "user",
			"timestamp": "2026-01-01T00:00:00Z",
			"message": map[string]any{
				"role":    "user",
				"content": "implement the transcript path please",
			},
		},
	})
	// A name is also on disk, but the transcript hit must win since it's the
	// higher layer.
	writeSessionName(t, home, sessionID, "jindaiko-different-name")

	sess := &session.Session{WorkDir: workDir, AgentSessionID: sessionID}
	got, layer, ok := e.TryGenerate(sess)
	if !ok {
		t.Fatalf("TryGenerate = (%q, %v), want ok=true", got, ok)
	}
	if layer != session.DescriptionLayerTranscript {
		t.Errorf("layer = %d, want %d (DescriptionLayerTranscript)", layer, session.DescriptionLayerTranscript)
	}
	want := "implement the transcript path please"
	if got != want {
		t.Errorf("got = %q, want %q", got, want)
	}
}
