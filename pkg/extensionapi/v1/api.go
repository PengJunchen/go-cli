// Package extensionapi defines the versioned external extension contract.
// It depends only on the Go standard library; internal host types never cross the wire.
package extensionapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Version identifies the supported wire and host contract.
const Version = "1.0"

// MaxFrameBytes bounds a JSONL request or response, including its newline.
const MaxFrameBytes = 1 << 20

// Manifest declares compatible capabilities without executable host references.
type Manifest struct {
	ID             string               `json:"id"`
	Version        string               `json:"version"`
	HostAPI        string               `json:"host_api"`
	Capabilities   []string             `json:"capabilities"`
	Requires       []string             `json:"requires,omitempty"`
	Provides       []string             `json:"provides,omitempty"`
	Permissions    []string             `json:"permissions,omitempty"`
	ArtifactDigest string               `json:"artifact_digest,omitempty"`
	Tools          []Tool               `json:"tools,omitempty"`
	Hooks          []Hook               `json:"hooks,omitempty"`
	Commands       []Command            `json:"commands,omitempty"`
	Providers      []Provider           `json:"providers,omitempty"`
	Skills         []Skill              `json:"skills,omitempty"`
	Prompts        []PromptContribution `json:"prompts,omitempty"`
}

// Clone returns detached manifest metadata, including nested schema bytes.
func (m Manifest) Clone() Manifest {
	copy := m
	copy.Capabilities = append([]string(nil), m.Capabilities...)
	copy.Requires = append([]string(nil), m.Requires...)
	copy.Provides = append([]string(nil), m.Provides...)
	copy.Permissions = append([]string(nil), m.Permissions...)
	copy.Tools = append([]Tool(nil), m.Tools...)
	for i := range copy.Tools {
		copy.Tools[i].Parameters = append(json.RawMessage(nil), m.Tools[i].Parameters...)
	}
	copy.Hooks = append([]Hook(nil), m.Hooks...)
	for i := range copy.Hooks {
		copy.Hooks[i].Events = append([]string(nil), m.Hooks[i].Events...)
	}
	copy.Commands = append([]Command(nil), m.Commands...)
	copy.Providers = append([]Provider(nil), m.Providers...)
	for i := range copy.Providers {
		copy.Providers[i].Models = append([]Model(nil), m.Providers[i].Models...)
	}
	copy.Skills = append([]Skill(nil), m.Skills...)
	copy.Prompts = append([]PromptContribution(nil), m.Prompts...)
	return copy
}

// Validate rejects unknown capabilities and unsupported API ranges before initialization.
func (m Manifest) Validate() error {
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("extensionapi: manifest requires id and version")
	}
	if m.HostAPI != Version && m.HostAPI != "^1.0" && m.HostAPI != "1" {
		return fmt.Errorf("extensionapi: unsupported host API %q", m.HostAPI)
	}
	known := map[string]bool{"tools": true, "hooks": true, "commands": true, "providers": true, "skills": true, "prompts": true}
	declared := map[string]bool{}
	for _, c := range m.Capabilities {
		if !known[c] || declared[c] {
			return fmt.Errorf("extensionapi: unknown or duplicate capability %q", c)
		}
		declared[c] = true
	}
	if len(m.Tools) > 0 && !declared["tools"] || len(m.Hooks) > 0 && !declared["hooks"] || len(m.Commands) > 0 && !declared["commands"] || len(m.Providers) > 0 && !declared["providers"] || len(m.Skills) > 0 && !declared["skills"] || len(m.Prompts) > 0 && !declared["prompts"] {
		return fmt.Errorf("extensionapi: contribution lacks negotiated capability")
	}
	seen := map[string]bool{}
	check := func(kind, name string) error {
		key := kind + ":" + name
		if strings.TrimSpace(name) == "" || seen[key] {
			return fmt.Errorf("extensionapi: duplicate or empty %s %q", kind, name)
		}
		seen[key] = true
		return nil
	}
	for _, t := range m.Tools {
		if err := check("tool", t.Name); err != nil {
			return err
		}
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			return fmt.Errorf("extensionapi: invalid tool schema %q", t.Name)
		}
	}
	for _, h := range m.Hooks {
		if err := check("hook", h.Name); err != nil {
			return err
		}
	}
	for _, c := range m.Commands {
		if err := check("command", c.Name); err != nil {
			return err
		}
	}
	for _, p := range m.Providers {
		if err := check("provider", p.Name); err != nil {
			return err
		}
	}
	return nil
}

