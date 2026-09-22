package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func Serve(in io.Reader, out io.Writer, backend Backend) error {
	if backend == nil {
		return fmt.Errorf("remote backend is required")
	}
	negotiated := false
	controllerID := ""
	for {
		var req Request
		if err := ReadFrame(in, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		resp := dispatch(req, backend, negotiated, controllerID)
		if err := WriteFrame(out, resp); err != nil {
			return err
		}
		if resp.Status == "ok" && req.Operation == OperationHandshake {
			negotiated = true
			controllerID = req.ControllerID
		}
	}
}

func dispatch(req Request, backend Backend, negotiated bool, controllerID string) Response {
	if req.Protocol != ProtocolName {
		return ErrorResponse(req, NewWireError("invalid_request", "unknown remote protocol", false))
	}
	if req.ProtocolVersion != ProtocolVersion {
		return ErrorResponse(req, NewWireError("unsupported_version", "remote protocol version mismatch", false))
	}
	if ValidateIdentifier("request id", req.RequestID) != nil || ValidateIdentifier("controller id", req.ControllerID) != nil {
		return ErrorResponse(req, NewWireError("invalid_request", "invalid request identity", false))
	}
	if req.Operation != OperationHandshake && (!negotiated || req.ControllerID != controllerID) {
		return ErrorResponse(req, NewWireError("identity_mismatch", "handshake is required before this operation", false))
	}

	switch req.Operation {
	case OperationHandshake:
		if negotiated {
			return ErrorResponse(req, NewWireError("invalid_request", "handshake was already completed", false))
		}
		var payload HandshakeRequest
		if err := decodePayload(req.Payload, &payload); err != nil {
			return ErrorResponse(req, NewWireError("invalid_request", err.Error(), false))
		}
		result, wireErr := backend.Handshake(payload)
		if wireErr == nil {
			wireErr = ValidateHandshake(payload, result)
		}
		if wireErr != nil {
			return ErrorResponse(req, wireErr)
		}
		return successOrInternal(req, result)
	case OperationRepositoryPreflight:
		var payload PreflightRequest
		if err := decodePayload(req.Payload, &payload); err != nil {
			return ErrorResponse(req, NewWireError("invalid_request", err.Error(), false))
		}
		if ValidateIdentifier("repository id", payload.RepositoryID) != nil {
			return ErrorResponse(req, NewWireError("invalid_request", "invalid repository id", false))
		}
		result, wireErr := backend.Preflight(req.ControllerID, payload)
		if wireErr != nil {
			return ErrorResponse(req, wireErr)
		}
		return successOrInternal(req, result)
	default:
		return ErrorResponse(req, NewWireError("unsupported_operation", "remote operation is not supported", false))
	}
}

func decodePayload(data []byte, value any) error {
	if len(data) == 0 {
		return fmt.Errorf("payload is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("payload contains trailing JSON")
	}
	return nil
}

func successOrInternal(req Request, payload any) Response {
	resp, err := SuccessResponse(req, payload)
	if err != nil {
		return ErrorResponse(req, NewWireError("internal", "encode remote response", false))
	}
	return resp
}
