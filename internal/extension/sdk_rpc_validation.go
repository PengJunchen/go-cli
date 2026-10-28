package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

// rpcObject rejects null, duplicate keys and trailing values before the request
// is removed from the pending map. A decoded zero-value struct is not evidence
// that a worker actually returned the protocol's declared result.
func rpcObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("extension: RPC value must be an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, errors.New("extension: invalid RPC object key")
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("extension: invalid RPC object key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("extension: duplicate RPC object field")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, errors.New("extension: invalid RPC object field")
		}
		fields[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errors.New("extension: invalid RPC object end")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("extension: trailing RPC value")
	}
	return fields, nil
}

func decodeRPCResponse(data []byte) (api.Response, error) {
	fields, err := rpcObject(data)
	if err != nil {
		return api.Response{}, err
	}
	for name := range fields {
		if name != "id" && name != "result" && name != "error" {
			return api.Response{}, errors.New("extension: unknown RPC response field")
		}
	}
	var response api.Response
	if err := json.Unmarshal(fields["id"], &response.ID); err != nil || response.ID == 0 {
		return response, errors.New("extension: invalid RPC response id")
	}
	result, hasResult := fields["result"]
	diagnostic, hasError := fields["error"]
	if hasResult == hasError {
		return response, errors.New("extension: RPC response requires exactly one result or error")
	}
	if hasResult {
		if _, err := rpcObject(result); err != nil {
			return response, err
		}
		response.Result = append(json.RawMessage(nil), result...)
		return response, nil
	}
	errorFields, err := rpcObject(diagnostic)
	if err != nil {
		return response, err
	}
	for name := range errorFields {
		if name != "code" && name != "message" && name != "source" {
			return response, errors.New("extension: unknown RPC diagnostic field")
		}
	}
	var code string
	if err := json.Unmarshal(errorFields["code"], &code); err != nil {
		return response, errors.New("extension: invalid RPC diagnostic code")
	}
	switch code {
	case "callback_failed", "callback_panicked", "outcome_unknown":
	default:
		return response, errors.New("extension: unsupported RPC diagnostic code")
	}
	for _, name := range []string{"message", "source"} {
		if value, present := errorFields[name]; present {
			var text string
			if err := json.Unmarshal(value, &text); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return response, errors.New("extension: invalid RPC diagnostic text")
			}
		}
	}
	response.Error = &api.Diagnostic{Code: code} // never retain private worker diagnostic text
	return response, nil
}

func validateRPCToolResult(data json.RawMessage) error {
	fields, err := rpcObject(data)
	if err != nil {
		return err
	}
	for name := range fields {
		if name != "output" && name != "metadata" && name != "is_error" {
			return errors.New("extension: unknown RPC tool result field")
		}
	}
	var output string
	if err := json.Unmarshal(fields["output"], &output); err != nil || bytes.Equal(bytes.TrimSpace(fields["output"]), []byte("null")) {
		return errors.New("extension: RPC tool result requires string output")
	}
	if raw, present := fields["is_error"]; present {
		var failed bool
		if err := json.Unmarshal(raw, &failed); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("extension: invalid RPC tool result status")
		}
	}
	if raw, present := fields["metadata"]; present {
		if _, err := rpcObject(raw); err != nil {
			return errors.New("extension: invalid RPC tool result metadata")
		}
	}
	return nil
}
