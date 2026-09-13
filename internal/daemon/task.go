package daemon

import (
	"encoding/json"
	"fmt"

	"github.com/takaaki-s/jind-ai/internal/task"
)

type TaskCreateRequest struct {
	Title         string      `json:"title"`
	Source        task.Source `json:"source"`
	RequestedBase string      `json:"requested_base,omitempty"`
	PromptSummary string      `json:"prompt_summary,omitempty"`
}

type TaskExecutionAddRequest struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
}

func (s *Server) handleTaskCreate(data json.RawMessage) Response {
	var req TaskCreateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	info, err := s.taskManager.Create(task.CreateOptions{
		Title: req.Title, Source: req.Source, RequestedBase: req.RequestedBase,
		PromptSummary: req.PromptSummary,
	})
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	payload, _ := json.Marshal(info)
	return Response{Success: true, Data: payload}
}

func (s *Server) handleTaskList() Response {
	payload, _ := json.Marshal(s.taskManager.List())
	return Response{Success: true, Data: payload}
}

func (s *Server) handleTaskGet(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	info, ok := s.taskManager.Get(req.ID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("task not found: %s", req.ID)}
	}
	payload, _ := json.Marshal(info)
	return Response{Success: true, Data: payload}
}

func (s *Server) handleTaskExecutionAdd(data json.RawMessage) Response {
	var req TaskExecutionAddRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.TaskID == "" || req.SessionID == "" {
		return Response{Success: false, Error: "task_id and session_id are required"}
	}
	info, err := s.taskManager.AppendExecution(req.TaskID, req.SessionID)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	payload, _ := json.Marshal(info)
	return Response{Success: true, Data: payload}
}
