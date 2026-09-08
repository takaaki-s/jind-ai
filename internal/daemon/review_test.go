package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestHandleReviewRefresh_ReturnsUnavailableFactsForOrdinarySession(t *testing.T) {
	s := newAsyncTestServer(t)
	info := reserveSession(t, s, "claude")
	data, _ := json.Marshal(IDRequest{ID: info.ID})

	resp := s.handleReviewRefresh(data)
	if !resp.Success {
		t.Fatalf("Success=false: %s", resp.Error)
	}
	var got session.Info
	if err := json.Unmarshal(resp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ReviewFacts.Status != session.ReviewFactsUnavailable ||
		got.ReviewFacts.UnavailableReason != session.ReviewBaseUnavailableNotManagedWorktree {
		t.Fatalf("ReviewFacts = %+v", got.ReviewFacts)
	}
}

func TestHandleReviewRefresh_ValidatesIDAndDispatches(t *testing.T) {
	s := newTestServer(t)
	bad := s.handleReviewRefresh(json.RawMessage("{}"))
	if bad.Success || !strings.Contains(bad.Error, "id is required") {
		t.Fatalf("empty-ID response = %+v", bad)
	}
	data, _ := json.Marshal(IDRequest{ID: "missing"})
	resp := s.handleRequest(&Request{Action: "review-refresh", Data: data})
	if strings.Contains(resp.Error, "unknown action") {
		t.Fatalf("review-refresh is not dispatched: %s", resp.Error)
	}
}

func TestReviewRefreshIsNotReadOnly(t *testing.T) {
	if readOnlyActions["review-refresh"] {
		t.Fatal("review-refresh persists a cache and must report timeouts as outcome unknown")
	}
}

func TestClientRefreshReview_SendsActionAndDecodesInfo(t *testing.T) {
	want := session.Info{
		ID: "abcd1234",
		ReviewFacts: session.ReviewFacts{
			Status: session.ReviewFactsAvailable, AttentionGeneration: 2, ChangedFiles: 3,
		},
	}
	data, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: data})

	got, err := NewClient(sock).RefreshReview(want.ID)
	if err != nil {
		t.Fatalf("RefreshReview: %v", err)
	}
	if received.Action != "review-refresh" {
		t.Fatalf("Action = %q", received.Action)
	}
	if got.ReviewFacts != want.ReviewFacts {
		t.Fatalf("ReviewFacts = %+v, want %+v", got.ReviewFacts, want.ReviewFacts)
	}
}
