package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func imageReadFixture(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, width, height))
	value.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buffer bytes.Buffer
	if format == "jpeg" {
		require.NoError(t, jpeg.Encode(&buffer, value, nil))
	} else {
		require.NoError(t, png.Encode(&buffer, value))
	}
	return buffer.Bytes()
}

func TestReadImageReturnsTypedOriginalBytes(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			data := imageReadFixture(t, format, 32, 16)
			require.NoError(t, os.WriteFile(filepath.Join(root, "fixture."+format), data, 0o600))
			result, err := NewReadTool(WithWorkdir(root)).Execute(context.Background(), ToolCall{Args: map[string]any{"path": "fixture." + format}})
			require.NoError(t, err)
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(encoded, &payload))
			images, ok := payload["images"].([]any)
			require.True(t, ok, "image data must be typed, not raw binary text")
			require.Len(t, images, 1)
			actual := images[0].(map[string]any)
			require.Equal(t, "image/"+format, actual["mime_type"])
			require.Equal(t, float64(32), actual["width"])
			require.Equal(t, float64(16), actual["height"])
			require.Equal(t, base64.StdEncoding.EncodeToString(data), actual["data"])
			require.NotContains(t, result.Output, base64.StdEncoding.EncodeToString(data))
		})
	}
}

func TestReadImageConstraintsAndLegacyText(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		args   map[string]any
		cancel bool
	}{
		{name: "invalid-image", data: []byte("not an image")},
		{name: "truncated-png", data: []byte("\x89PNG\r\n\x1a\n")},
		{name: "image-byte-bound", data: append(imageReadFixture(t, "png", 1, 1), make([]byte, 512*1024)...)},
		{name: "image-width-bound", data: imageReadFixture(t, "png", 4097, 1)},
		{name: "line-window-not-image", data: imageReadFixture(t, "png", 1, 1), args: map[string]any{"offset": 1}},
		{name: "cancel-image", data: imageReadFixture(t, "png", 1, 1), cancel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "fixture.png"), tc.data, 0o600))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			args := map[string]any{"path": "fixture.png"}
			for key, value := range tc.args {
				args[key] = value
			}
			_, err := NewReadTool(WithWorkdir(root)).Execute(ctx, ToolCall{Args: args})
			require.Error(t, err)
		})
	}
	root := t.TempDir()
	data := []byte("first\r\nsecond\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "fixture.txt"), data, 0o600))
	result, err := NewReadTool(WithWorkdir(root)).Execute(context.Background(), ToolCall{Args: map[string]any{"path": "fixture.txt"}})
	require.NoError(t, err)
	require.Equal(t, string(data), result.Output)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "images")
}

func TestReadImageValidationClonesAndBoundsCustomImageResults(t *testing.T) {
	data := imageReadFixture(t, "png", 2, 2)
	input := []ToolImage{{MIMEType: "image/png", Data: data, Width: 2, Height: 2}}
	retained, err := ValidateToolImages(input)
	require.NoError(t, err)
	want := bytes.Clone(data)
	input[0].Data[0] ^= 0xff
	require.Equal(t, want, retained[0].Data, "caller mutation cannot change retained image bytes")
	input[0] = retained[0]
	input[0].Width = 3
	_, err = ValidateToolImages(input)
	require.Error(t, err)
}
