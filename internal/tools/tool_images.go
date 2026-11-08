package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // Register the supported JPEG decoder.
	_ "image/png"  // Register the supported PNG decoder.
	"net/http"
	"path/filepath"
	"strings"
)

// MaxToolImageBytes bounds one original image, not its base64 or JSON encoding.
const MaxToolImageBytes = 512 << 10

const maxToolImageDimension = 4096
const maxToolImagePixels = 16777216

// ToolImage holds original validated local image bytes as tool-result data.
// It cannot refer to a path or a remote URL.
type ToolImage struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"data"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// ValidateToolImages validates and clones image data before a consumer retains it.
// The aggregate original bytes are bounded independently of encoded event limits.
func ValidateToolImages(images []ToolImage) ([]ToolImage, error) {
	if images == nil {
		return nil, nil
	}
	result := make([]ToolImage, 0, len(images))
	total := 0
	for _, item := range images {
		if len(item.Data) > MaxToolImageBytes-total {
			return nil, errors.New("tool images exceed the aggregate byte limit")
		}
		valid, err := decodeToolImage(item.Data)
		if err != nil {
			return nil, err
		}
		if valid.MIMEType != item.MIMEType || valid.Width != item.Width || valid.Height != item.Height {
			return nil, errors.New("tool image metadata does not match its bytes")
		}
		total += len(item.Data)
		valid.Data = bytes.Clone(item.Data)
		result = append(result, valid)
	}
	return result, nil
}

func decodeToolImage(data []byte) (ToolImage, error) {
	if len(data) == 0 || len(data) > MaxToolImageBytes {
		return ToolImage{}, errors.New("image exceeds the non-empty 512 KiB byte limit")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ToolImage{}, fmt.Errorf("decode image configuration: %w", err)
	}
	if format != "png" && format != "jpeg" {
		return ToolImage{}, errors.New("only PNG and JPEG images are supported")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxToolImageDimension || config.Height > maxToolImageDimension || int64(config.Width)*int64(config.Height) > maxToolImagePixels {
		return ToolImage{}, errors.New("image dimensions exceed the bounded pixel limit")
	}
	decoded, actualFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ToolImage{}, fmt.Errorf("decode image pixels: %w", err)
	}
	if actualFormat != format || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return ToolImage{}, errors.New("image dimensions changed while decoding")
	}
	return ToolImage{MIMEType: "image/" + format, Data: data, Width: config.Width, Height: config.Height}, nil
}

func readImageResult(ctx context.Context, path string, data []byte, window bool) (*ToolResult, bool, error) {
	mime := http.DetectContentType(data)
	extension := strings.ToLower(filepath.Ext(path))
	imageFile := strings.HasPrefix(mime, "image/") || extension == ".png" || extension == ".jpg" || extension == ".jpeg"
	if !imageFile {
		return nil, false, nil
	}
	if ctx == nil {
		return nil, true, errors.New("read: image context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, true, fmt.Errorf("read: image: %w", err)
	}
	if window {
		return nil, true, errors.New("read: image reads do not support line windows")
	}
	value, err := decodeToolImage(data)
	if err != nil {
		return nil, true, fmt.Errorf("read: image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, true, fmt.Errorf("read: image: %w", err)
	}
	value.Data = bytes.Clone(data)
	return &ToolResult{Output: fmt.Sprintf("Read image file [%s], %d x %d.", value.MIMEType, value.Width, value.Height), Images: []ToolImage{value}, Metadata: map[string]any{"path": path, "bytes": len(data), "mime_type": value.MIMEType, "width": value.Width, "height": value.Height}}, true, nil
}
