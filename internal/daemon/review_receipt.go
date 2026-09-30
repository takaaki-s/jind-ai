package daemon

import (
	"encoding/json"
	"errors"

	"github.com/takaaki-s/jind-ai/internal/session"
)

type ReviewDispositionRecordRequest struct {
	ID string `json:"id"`
	session.ReviewDispositionSubmission
}

func (s *Server) handleReviewDispositionRecord(data json.RawMessage) Response {
	var req ReviewDispositionRecordRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Error: "id is required"}
	}
	result, err := s.manager.RecordReviewDisposition(req.ID, req.ReviewDispositionSubmission)
	if err != nil {
		return Response{Error: err.Error()}
	}
	body, _ := json.Marshal(result)
	return Response{Success: true, Data: body}
}

func (c *Client) RecordReviewDisposition(id string, submission session.ReviewDispositionSubmission) (*session.ReviewDispositionRecordResult, error) {
	data, _ := json.Marshal(ReviewDispositionRecordRequest{ID: id, ReviewDispositionSubmission: submission})
	resp, err := c.send(Request{Action: "review-disposition-record", Data: data})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}
	var result session.ReviewDispositionRecordResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
