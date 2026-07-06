package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSessionFiles creates the ~/.claude/sessions directory under home and
// populates it with the given filename -> raw JSON content pairs.
func writeSessionFiles(t *testing.T, home string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func TestCCSessionNameReader_LookupName(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		lookupID   string
		wantName   string
		wantSource string
		wantOK     bool
	}{
		{
			name: "normal: sessionId matches, name and nameSource returned",
			files: map[string]string{
				"1234.json": `{"sessionId":"uuid-a","name":"jindaiko-1","nameSource":"derived"}`,
			},
			lookupID:   "uuid-a",
			wantName:   "jindaiko-1",
			wantSource: "derived",
			wantOK:     true,
		},
		{
			name: "miss: sessionId does not match any file",
			files: map[string]string{
				"1234.json": `{"sessionId":"uuid-a","name":"jindaiko-1"}`,
			},
			lookupID: "uuid-b",
			wantOK:   false,
		},
		{
			name: "old CC: no name field",
			files: map[string]string{
				"1234.json": `{"sessionId":"uuid-a"}`,
			},
			lookupID: "uuid-a",
			wantOK:   false,
		},
		{
			name: "broken JSON mixed in: valid file is still found",
			files: map[string]string{
				"1234.json": `{`,
				"5678.json": `{"sessionId":"uuid-a","name":"jindaiko-1"}`,
			},
			lookupID: "uuid-a",
			wantName: "jindaiko-1",
			wantOK:   true,
		},
		{
			name: "empty name value is treated as a miss",
			files: map[string]string{
				"1234.json": `{"sessionId":"uuid-a","name":""}`,
			},
			lookupID: "uuid-a",
			wantOK:   false,
		},
		{
			name: "nameSource missing but name non-empty is still a hit",
			files: map[string]string{
				"1234.json": `{"sessionId":"uuid-a","name":"jindaiko-1"}`,
			},
			lookupID:   "uuid-a",
			wantName:   "jindaiko-1",
			wantSource: "",
			wantOK:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeSessionFiles(t, home, tc.files)

			r := NewCCSessionNameReader()
			gotName, gotSource, gotOK := r.LookupName(tc.lookupID)

			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (name=%q, source=%q)", gotOK, tc.wantOK, gotName, gotSource)
			}
			if gotName != tc.wantName {
				t.Errorf("name = %q, want %q", gotName, tc.wantName)
			}
			if gotSource != tc.wantSource {
				t.Errorf("nameSource = %q, want %q", gotSource, tc.wantSource)
			}
		})
	}
}

func TestCCSessionNameReader_LookupName_NoSessionsDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	r := NewCCSessionNameReader()
	if _, _, ok := r.LookupName("uuid-a"); ok {
		t.Error("expected miss when the sessions directory does not exist")
	}
}

func TestCCSessionNameReader_LookupName_EmptySessionsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	r := NewCCSessionNameReader()
	if _, _, ok := r.LookupName("uuid-a"); ok {
		t.Error("expected miss when the sessions directory is empty")
	}
}

func TestCCSessionNameReader_LookupName_EmptySessionID(t *testing.T) {
	r := NewCCSessionNameReader()
	if _, _, ok := r.LookupName(""); ok {
		t.Error("expected miss for an empty ccSessionID argument")
	}
}
