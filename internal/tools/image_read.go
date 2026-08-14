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

// maxImageFileSize caps the image file size at 20 MiB to prevent excessive
// memory consumption from large files.
const maxImageFileSize = 20 << 20

// ImageReadOption configures an ImageReadTool.
type ImageReadOption func(*ImageReadTool)

// WithImageAllowedDirs restricts the tool to reading image files only from
// the given base directories. When empty, the tool defaults to the current
// working directory to prevent path traversal to arbitrary locations.
func WithImageAllowedDirs(dirs []string) ImageReadOption {
	return func(t *ImageReadTool) {
		if len(dirs) == 0 {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			dirs = []string{cwd}
		}
		cleaned := make([]string, 0, len(dirs))
		for _, d := range dirs {
			abs, err := filepath.Abs(d)
			if err != nil {
				abs = d
			}
			cleaned = append(cleaned, filepath.Clean(abs))
		}
		t.allowedDirs = cleaned
	}
}

// ImageReadTool reads an image file from disk and returns its base64-encoded
// content as a multimodal data URI. It implements the ToolDefinition and
// Parameterized interfaces.
type ImageReadTool struct {
	allowedDirs []string
}

var (
	_ ToolDefinition = (*ImageReadTool)(nil)
	_ Parameterized  = (*ImageReadTool)(nil)
)

// NewImageReadTool returns a new ImageReadTool. By default the tool is
// restricted to the current working directory; use WithImageAllowedDirs to
// broaden the scope.
func NewImageReadTool(opts ...ImageReadOption) *ImageReadTool {
	t := &ImageReadTool{}
	for _, opt := range opts {
		opt(t)
	}
	if t.allowedDirs == nil {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		abs, err := filepath.Abs(cwd)
		if err != nil {
			abs = cwd
		}
		t.allowedDirs = []string{filepath.Clean(abs)}
	}
	return t
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
// extension, is within the allowed directories, and does not exceed the size
// limit, then returns its base64-encoded content as a data URI.
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

	// Resolve to absolute path and verify it falls within an allowed directory.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("image_read: cannot resolve path %q: %w", path, err)
	}
	if !t.isPathAllowed(absPath) {
		return nil, fmt.Errorf("image_read: path %q is outside the allowed directories", path)
	}

	// Check file size before reading to avoid loading oversized files.
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("image_read: %w", err)
	}
	if info.Size() > maxImageFileSize {
		return nil, fmt.Errorf("image_read: file size %d bytes exceeds the %d byte limit", info.Size(), maxImageFileSize)
	}

	data, err := os.ReadFile(absPath)
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

// isPathAllowed reports whether absPath is equal to or nested under one of the
// allowed directories.
func (t *ImageReadTool) isPathAllowed(absPath string) bool {
	cleaned := filepath.Clean(absPath)
	for _, dir := range t.allowedDirs {
		if cleaned == dir {
			return true
		}
		if strings.HasPrefix(cleaned, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
