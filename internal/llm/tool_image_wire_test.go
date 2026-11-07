package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func wireImageFixture(t *testing.T) string {
	t.Helper()
	var buffer bytes.Buffer
	require.NoError(t, png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func wireImageMessages(t *testing.T) []Message {
	return []Message{
		{Role: RoleUser, Content: "inspect fixture"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "read-image", Name: "read", Args: map[string]any{"path": "fixture.png"}}, {ID: "read-text", Name: "read", Args: map[string]any{"path": "fixture.txt"}}}},
		{Role: RoleTool, ToolCallID: "read-image", Name: "read", Content: "image receipt", ContentBlocks: []ContentBlock{{Type: "text", Text: "image receipt"}, {Type: "image_url", ImageURL: &ImageURL{URL: wireImageFixture(t), Detail: "high"}}}},
		{Role: RoleTool, ToolCallID: "read-text", Name: "read", Content: "text receipt"},
	}
}

func TestChatToolImageProjectionAcrossBothEncodersAndFraming(t *testing.T) {
	messages := wireImageMessages(t)
	before, err := json.Marshal(messages)
	require.NoError(t, err)
	encoders := map[string]func([]Message) ([]byte, error){
		"native": func(msgs []Message) ([]byte, error) {
			return encodeOpenAIRequest(ModelConfig{}, "test-model", msgs, nil)
		},
		"http": func(msgs []Message) ([]byte, error) {
			return (&HTTPChatModel{cfg: ModelConfig{Model: "test-model"}}).buildBody(msgs)
		},
	}
	for name, encode := range encoders {
		t.Run(name, func(t *testing.T) {
			body, err := encode(messages)
			require.NoError(t, err)
			var payload struct {
				Messages []openAIMessage `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(body, &payload))
			require.Len(t, payload.Messages, 5)
			require.Equal(t, []string{"user", "assistant", "tool", "tool", "user"}, []string{payload.Messages[0].Role, payload.Messages[1].Role, payload.Messages[2].Role, payload.Messages[3].Role, payload.Messages[4].Role})
			require.Equal(t, "image receipt", payload.Messages[2].Content)
			require.Equal(t, "read-image", payload.Messages[2].ToolCallID)
			require.Equal(t, "text receipt", payload.Messages[3].Content)
			parts := payload.Messages[4].Content.([]any)
			require.Len(t, parts, 2)
			actual := parts[1].(map[string]any)["image_url"].(map[string]any)
			require.Equal(t, messages[2].ContentBlocks[1].ImageURL.URL, actual["url"])
			require.Equal(t, "high", actual["detail"])
		})
	}
	framing, err := estimateOpenAICompatibleFraming(messages, GenerationOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, framing, 3+5*4+2*5, "auxiliary transport message must be included in framing")
	after, err := json.Marshal(messages)
	require.NoError(t, err)
	require.Equal(t, before, after, "transport projection must not mutate canonical messages")
}

func TestChatToolImageRejectsIncompleteUnsafeOrOversizedGroup(t *testing.T) {
	for _, name := range []string{"incomplete-group", "remote-url", "invalid-base64", "aggregate-bytes"} {
		t.Run(name, func(t *testing.T) {
			messages := wireImageMessages(t)
			switch name {
			case "incomplete-group":
				messages = messages[:3]
			case "remote-url":
				messages[2].ContentBlocks[1].ImageURL.URL = "https://invalid.example/fixture.png"
			case "invalid-base64":
				messages[2].ContentBlocks[1].ImageURL.URL = "data:image/png;base64,?"
			case "aggregate-bytes":
				data := make([]byte, 300000)
				url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
				messages[2].ContentBlocks[1].ImageURL.URL = url
				messages[3].ContentBlocks = []ContentBlock{{Type: "image_url", ImageURL: &ImageURL{URL: url}}}
			}
			_, err := encodeOpenAIRequest(ModelConfig{}, "test-model", messages, nil)
			require.Error(t, err)
			_, err = estimateOpenAICompatibleFraming(messages, GenerationOptions{})
			require.Error(t, err)
		})
	}
	messages := wireImageMessages(t)
	_, err := encodeClaudeRequest(ModelConfig{}, "test-model", messages, nil)
	require.Error(t, err)
	_, err = encodeGeminiRequest(ModelConfig{}, messages, nil)
	require.Error(t, err)
}

func TestChatToolImageRejectsEncodedGroupOverflow(t *testing.T) {
	messages := wireImageMessages(t)
	tiny := ContentBlock{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64,YQ==", Detail: "high"}}
	messages[2].ContentBlocks = make([]ContentBlock, 14000)
	for i := range messages[2].ContentBlocks {
		messages[2].ContentBlocks[i] = tiny
	}
	_, _, err := projectChatToolImages(messages)
	require.Error(t, err, "encoded aux image overhead can exceed 1 MiB even when decoded bytes are small")
	messages = wireImageMessages(t)
	messages[1].ToolCalls[0].ID = strings.Repeat("x", 1<<20)
	messages[2].ToolCallID = messages[1].ToolCalls[0].ID
	_, _, err = projectChatToolImages(messages)
	require.Error(t, err, "the full encoded auxiliary message including source label must be bounded")
}

func TestChatToolImageOnlyFramingIncludesAllAddedText(t *testing.T) {
	messages := wireImageMessages(t)
	messages[2].Content = ""
	messages[2].ContentBlocks = messages[2].ContentBlocks[1:]
	messages[3].Content = ""
	messages[3].ContentBlocks = []ContentBlock{{Type: "image_url", ImageURL: &ImageURL{URL: wireImageFixture(t)}}}
	projected, reserve, err := projectChatToolImages(messages)
	require.NoError(t, err)
	require.Equal(t, "", projected[2].Content)
	require.Equal(t, "", projected[3].Content)
	require.Equal(t, len(projected[4].ContentBlocks[0].Text), reserve, "all adapter-owned text must enter prepared framing reserve")
}
