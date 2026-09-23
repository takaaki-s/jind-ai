package remotecontract

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/remote"
)

type envelope struct {
	Protocol        string          `json:"protocol"`
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	ControllerID    string          `json:"controller_id,omitempty"`
	Operation       string          `json:"operation"`
	Status          string          `json:"status,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Error           json.RawMessage `json:"error,omitempty"`
}

type startPayload struct {
	ControllerExecutionID      string `json:"controller_execution_id"`
	IdempotencyKey             string `json:"idempotency_key"`
	RepositoryID               string `json:"repository_id"`
	ExpectedRepositoryIdentity string `json:"expected_repository_identity"`
	Title                      string `json:"title"`
	Source                     struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"source"`
	RequestedBase   string `json:"requested_base"`
	RelativeWorkDir string `json:"relative_work_dir"`
	AgentKind       string `json:"agent_kind"`
	Model           string `json:"model"`
	Fleet           string `json:"fleet"`
	NoHook          bool   `json:"no_hook"`
	Prompt          struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
		Body   string `json:"body"`
	} `json:"prompt"`
}

func TestCanonicalFixtures(t *testing.T) {
	fixtures := map[string]struct {
		operation  string
		request    bool
		controller bool
	}{
		"handshake-request.json":  {operation: "handshake", request: true, controller: true},
		"handshake-response.json": {operation: "handshake"},
		"preflight-request.json":  {operation: "repository.preflight", request: true, controller: true},
		"preflight-response.json": {operation: "repository.preflight"},
		"start-request.json":      {operation: "execution.start", request: true, controller: true},
		"start-response.json":     {operation: "execution.start"},
		"inspect-request.json":    {operation: "execution.inspect", request: true, controller: true},
		"inspect-response.json":   {operation: "execution.inspect"},
		"cancel-request.json":     {operation: "execution.cancel", request: true, controller: true},
		"cancel-response.json":    {operation: "execution.cancel"},
		"cleanup-request.json":    {operation: "execution.cleanup", request: true, controller: true},
		"cleanup-response.json":   {operation: "execution.cleanup"},
	}

	for name, want := range fixtures {
		t.Run(name, func(t *testing.T) {
			data := readFixture(t, name)
			var got envelope
			decodeStrict(t, data, &got)
			if got.Protocol != remote.ProtocolName || got.ProtocolVersion != remote.ProtocolVersion {
				t.Fatalf("protocol = %q v%d", got.Protocol, got.ProtocolVersion)
			}
			if got.RequestID == "" || got.Operation != want.operation || len(got.Payload) == 0 {
				t.Fatalf("invalid envelope: %+v", got)
			}
			if want.controller != (got.ControllerID != "") {
				t.Fatalf("controller presence = %t, want %t", got.ControllerID != "", want.controller)
			}
			if want.request {
				if got.Status != "" || len(got.Error) != 0 {
					t.Fatalf("request contains response fields: %+v", got)
				}
			} else if got.Status != "ok" || len(got.Error) != 0 {
				t.Fatalf("response status/error = %q/%s", got.Status, got.Error)
			}
		})
	}
}

func TestStartFixtureBindsPromptEvidence(t *testing.T) {
	var env envelope
	decodeStrict(t, readFixture(t, "start-request.json"), &env)
	var payload startPayload
	decodeStrict(t, env.Payload, &payload)
	if payload.ControllerExecutionID == "" || payload.IdempotencyKey == "" || payload.RepositoryID == "" || payload.ExpectedRepositoryIdentity == "" {
		t.Fatalf("start identity is incomplete: %+v", payload)
	}
	sum := sha256.Sum256([]byte(payload.Prompt.Body))
	if payload.Prompt.Bytes != len([]byte(payload.Prompt.Body)) || payload.Prompt.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("prompt evidence does not match live body: %+v", payload.Prompt)
	}
}

