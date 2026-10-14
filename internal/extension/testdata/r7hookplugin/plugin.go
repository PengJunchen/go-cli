package main

import (
	"context"
	"os"
	"strings"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/tools"
)

const ownerName = "r7-fixture-owner"

func New() ([]extension.Extension, error) {
	report("Load")
	return []extension.Extension{&fixtureExtension{}}, nil
}

type fixtureExtension struct{}

func (*fixtureExtension) Name() string { return ownerName }

func (*fixtureExtension) Init(ctx context.Context, registry extension.ExtensionRegistry) error {
	report("Init")
	if err := registry.RegisterHook(ctx, &fixtureHook{}); err != nil {
		return err
	}
	return registry.RegisterTool(ctx, &fixtureTool{})
}

func (*fixtureExtension) Shutdown(context.Context) error {
	report("Shutdown")
	return nil
}

type fixtureHook struct{}

type fixtureTool struct{}

func (*fixtureTool) Name() string        { return "r7_fixture_tool" }
func (*fixtureTool) Description() string { return "R7 configured plugin dispatch fixture" }
func (*fixtureTool) Parameters() any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []any{"value"},
	}
}
func (*fixtureTool) Execute(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	value, _ := call.Args["value"].(string)
	report("ToolExecute|" + value)
	return &tools.ToolResult{Output: "r7 plugin tool:" + value}, nil
}

func (*fixtureHook) Name() string { return "fixture-hook-owner-spoof" }

func (*fixtureHook) Handle(_ context.Context, event extension.HookEvent) extension.HookResult {
	switch snapshot := event.Data.(type) {
	case extension.HookBeforeRunSnapshot:
		if snapshot.Submission.Metadata != nil {
			if nested, ok := snapshot.Submission.Metadata["nested"].(map[string]any); ok {
				nested["value"] = "mutated-before"
			}
		}
		snapshot.Submission.Content = "mutated-before"
		report(event.Name + "|" + event.Source + "|" + snapshot.Submission.Content)
	case extension.HookAfterRunSnapshot:
		present, class := snapshot.Error.Present, string(snapshot.Error.Class)
		snapshot.Run.Message = "mutated-after"
		if snapshot.Submission.Metadata != nil {
			if nested, ok := snapshot.Submission.Metadata["nested"].(map[string]any); ok {
				nested["value"] = "mutated-after"
			}
		}
		report(event.Name + "|" + event.Source + "|error-present=" + boolString(present) + "|class=" + class)
		return extension.HookResult{Action: extension.HookActionBlock, Reason: "fixture-reason-canary-608c"}
	default:
		report("unexpected-event-data")
	}
	return extension.HookResult{Action: extension.HookActionPass}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func report(line string) {
	if len(line) > 512 {
		line = line[:512]
	}
	path := os.Getenv("GOCLI_R7_PLUGIN_REPORT")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(strings.TrimSpace(line) + "\n")
	_ = file.Close()
}
