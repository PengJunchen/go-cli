package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// OutcomeUnknown marks a tool-call error where the request may have been
// dispatched but no trusted response established the outcome.
type OutcomeUnknown interface {
	error
	// OutcomeUnknown reports whether the operation may have completed without
	// a trusted response.
	OutcomeUnknown() bool
}

// requestBoundaryError records whether a request may have reached the remote
// server. retryable is only used by safe, read-only SDK methods that may
// reconnect and repeat their request.
type requestBoundaryError struct {
	cause            error
	dispatchPossible bool
	retryable        bool
}

func (e *requestBoundaryError) Error() string { return e.cause.Error() }

func (e *requestBoundaryError) Unwrap() error { return e.cause }

func newRequestBoundaryError(err error, dispatchPossible, retryable bool) error {
	if err == nil {
		return nil
	}
	return &requestBoundaryError{
		cause:            err,
		dispatchPossible: dispatchPossible,
		retryable:        retryable,
	}
}

func requestMayHaveBeenDispatched(err error) bool {
	var boundaryErr *requestBoundaryError
	return errors.As(err, &boundaryErr) && boundaryErr.dispatchPossible
}

// rpcResponseError represents a well-formed JSON-RPC error tied to the
// current request ID. Unlike a transport or envelope failure, it establishes
// that the peer returned a definite RPC-level failure.
type rpcResponseError struct {
	code    int64
	message string
}

func (e *rpcResponseError) Error() string {
	return fmt.Sprintf("mcp: rpc error: code %d: %s", e.code, e.message)
}

type unknownToolOutcomeError struct {
	tool  string
	cause error
}

func (e *unknownToolOutcomeError) Error() string {
	return fmt.Sprintf("outcome unknown; tool %q may have executed but no trusted response was received: %v", e.tool, e.cause)
}

func (e *unknownToolOutcomeError) Unwrap() error { return e.cause }

func (*unknownToolOutcomeError) OutcomeUnknown() bool { return true }

func parseIntegerJSON(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

// parseJSONRPCResponse validates a response envelope for the request ID that
// was sent. Responses that cannot be trusted as the result of that request
// are marked as possibly dispatched, with no retry recommendation.
func parseJSONRPCResponse(raw []byte, expectedID int64) (map[string]any, error) {
	if err := validateJSONRPCObjectFields(raw, "response envelope", "jsonrpc", "id", "result", "error"); err != nil {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: invalid JSON-RPC response envelope: %w", err), true, false)
	}

	var frame struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: decode response: %w", err), true, false)
	}
	if frame.JSONRPC != "2.0" {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: invalid JSON-RPC version %q", frame.JSONRPC), true, false)
	}
	responseID, validID := parseIntegerJSON(frame.ID)
	if !validID || responseID != expectedID {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: response ID does not match request ID %d", expectedID), true, false)
	}

	resultPresent := len(frame.Result) > 0 && !bytes.Equal(bytes.TrimSpace(frame.Result), []byte("null"))
	errorPresent := len(frame.Error) > 0 && !bytes.Equal(bytes.TrimSpace(frame.Error), []byte("null"))
	if resultPresent == errorPresent {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: response must contain exactly one of result or error"), true, false)
	}
	if errorPresent {
		if err := validateJSONRPCObjectFields(frame.Error, "JSON-RPC error object", "code", "message"); err != nil {
			return nil, newRequestBoundaryError(fmt.Errorf("mcp: invalid JSON-RPC error object: %w", err), true, false)
		}
		var errorObject map[string]json.RawMessage
		if err := json.Unmarshal(frame.Error, &errorObject); err != nil || errorObject == nil {
			return nil, newRequestBoundaryError(fmt.Errorf("mcp: invalid JSON-RPC error object"), true, false)
		}
		code, validCode := parseIntegerJSON(errorObject["code"])
		var message string
		messageRaw := bytes.TrimSpace(errorObject["message"])
		if !validCode || len(messageRaw) == 0 || messageRaw[0] != '"' || json.Unmarshal(messageRaw, &message) != nil {
			return nil, newRequestBoundaryError(fmt.Errorf("mcp: invalid JSON-RPC error object"), true, false)
		}
		return nil, &rpcResponseError{code: code, message: message}
	}

	var result map[string]any
	if err := json.Unmarshal(frame.Result, &result); err != nil || result == nil {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: invalid JSON-RPC result object"), true, false)
	}
	return result, nil
}

func validateJSONRPCObjectFields(raw []byte, objectName string, protocolFields ...string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := start.(json.Delim)
	if !ok || delim != '{' {
		return fmt.Errorf("%s must be an object", objectName)
	}

	reserved := make(map[string]struct{}, len(protocolFields))
	for _, field := range protocolFields {
		reserved[field] = struct{}{}
	}
	seen := make(map[string]struct{}, len(protocolFields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("%s contains a non-string field name", objectName)
		}
		for field := range reserved {
			if !strings.EqualFold(key, field) {
				continue
			}
			if key != field {
				return fmt.Errorf("%s uses non-canonical spelling %q for protocol field %q", objectName, key, field)
			}
			if _, exists := seen[field]; exists {
				return fmt.Errorf("duplicate protocol field %q in %s", field, objectName)
			}
			seen[field] = struct{}{}
			break
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}

	end, err := decoder.Token()
	if err != nil {
		return err
	}
	endDelim, ok := end.(json.Delim)
	if !ok || endDelim != '}' {
		return fmt.Errorf("%s object is not terminated", objectName)
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s has trailing JSON value", objectName)
		}
		return err
	}
	return nil
}
