package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

type sdkCaller interface {
	call(context.Context, string, any, any) error
	close(context.Context) error
}

type sdkExtension struct {
	manifest api.Manifest
	caller   sdkCaller
}

var _ Extension = (*sdkExtension)(nil)

func (e *sdkExtension) Name() string { return e.manifest.ID }

// Manifest returns a detached declaration for pre-initialization graph validation.
func (e *sdkExtension) Manifest() api.Manifest             { return e.manifest.Clone() }
func (e *sdkExtension) Shutdown(ctx context.Context) error { return e.caller.close(ctx) }
func (e *sdkExtension) Init(ctx context.Context, registry ExtensionRegistry) error {
	if err := e.manifest.Validate(); err != nil {
		return err
	}
	for _, declaration := range e.manifest.Tools {
		if err := registry.RegisterTool(ctx, &sdkTool{declaration: declaration, caller: e.caller}); err != nil {
			return err
		}
	}
	for _, declaration := range e.manifest.Hooks {
		if err := registry.RegisterHook(ctx, &sdkHook{declaration: declaration, caller: e.caller}); err != nil {
			return err
		}
	}
	for _, declaration := range e.manifest.Commands {
		declaration := declaration
		if err := registry.RegisterCommand(declaration.Name, func(args []string) error {
			var result api.CommandResult
			if err := e.caller.call(context.Background(), "command.run", api.CommandRequest{Name: declaration.Name, Args: append([]string(nil), args...)}, &result); err != nil {
				return err
			}
			if result.Output != "" {
				if _, err := fmt.Fprint(os.Stdout, result.Output); err != nil {
					return fmt.Errorf("extension: write command output: %w", err)
				}
			}
			if result.ExitCode != 0 {
				return fmt.Errorf("extension: command %q exited with %d", declaration.Name, result.ExitCode)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(e.manifest.Prompts) > 0 {
		registrar, ok := registry.(PromptRegistrar)
		if !ok {
			return errors.New("extension: prompt registration unavailable")
		}
		for _, declaration := range e.manifest.Prompts {
			if err := registrar.RegisterPrompt(PromptRegistration{Name: declaration.ID, Text: declaration.Text, Priority: declaration.Priority}); err != nil {
				return err
			}
		}
	}
	for _, declaration := range e.manifest.Providers {
		provider, err := llm.NewExtensionProvider(&sdkProvider{declaration: declaration, caller: e.caller})
		if err != nil {
			return err
		}
		if err := registry.RegisterProvider(provider); err != nil {
			return err
		}
	}
	return nil
}

type sdkTool struct {
	declaration api.Tool
	caller      sdkCaller
}

var _ tools.ToolDefinition = (*sdkTool)(nil)
var _ tools.Parameterized = (*sdkTool)(nil)

func (t *sdkTool) Name() string        { return t.declaration.Name }
func (t *sdkTool) Description() string { return t.declaration.Description }
func (t *sdkTool) Parameters() any {
	var schema any
	if len(t.declaration.Parameters) > 0 {
		_ = json.Unmarshal(t.declaration.Parameters, &schema)
	}
	return schema
}
func (t *sdkTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	var result api.ToolResult
	if err := t.caller.call(ctx, "tool.call", api.ToolCall{ID: call.ID, Name: t.Name(), Args: call.Args}, &result); err != nil {
		return nil, err
	}
	return &tools.ToolResult{Output: result.Output, Metadata: result.Metadata, IsError: result.IsError, ToolCallID: call.ID}, nil
}

type sdkHook struct {
	declaration api.Hook
	caller      sdkCaller
}

var _ Hook = (*sdkHook)(nil)

func (h *sdkHook) Name() string { return h.declaration.Name }
func (h *sdkHook) Handle(ctx context.Context, event HookEvent) HookResult {
	matched := false
	for _, name := range h.declaration.Events {
		if name == event.Name {
			matched = true
			break
		}
	}
	if !matched {
		return HookResult{Action: HookActionPass}
	}
	data, err := json.Marshal(event.Data)
	if err != nil {
		return HookResult{Action: HookActionBlock, Reason: "extension: hook event cannot be encoded"}
	}
	var patch api.HookPatch
	if err = h.caller.call(ctx, "hook.handle", api.HookEvent{Hook: h.Name(), Name: event.Name, Data: data}, &patch); err != nil {
		return HookResult{Action: HookActionBlock, Reason: "extension: hook callback failed"}
	}
	action := HookAction(patch.Action)
	if action != HookActionPass && action != HookActionBlock && action != hookActionTerminate {
		return HookResult{Action: HookActionBlock, Reason: "extension: invalid hook action"}
	}
	return HookResult{Action: action, Reason: patch.Reason}
}

type sdkProvider struct {
	declaration api.Provider
	caller      sdkCaller
}

var _ llm.ModelProvider = (*sdkProvider)(nil)

func (p *sdkProvider) Name() string { return p.declaration.Name }
func (p *sdkProvider) Models() []llm.ModelInfo {
	models := make([]llm.ModelInfo, 0, len(p.declaration.Models))
	for _, m := range p.declaration.Models {
		models = append(models, llm.ModelInfo{Name: m.Name, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens, ToolCall: m.ToolCall})
	}
	return models
}
func (p *sdkProvider) Build(_ context.Context, config llm.ModelConfig) (llm.BaseChatModel, func(), error) {
	found := false
	for _, model := range p.declaration.Models {
		if model.Name == config.Model {
			found = true
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("extension: provider %q has no model %q", p.Name(), config.Model)
	}
	return &sdkModel{provider: p.Name(), config: config, caller: p.caller}, func() {}, nil
}

type sdkModel struct {
	provider string
	config   llm.ModelConfig
	caller   sdkCaller
}

var _ llm.BaseChatModel = (*sdkModel)(nil)

func (m *sdkModel) Generate(ctx context.Context, messages []llm.Message, options ...llm.Option) (*llm.Message, error) {
	var request api.ModelRequest
	request.Provider = m.provider
	request.Model = m.config.Model
	if err := convertSDK(messages, &request.Messages); err != nil {
		return nil, err
	}
	settings := llm.GenerationOptions{}
	for _, option := range options {
		option(&settings)
	}
	if err := convertSDK(settings.Tools, &request.Tools); err != nil {
		return nil, err
	}
	request.Temperature = settings.Temperature
	request.MaxTokens = settings.MaxTokens
	request.Stop = settings.StopStrings
	if request.Temperature == nil && m.config.TemperatureSet {
		request.Temperature = &m.config.Temperature
	}
	if request.MaxTokens == nil && m.config.MaxTokensSet {
		request.MaxTokens = &m.config.MaxTokens
	}
	var response api.Message
	if err := m.caller.call(ctx, "provider.generate", request, &response); err != nil {
		return nil, err
	}
	var result llm.Message
	if err := convertSDK(response, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Stream deliberately emits one final response: v1 does not negotiate token streaming.
func (m *sdkModel) Stream(ctx context.Context, messages []llm.Message, options ...llm.Option) (<-chan llm.MessageChunk, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	output := make(chan llm.MessageChunk, 1)
	go func() {
		defer close(output)
		message, err := m.Generate(ctx, messages, options...)
		if err != nil {
			select {
			case output <- llm.MessageChunk{Error: err}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- llm.MessageChunk{Role: message.Role, Content: message.Content, ToolCalls: message.ToolCalls, Usage: message.Usage, FinishReason: message.FinishReason, Final: true}:
		case <-ctx.Done():
		}
	}()
	return output, nil
}

func convertSDK(input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("extension: encode SDK value: %w", err)
	}
	if len(data) > api.MaxFrameBytes {
		return errors.New("extension: SDK value exceeds frame budget")
	}
	if err = json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("extension: decode SDK value: %w", err)
	}
	return nil
}
