// Command extension-worker is a public-SDK-only CLI entry acceptance fixture.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

type event struct {
	Kind       string          `json:"kind"`
	PID        int             `json:"pid"`
	Executable string          `json:"executable,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
}

func record(kind string, request json.RawMessage) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(os.Getenv("ENTRY_EVENTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(file).Encode(event{Kind: kind, PID: os.Getpid(), Executable: executable, Request: request})
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

type handshakeWriter struct{ output io.Writer }

func (w handshakeWriter) Write(frame []byte) (int, error) {
	var response api.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		return 0, err
	}
	var manifest api.Manifest
	if json.Unmarshal(response.Result, &manifest) == nil && manifest.ID == "entry.fixture" {
		if err := record("initialized", nil); err != nil {
			return 0, err
		}
	}
	return w.output.Write(frame)
}

func main() {
	if err := record("started", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	manifest := api.Manifest{ID: "entry.fixture", Version: "1.0.0", HostAPI: api.Version,
		Capabilities: []string{"commands", "providers"},
		Commands:     []api.Command{{Name: "extension-hello", Description: "Public SDK entry fixture"}},
		Providers:    []api.Provider{{Name: "entrysdk", Models: []api.Model{{Name: "entrymodel", ContextWindow: 128000, MaxOutputTokens: 1024, ToolCall: true}}}}}
	handler := api.HandlerFunc(func(_ context.Context, request api.Request) (json.RawMessage, error) {
		if err := record(request.Method, request.Params); err != nil {
			return nil, err
		}
		switch request.Method {
		case "command.run":
			var command api.CommandRequest
			if err := json.Unmarshal(request.Params, &command); err != nil {
				return nil, err
			}
			if command.Name != "extension-hello" || len(command.Args) != 2 || command.Args[0] != "entry" || command.Args[1] != "literal $HOME" {
				return nil, fmt.Errorf("unexpected command arguments")
			}
			return json.Marshal(api.CommandResult{Output: "PUBLIC_SDK_COMMAND_OK\n"})
		case "provider.generate":
			var model api.ModelRequest
			if err := json.Unmarshal(request.Params, &model); err != nil {
				return nil, err
			}
			if model.Provider != "entrysdk" || model.Model != "entrymodel" || len(model.Messages) == 0 || !strings.Contains(model.Messages[len(model.Messages)-1].Content, "ENTRY_PROVIDER_REQUEST") {
				return nil, fmt.Errorf("unexpected provider request")
			}
			for _, forbidden := range []string{"api_key", "base_url", "credential"} {
				if strings.Contains(string(request.Params), `"`+forbidden+`"`) {
					return nil, fmt.Errorf("host credential material reached SDK request")
				}
			}
			if os.Getenv("GO_CLI_PROVIDER_API_KEY") != "" || os.Getenv("GO_CLI_PROVIDER_BASE_URL") != "" {
				return nil, fmt.Errorf("host provider environment reached worker")
			}
			return json.Marshal(api.Message{Role: "assistant", Content: "PUBLIC_SDK_PROVIDER_OK", FinishReason: "stop", Usage: &api.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
		default:
			return nil, fmt.Errorf("unsupported method %q", request.Method)
		}
	})
	if err := api.Serve(context.Background(), os.Stdin, handshakeWriter{output: os.Stdout}, manifest, handler); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
