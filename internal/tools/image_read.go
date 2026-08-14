package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// imageMimeTypes maps supported image file extensions to their MIME types.
var imageMimeTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// ImageReadTool reads an image file from disk and returns its base64-encoded
// content as a multimodal data URI. It implements the ToolDefinition and
// Parameterized interfaces.
type ImageReadTool struct{}

var (
	_ ToolDefinition = (*ImageReadTool)(nil)
	_ Parameterized  = (*ImageReadTool)(nil)
)

// NewImageReadTool returns a new ImageReadTool.
func NewImageReadTool() *ImageReadTool {
	return &ImageReadTool{}
}

// Name returns the tool name.
func (t *ImageReadTool) Name() string { return "image_read" }

// Description returns a brief description of the tool.
func (t *ImageReadTool) Description() string {
	return "image_read: reads an image file and returns its content as base64. Args: path (string)."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *ImageReadTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The path to the image file to read.",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

// Execute reads the image file at the given path, validates it has a supported
// extension, and returns its base64-encoded content as a data URI.
func (t *ImageReadTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	path, ok := call.Args["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("image_read: missing string argument 'path'")
	}

	ext := strings.ToLower(filepath.Ext(path))
	mimeType, supported := imageMimeTypes[ext]
	if !supported {
		return nil, fmt.Errorf("image_read: unsupported image format %q (supported: .png, .jpg, .jpeg, .gif, .webp)", ext)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("image_read: %w", err)
	}

	dataURI := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)

	return &ToolResult{
		Output:     fmt.Sprintf("Read image file %s (%s, %d bytes)\n%s", path, mimeType, len(data), dataURI),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"mime_type": mimeType,
			"file_size": len(data),
			"file_path": path,
			"data_uri":  dataURI,
		},
	}, nil
}
