// Command external-extension demonstrates a worker importing only the public SDK.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

func main() {
	manifest := api.Manifest{ID: "example.echo", Version: "1.0.0", HostAPI: api.Version, Capabilities: []string{"tools", "commands", "prompts"}, Tools: []api.Tool{{Name: "extension_echo", Description: "Echo a supplied message", Parameters: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`)}}, Commands: []api.Command{{Name: "extension-hello", Description: "Print a worker greeting"}}, Prompts: []api.PromptContribution{{ID: "example.echo.instructions", Text: "Use extension_echo when the user asks to echo a message."}}}
	handler := api.HandlerFunc(func(_ context.Context, request api.Request) (json.RawMessage, error) {
		switch request.Method {
		case "tool.call":
			var call api.ToolCall
			if err := json.Unmarshal(request.Params, &call); err != nil {
				return nil, err
			}
			return json.Marshal(api.ToolResult{Output: fmt.Sprint(call.Args["message"])})
		case "command.run":
			return json.Marshal(api.CommandResult{Output: "hello from external SDK\n"})
		default:
			return nil, fmt.Errorf("unsupported method %q", request.Method)
		}
	})
	if err := api.Serve(context.Background(), os.Stdin, os.Stdout, manifest, handler); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
