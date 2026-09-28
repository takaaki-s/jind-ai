package daemon

import (
	"encoding/json"
	"errors"

	"github.com/takaaki-s/jind-ai/internal/session"
)

type CheckReportRecordRequest struct {
	ID string `json:"id"`
	session.CheckReportSubmission
}

func (s *Server) handleCheckReportRecord(data json.RawMessage) Response {
	var req CheckReportRecordRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Error: "id is required"}
	}
	result, err := s.manager.RecordChecks(req.ID, req.CheckReportSubmission)
	if err != nil {
		return Response{Error: err.Error()}
	}
	body, _ := json.Marshal(result)
	return Response{Success: true, Data: body}
}

func (c *Client) RecordChecks(id string, submission session.CheckReportSubmission) (*session.CheckReportRecordResult, error) {
	data, _ := json.Marshal(CheckReportRecordRequest{ID: id, CheckReportSubmission: submission})
	resp, err := c.send(Request{Action: "check-report-record", Data: data})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}
	var result session.CheckReportRecordResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
