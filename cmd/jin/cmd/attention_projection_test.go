package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

// Every renderer that prints a session serializes session.Info, so the
// attention object must be identical in all of them. A projection that drifted
// between them would break scripts that read one and act on another.
//
// The five here are the whole population: `grep -n "func render.*JSON"
// cmd/jin/cmd/*.go` plus seen.go's direct writeJSON, filtered to those taking
// a *session.Info or []session.Info. The rest (kill/delete, send, respond,
// output, result, daemon) print shapes of their own and carry no attention.
func TestAttentionProjection_AgreesAcrossEverySessionRenderer(t *testing.T) {
	info := session.Info{
		ID:          "abcd1234",
		Description: "auth callback",
		Status:      session.StatusIdle,
		Attention: session.AttentionInfo{
			State:          session.AttentionDone,
			Generation:     4,
			SeenGeneration: 3,
			Unseen:         true,
		},
	}

	renderers := map[string]func() (string, error){
		"list": func() (string, error) {
			var buf bytes.Buffer
			err := renderSessionListJSON(&buf, []session.Info{info})
			return buf.String(), err
		},
		"info": func() (string, error) {
			var buf bytes.Buffer
			err := renderSessionInfoJSON(&buf, &info)
			return buf.String(), err
		},
		"new": func() (string, error) {
			var buf bytes.Buffer
			err := renderNewSessionJSON(&buf, &info)
			return buf.String(), err
		},
		"wait": func() (string, error) {
			var buf bytes.Buffer
			err := renderWaitResultJSON(&buf, &info)
			return buf.String(), err
		},
		"seen": func() (string, error) {
			var buf bytes.Buffer
			err := writeJSON(&buf, &info)
			return buf.String(), err
		},
	}

	want := map[string]any{
		"state":           "done",
		"generation":      float64(4),
		"seen_generation": float64(3),
		"unseen":          true,
	}

	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			out, err := render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			got := attentionObjectIn(t, out)
			if len(got) != len(want) {
				t.Errorf("attention keys = %v, want exactly %v", got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("attention[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

// attentionObjectIn digs the attention object out of whichever shape the
// renderer produced — a bare object for info/seen, a list for list.
func attentionObjectIn(t *testing.T, out string) map[string]any {
	t.Helper()

	var single struct {
		Attention map[string]any `json:"attention"`
	}
	if err := json.Unmarshal([]byte(out), &single); err == nil && single.Attention != nil {
		return single.Attention
	}

	var listed []struct {
		Attention map[string]any `json:"attention"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("output is neither an object nor a list: %v\noutput: %s", err, out)
	}
	if len(listed) != 1 {
		t.Fatalf("list output has %d entries, want 1", len(listed))
	}
	return listed[0].Attention
}