func TestStartAndInspectFixturesMatchProductionTypes(t *testing.T) {
	var startEnvelope envelope
	decodeStrict(t, readFixture(t, "start-request.json"), &startEnvelope)
	var startRequest remote.StartRequest
	decodeStrict(t, startEnvelope.Payload, &startRequest)
	if wireErr := remote.ValidateStartRequest(startRequest, 64*1024); wireErr != nil {
		t.Fatalf("start request: %+v", wireErr)
	}
	var startResponseEnvelope envelope
	decodeStrict(t, readFixture(t, "start-response.json"), &startResponseEnvelope)
	var startResponse remote.StartResponse
	decodeStrict(t, startResponseEnvelope.Payload, &startResponse)
	if wireErr := remote.ValidateStartResponse(startRequest, startResponse); wireErr != nil {
		t.Fatalf("start response: %+v", wireErr)
	}

	var inspectEnvelope envelope
	decodeStrict(t, readFixture(t, "inspect-request.json"), &inspectEnvelope)
	var inspectRequest remote.InspectRequest
	decodeStrict(t, inspectEnvelope.Payload, &inspectRequest)
	if wireErr := remote.ValidateInspectRequest(inspectRequest); wireErr != nil {
		t.Fatalf("inspect request: %+v", wireErr)
	}
	var inspectResponseEnvelope envelope
	decodeStrict(t, readFixture(t, "inspect-response.json"), &inspectResponseEnvelope)
	var inspectResponse remote.InspectResponse
	decodeStrict(t, inspectResponseEnvelope.Payload, &inspectResponse)
	if wireErr := remote.ValidateInspectResponse(inspectRequest, inspectResponse); wireErr != nil {
		t.Fatalf("inspect response: %+v", wireErr)
	}
}

func TestFixturesDoNotCrossForbiddenAuthorityBoundaries(t *testing.T) {
	for _, name := range []string{
		"handshake-request.json", "handshake-response.json", "preflight-request.json", "preflight-response.json",
		"start-request.json",
		"start-response.json", "inspect-request.json", "inspect-response.json",
		"cancel-request.json", "cancel-response.json", "cleanup-request.json", "cleanup-response.json",
	} {
		t.Run(name, func(t *testing.T) {
			var value any
			decodeStrict(t, readFixture(t, name), &value)
			walkKeys(t, value, func(key string) {
				switch strings.ToLower(key) {
				case "repo", "repository_path", "worktree_path", "socket", "pane", "screen", "transcript", "environment", "credentials":
					t.Fatalf("fixture crosses forbidden boundary with key %q", key)
				}
			})
		})
	}
}

func TestLengthPrefixedFixtureRoundTrip(t *testing.T) {
	want := readFixture(t, "inspect-response.json")
	var framed bytes.Buffer
	if err := remote.WriteFrame(&framed, json.RawMessage(want)); err != nil {
		t.Fatal(err)
	}
	var got json.RawMessage
	if err := remote.ReadFrame(bufio.NewReader(&framed), &got); err != nil {
		t.Fatal(err)
	}
	var compactWant bytes.Buffer
	if err := json.Compact(&compactWant, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, compactWant.Bytes()) {
		t.Fatal("framed payload changed")
	}
}

func TestLengthPrefixRejectsOversizeBeforeReadingBody(t *testing.T) {
	var framed bytes.Buffer
	if err := binary.Write(&framed, binary.BigEndian, uint32(remote.MaxFrameBytes+1)); err != nil {
		t.Fatal(err)
	}
	var got json.RawMessage
	err := remote.ReadFrame(&framed, &got)
	if !errors.Is(err, remote.ErrFrameTooLarge) || got != nil {
		t.Fatalf("ReadFrame = %q, %v", got, err)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeStrict(t *testing.T, data []byte, dst any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("fixture contains trailing JSON: %v", err)
	}
}

func walkKeys(t *testing.T, value any, visit func(string)) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			visit(key)
			walkKeys(t, child, visit)
		}
	case []any:
		for _, child := range value {
			walkKeys(t, child, visit)
		}
	case nil, bool, float64, string:
	default:
		t.Fatalf("unexpected JSON value %T (%s)", value, fmt.Sprint(value))
	}
}
