package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAttention_Unseen(t *testing.T) {
	tests := []struct {
		name string
		a    Attention
		want bool
	}{
		{"zero", Attention{}, false},
		{"done and unacknowledged", Attention{State: AttentionDone, Generation: 1}, true},
		{"done and acknowledged", Attention{State: AttentionDone, Generation: 1, SeenGeneration: 1}, false},
		{"completion after seen", Attention{State: AttentionDone, Generation: 2, SeenGeneration: 1}, true},
		// A generation without the state is not a receipt. Nothing produces
		// this, and the derivation must not invent one from a stray counter.
		{"generation without state", Attention{Generation: 3}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Unseen(); got != tt.want {
				t.Errorf("Unseen() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The transition helpers are the whole of what advances the two counters, so
// the invariants are pinned on them rather than on each call site.
func TestAttention_CompletedAndAcknowledged(t *testing.T) {
	a := Attention{}

	a = a.completed()
	if a.State != AttentionDone || a.Generation != 1 || a.SeenGeneration != 0 {
		t.Fatalf("after first completion = %+v, want done/1/0", a)
	}
	if !a.Unseen() {
		t.Error("a first completion must be unseen")
	}

	a = a.acknowledged()
	if a.State != AttentionDone || a.Generation != 1 || a.SeenGeneration != 1 {
		t.Fatalf("after seen = %+v, want done/1/1", a)
	}
	if a.Unseen() {
		t.Error("an acknowledged completion must not be unseen")
	}

	// Idempotent: acknowledging twice changes nothing.
	if again := a.acknowledged(); again != a {
		t.Errorf("second seen = %+v, want %+v", again, a)
	}

	a = a.completed()
	if a.Generation != 2 || a.SeenGeneration != 1 {
		t.Fatalf("after completion-since-seen = %+v, want generation 2 seen 1", a)
	}
	if !a.Unseen() {
		t.Error("a completion after seen must be unseen again")
	}
}

// Acknowledging a session that never completed must not manufacture a receipt
// — otherwise `jin session seen` on a fresh session would write a done state.
func TestAttention_AcknowledgedOnZeroStaysZero(t *testing.T) {
	if got := (Attention{}).acknowledged(); got != (Attention{}) {
		t.Errorf("acknowledged() on zero = %+v, want the zero value", got)
	}
}

func TestMergeAttention(t *testing.T) {
	tests := []struct {
		name string
		a, b Attention
		want Attention
	}{
		{"both zero", Attention{}, Attention{}, Attention{}},
		{
			"a stale snapshot cannot undo a completion",
			Attention{},
			Attention{State: AttentionDone, Generation: 1},
			Attention{State: AttentionDone, Generation: 1},
		},
		{
			"a stale snapshot cannot resurrect an acknowledged receipt",
			Attention{State: AttentionDone, Generation: 1},
			Attention{State: AttentionDone, Generation: 1, SeenGeneration: 1},
			Attention{State: AttentionDone, Generation: 1, SeenGeneration: 1},
		},
		{
			"a completion and a seen that crossed keep both halves",
			Attention{State: AttentionDone, Generation: 2, SeenGeneration: 0},
			Attention{State: AttentionDone, Generation: 1, SeenGeneration: 1},
			Attention{State: AttentionDone, Generation: 2, SeenGeneration: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeAttention(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("mergeAttention(a, b) = %+v, want %+v", got, tt.want)
			}
			// Commutativity is what lets Save apply the merge without knowing
			// which of the two snapshots is the newer one.
			if rev := mergeAttention(tt.b, tt.a); rev != tt.want {
				t.Errorf("mergeAttention(b, a) = %+v, want %+v", rev, tt.want)
			}
			// Idempotence is what makes a retried Save harmless.
			if again := mergeAttention(got, got); again != tt.want {
				t.Errorf("mergeAttention(got, got) = %+v, want %+v", again, tt.want)
			}
		})
	}
}

// SeenGeneration <= Generation must survive a merge of two snapshots that each
// satisfy it separately — component-wise max is only safe because max is
// monotone, and that is the property under test.
func TestMergeAttention_KeepsSeenBelowGeneration(t *testing.T) {
	got := mergeAttention(
		Attention{State: AttentionDone, Generation: 5, SeenGeneration: 5},
		Attention{State: AttentionDone, Generation: 9, SeenGeneration: 2},
	)
	if got.SeenGeneration > got.Generation {
		t.Errorf("merged %+v violates seen <= generation", got)
	}
}

func TestAttention_ToInfo(t *testing.T) {
	got := Attention{State: AttentionDone, Generation: 4, SeenGeneration: 3}.toInfo()
	want := AttentionInfo{State: AttentionDone, Generation: 4, SeenGeneration: 3, Unseen: true}
	if got != want {
		t.Errorf("toInfo() = %+v, want %+v", got, want)
	}
}

// Zero attention writes no key at all, on either struct. That is what lets a
// pre-attention session file stay valid without a migration, and what lets a
// consumer read a missing object as none/seen.
func TestAttention_ZeroOmittedFromJSON(t *testing.T) {
	sessData, err := json.Marshal(Session{ID: "s1"})
	if err != nil {
		t.Fatalf("marshal Session: %v", err)
	}
	if strings.Contains(string(sessData), "attention") {
		t.Errorf("zero Session JSON mentions attention: %s", sessData)
	}

	infoData, err := json.Marshal(Info{ID: "s1"})
	if err != nil {
		t.Fatalf("marshal Info: %v", err)
	}
	if strings.Contains(string(infoData), "attention") {
		t.Errorf("zero Info JSON mentions attention: %s", infoData)
	}
}

// Once the object exists every field is spelled out, so a script can read
// .attention.unseen without first testing for the key.
func TestAttentionInfo_JSONShape(t *testing.T) {
	sess := Session{ID: "s1", Attention: Attention{State: AttentionDone, Generation: 4, SeenGeneration: 3}}
	data, err := json.Marshal(sess.ToInfo())
	if err != nil {
		t.Fatalf("marshal Info: %v", err)
	}

	var decoded struct {
		Attention map[string]any `json:"attention"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"state":           "done",
		"generation":      float64(4),
		"seen_generation": float64(3),
		"unseen":          true,
	}
	if len(decoded.Attention) != len(want) {
		t.Errorf("attention keys = %v, want exactly %v", decoded.Attention, want)
	}
	for k, v := range want {
		if decoded.Attention[k] != v {
			t.Errorf("attention[%q] = %v, want %v", k, decoded.Attention[k], v)
		}
	}
}

// A seen receipt still emits the object — with unseen false — so a consumer
// can tell "acknowledged" apart from "never completed".
func TestAttentionInfo_SeenStillEmitsTheObject(t *testing.T) {
	sess := Session{ID: "s1", Attention: Attention{State: AttentionDone, Generation: 2, SeenGeneration: 2}}
	data, err := json.Marshal(sess.ToInfo())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"unseen": false`) && !strings.Contains(string(data), `"unseen":false`) {
		t.Errorf("Info JSON = %s, want an attention object with unseen false", data)
	}
}

// unseen is derived, so it must never reach a session file — a persisted copy
// would be a second source of truth that a stale merge could contradict.
func TestAttention_UnseenNeverPersisted(t *testing.T) {
	sess := Session{ID: "s1", Attention: Attention{State: AttentionDone, Generation: 1}}
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "unseen") {
		t.Errorf("Session JSON mentions unseen: %s", data)
	}
}

func TestAttention_PersistedRoundTrip(t *testing.T) {
	want := Attention{State: AttentionDone, Generation: 7, SeenGeneration: 6}
	data, err := json.Marshal(Session{ID: "s1", Attention: want})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Session
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Attention != want {
		t.Errorf("round trip = %+v, want %+v", back.Attention, want)
	}
}
