package daemon

import (
	"encoding/json"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
)

func TestTaskHandlers_CreateListAndGet(t *testing.T) {
	s := newTestServer(t)
	createData, _ := json.Marshal(TaskCreateRequest{
		Title: "Preserve intent", Source: task.Source{Kind: "issue", Ref: "ENG-12"},
		RequestedBase: "origin/main", PromptSummary: "Bounded metadata",
	})
	createdResp := s.handleRequest(&Request{Action: "task-create", Data: createData})
	if !createdResp.Success {
		t.Fatalf("create: %s", createdResp.Error)
	}
	var created task.Info
	if err := json.Unmarshal(createdResp.Data, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Source.Ref != "ENG-12" {
		t.Fatalf("created = %+v", created)
	}

	listResp := s.handleRequest(&Request{Action: "task-list"})
	if !listResp.Success {
		t.Fatalf("list: %s", listResp.Error)
	}
	var listed []task.Info
	if err := json.Unmarshal(listResp.Data, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("list = %+v", listed)
	}

	getData, _ := json.Marshal(IDRequest{ID: created.ID})
	getResp := s.handleRequest(&Request{Action: "task-get", Data: getData})
	if !getResp.Success {
		t.Fatalf("get: %s", getResp.Error)
	}
	var got task.Info
	if err := json.Unmarshal(getResp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.PromptSummary != "Bounded metadata" {
		t.Fatalf("got = %+v", got)
	}
}

func TestTaskHandlers_RejectInvalidRequests(t *testing.T) {
	s := newTestServer(t)
	if resp := s.handleTaskCreate(json.RawMessage(`{"title":""}`)); resp.Success {
		t.Fatal("accepted empty task title")
	}
	if resp := s.handleTaskGet(json.RawMessage(`{"id":"missing"}`)); resp.Success {
		t.Fatal("returned missing task")
	}
	if resp := s.handleTaskExecutionAdd(json.RawMessage(`{"task_id":"x"}`)); resp.Success {
		t.Fatal("accepted missing session id")
	}
}

func TestClientTaskMethods_SendExpectedActions(t *testing.T) {
	want := task.Info{SchemaVersion: task.SchemaVersion, ID: "task-1", Title: "Intent", Source: task.Source{Kind: "manual"}, Executions: []task.ExecutionInfo{}}
	payload, _ := json.Marshal(want)
	t.Run("new", func(t *testing.T) {
		wantResult := TaskNewResponse{
			Task:      want,
			Execution: task.ExecutionInfo{Execution: task.Execution{ID: "execution-1", SessionID: "session-1"}},
			Session:   session.Info{ID: "session-1", Status: session.StatusCreating},
		}
		resultPayload, _ := json.Marshal(wantResult)
		sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: resultPayload})
		got, err := NewClient(sock).NewTask(TaskNewRequest{IdempotencyKey: "request-1", Prompt: "do it", Repo: "/repo"})
		if err != nil {
			t.Fatal(err)
		}
		if received.Action != "task-new" || got.Execution.ID != "execution-1" {
			t.Fatalf("action=%s got=%+v", received.Action, got)
		}
		var req TaskNewRequest
		if err := json.Unmarshal(received.Data, &req); err != nil {
			t.Fatal(err)
		}
		if req.IdempotencyKey != "request-1" || req.Prompt != "do it" {
			t.Fatalf("request=%+v", req)
		}
	})

	t.Run("create", func(t *testing.T) {
		sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: payload})
		got, err := NewClient(sock).CreateTask(TaskCreateRequest{Title: "Intent"})
		if err != nil {
			t.Fatal(err)
		}
		if received.Action != "task-create" || got.ID != want.ID {
			t.Fatalf("action=%s got=%+v", received.Action, got)
		}
	})
	t.Run("list", func(t *testing.T) {
		listPayload, _ := json.Marshal([]task.Info{want})
		sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: listPayload})
		got, err := NewClient(sock).ListTasks()
		if err != nil {
			t.Fatal(err)
		}
		if received.Action != "task-list" || len(got) != 1 {
			t.Fatalf("action=%s got=%+v", received.Action, got)
		}
	})
	t.Run("get", func(t *testing.T) {
		sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: payload})
		if _, err := NewClient(sock).GetTask("task-1"); err != nil {
			t.Fatal(err)
		}
		if received.Action != "task-get" {
			t.Fatalf("action=%s", received.Action)
		}
	})
	t.Run("append", func(t *testing.T) {
		sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: payload})
		if _, err := NewClient(sock).AddTaskExecution("task-1", "session-1"); err != nil {
			t.Fatal(err)
		}
		if received.Action != "task-execution-add" {
			t.Fatalf("action=%s", received.Action)
		}
		var req TaskExecutionAddRequest
		if err := json.Unmarshal(received.Data, &req); err != nil {
			t.Fatal(err)
		}
		if req.TaskID != "task-1" || req.SessionID != "session-1" {
			t.Fatalf("request=%+v", req)
		}
	})
}

func TestTaskReadActionsAreClassified(t *testing.T) {
	if !readOnlyActions["task-list"] || !readOnlyActions["task-get"] {
		t.Fatal("task reads must be classified read-only")
	}
	if readOnlyActions["task-create"] || readOnlyActions["task-new"] || readOnlyActions["task-execution-add"] {
		t.Fatal("task writes must not be classified read-only")
	}
}
