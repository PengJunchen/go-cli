package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/tools"
)

const reportEnvironment = "GOCLI_R6_PROJECT_PLUGIN_REPORT"

func New() ([]extension.Extension, error) {
	return []extension.Extension{projectExtension{}}, nil
}

type projectExtension struct{}

func (projectExtension) Name() string { return "r6-native-project-extension" }

func (projectExtension) Init(ctx context.Context, registry extension.ExtensionRegistry) error {
	return registry.RegisterTool(ctx, projectExtensionTool{})
}

func (projectExtension) Shutdown(context.Context) error { return nil }

type projectExtensionTool struct{}

func (projectExtensionTool) Name() string { return "r6_native_project_extension_tool" }

func (projectExtensionTool) Description() string { return "R6 native project extension trust fixture" }

func (projectExtensionTool) Execute(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := os.Getenv(reportEnvironment)
	if path == "" {
		return nil, errors.New("native project extension report path is unavailable")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open native project extension report: %w", err)
	}
	_, writeErr := file.WriteString("executed\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, fmt.Errorf("write native project extension report: %w", err)
	}
	return &tools.ToolResult{Output: "r6 native project extension ran"}, nil
}