// Tool is a model-visible tool declaration.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is a tool invocation.
type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ToolResult carries a tool's output and structured status.
type ToolResult struct {
	Output   string         `json:"output"`
	Metadata map[string]any `json:"metadata,omitempty"`
	IsError  bool           `json:"is_error,omitempty"`
}

// Hook declares a hook's subscribed event names.
type Hook struct {
	Name     string   `json:"name"`
	Events   []string `json:"events"`
	Priority int      `json:"priority,omitempty"`
}

// HookEvent is an immutable wire event.
type HookEvent struct {
	Hook string          `json:"hook"`
	Name string          `json:"name"`
	Data json.RawMessage `json:"data,omitempty"`
}

// HookPatch limits hooks to the host's existing pass/block/terminate semantics.
type HookPatch struct {
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// Command declares a pre-route CLI command.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CommandRequest carries arguments without a shell expansion step.
type CommandRequest struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

// CommandResult is the command's printable result and exit status.
type CommandResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// Provider declares supported model names.
type Provider struct {
	Name   string  `json:"name"`
	Models []Model `json:"models"`
}

// Model describes model capability limits.
type Model struct {
	Name            string `json:"name"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	ToolCall        bool   `json:"tool_call"`
}

// Message carries model conversation data and preserves multimodal content as JSON.
type Message struct {
	Role          string          `json:"role"`
	Content       string          `json:"content"`
	ContentBlocks json.RawMessage `json:"content_blocks,omitempty"`
	ToolCalls     []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
	Name          string          `json:"name,omitempty"`
	FinishReason  string          `json:"finish_reason,omitempty"`
	Usage         *Usage          `json:"usage,omitempty"`
}

// Usage records model token counts.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ModelRequest carries one complete provider request; secrets are not included implicitly.
type ModelRequest struct {
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
}

// StreamEvent reserves a versioned incremental model event shape.
type StreamEvent struct {
	Content string   `json:"content,omitempty"`
	Final   bool     `json:"final,omitempty"`
	Message *Message `json:"message,omitempty"`
}

// Skill declares progressive skill discovery metadata and a content handle.
type Skill struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContentDigest string `json:"content_digest"`
	Handle        string `json:"handle"`
}

// PromptContribution declares ordered prompt text.
type PromptContribution struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Priority int    `json:"priority"`
}

// Diagnostic is a safe error category with an optional source identifier.
type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
}

// Request is one bounded JSONL RPC request.
type Request struct {
	ID               uint64          `json:"id"`
	Method           string          `json:"method"`
	Params           json.RawMessage `json:"params,omitempty"`
	DeadlineUnixNano int64           `json:"deadline_unix_nano,omitempty"`
}

// Response is a matched request result. Unknown execution status must never be replayed automatically.
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Diagnostic     `json:"error,omitempty"`
}

// Handler implements external callbacks; "initialize" is handled by Serve.
type Handler interface {
	Handle(context.Context, Request) (json.RawMessage, error)
}

// HandlerFunc adapts a callback to Handler.
type HandlerFunc func(context.Context, Request) (json.RawMessage, error)

// Handle invokes the callback.
func (f HandlerFunc) Handle(ctx context.Context, r Request) (json.RawMessage, error) {
	return f(ctx, r)
}
