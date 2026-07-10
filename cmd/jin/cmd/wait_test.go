package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestRenderWaitResultJSON(t *testing.T) {
	t.Run("outputs session info on wait completion", func(t *testing.T) {
		info := &session.Info{
			ID:          "abc-123",
			Description: "my-session",
			Status:      session.StatusIdle,
			WorkDir:     "/home/user/project",
			CreatedAt:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		var buf bytes.Buffer
		if err := renderWaitResultJSON(&buf, info); err != nil {
			t.Fatalf("renderWaitResultJSON() error = %v", err)
		}
		var parsed session.Info
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed.Status != session.StatusIdle {
			t.Errorf("expected status %q, got %q", session.StatusIdle, parsed.Status)
		}
	})
}

func TestParseWaitTargets(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		until   string
		want    []session.Status
		wantErr bool
	}{
		{name: "default status only", status: "idle", until: "", want: []session.Status{session.StatusIdle}},
		{name: "until overrides status", status: "idle", until: "permission", want: []session.Status{session.StatusPermission}},
		{name: "until multiple values", status: "idle", until: "idle,permission", want: []session.Status{session.StatusIdle, session.StatusPermission}},
		{name: "until trims whitespace", status: "idle", until: " idle , permission ", want: []session.Status{session.StatusIdle, session.StatusPermission}},
		{name: "until dedups", status: "", until: "idle,idle,permission", want: []session.Status{session.StatusIdle, session.StatusPermission}},
		{name: "both empty errors", status: "", until: "", wantErr: true},
		{name: "comma only errors", status: "", until: ",,", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWaitTargets(tc.status, tc.until)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v vs %v)", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestFormatTargets(t *testing.T) {
	got := formatTargets([]session.Status{session.StatusIdle, session.StatusPermission})
	want := "idle|permission"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
