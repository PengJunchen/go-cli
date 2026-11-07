package llm

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxChatToolImageGroupBytes = 512 << 10
const maxChatToolImageEncodedBytes = 1 << 20

// projectChatToolImages is a transport projection only. Tool receipts remain
// canonical tool data; images follow the complete group in a legal Chat user
// message. The returned text reserve accounts for the adapter-owned label.
func projectChatToolImages(messages []Message) ([]Message, int, error) {
	result := make([]Message, 0, len(messages))
	textReserve := 0
	for index := 0; index < len(messages); index++ {
		current := messages[index]
		if current.Role != RoleAssistant || len(current.ToolCalls) == 0 {
			if toolMessageHasImages(current) {
				return nil, 0, errors.New("llm: tool image has no complete preceding assistant tool-call group")
			}
			result = append(result, current)
			continue
		}
		end := index + 1
		hasImages := false
		for end < len(messages) && messages[end].Role == RoleTool {
			hasImages = hasImages || toolMessageHasImages(messages[end])
			end++
		}
		if !hasImages {
			result = append(result, current)
			continue
		}
		calls := make(map[string]string, len(current.ToolCalls))
		for _, call := range current.ToolCalls {
			if call.ID == "" {
				return nil, 0, errors.New("llm: tool image group has an empty call identity")
			}
			if _, duplicate := calls[call.ID]; duplicate {
				return nil, 0, errors.New("llm: tool image group repeats a call identity")
			}
			calls[call.ID] = call.Name
		}
		if end-index-1 != len(calls) {
			return nil, 0, errors.New("llm: tool image group is incomplete")
		}
		result = append(result, current)
		seen := make(map[string]bool, len(calls))
		var images []ContentBlock
		var sourceIDs []string
		total := 0
		encodedImageBytes := 0
		for _, original := range messages[index+1 : end] {
			name, exists := calls[original.ToolCallID]
			if !exists || seen[original.ToolCallID] || (original.Name != "" && original.Name != name) {
				return nil, 0, errors.New("llm: tool image receipt does not match its call group")
			}
			seen[original.ToolCallID] = true
			receipt := original
			if original.ContentBlocks != nil {
				var text strings.Builder
				receipt.ContentBlocks = nil
				imageCount := 0
				for _, block := range original.ContentBlocks {
					switch block.Type {
					case "text":
						text.WriteString(block.Text)
					case "image_url":
						if block.ImageURL == nil {
							return nil, 0, errors.New("llm: tool image has no payload")
						}
						count, err := boundedToolImageData(*block.ImageURL)
						if err != nil {
							return nil, 0, err
						}
						if count > maxChatToolImageGroupBytes-total {
							return nil, 0, errors.New("llm: tool image group exceeds 512 KiB original bytes")
						}
						total += count
						imageURL := *block.ImageURL
						projected := ContentBlock{Type: "image_url", ImageURL: &imageURL}
						encoded, err := json.Marshal(projected)
						if err != nil {
							return nil, 0, fmt.Errorf("llm: encode tool image: %w", err)
						}
						if len(encoded)+1 > maxChatToolImageEncodedBytes-encodedImageBytes {
							return nil, 0, errors.New("llm: encoded tool image group exceeds 1 MiB")
						}
						encodedImageBytes += len(encoded) + 1
						images = append(images, projected)
						imageCount++
					default:
						return nil, 0, errors.New("llm: tool image group contains unsupported content")
					}
				}
				receipt.Content = text.String()
				if imageCount > 0 {
					sourceIDs = append(sourceIDs, original.ToolCallID)

				}
			}
			result = append(result, receipt)
		}
		notice := "Image data from tool result(s): " + strings.Join(sourceIDs, ", ")
		if len(notice) > int(^uint(0)>>1)-textReserve {
			return nil, 0, errors.New("llm: tool image framing reserve overflow")
		}
		textReserve += len(notice)
		parts := make([]ContentBlock, 0, len(images)+1)
		parts = append(parts, ContentBlock{Type: "text", Text: notice})
		parts = append(parts, images...)
		auxiliary := Message{Role: RoleUser, ContentBlocks: parts}
		encodedAuxiliary, err := json.Marshal(auxiliary)
		if err != nil {
			return nil, 0, fmt.Errorf("llm: encode auxiliary image message: %w", err)
		}
		if len(encodedAuxiliary) > maxChatToolImageEncodedBytes {
			return nil, 0, errors.New("llm: encoded auxiliary image message exceeds 1 MiB")
		}
		result = append(result, auxiliary)
		index = end - 1
	}
	return result, textReserve, nil
}

func toolMessageHasImages(message Message) bool {
	if message.Role != RoleTool {
		return false
	}
	for _, block := range message.ContentBlocks {
		if block.Type == "image_url" {
			return true
		}
	}
	return false
}

func boundedToolImageData(value ImageURL) (int, error) {
	var encoded string
	for _, prefix := range []string{"data:image/png;base64,", "data:image/jpeg;base64,"} {
		if strings.HasPrefix(value.URL, prefix) {
			encoded = strings.TrimPrefix(value.URL, prefix)
			break
		}
	}
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(maxChatToolImageGroupBytes) {
		return 0, errors.New("llm: tool images require bounded inline PNG or JPEG data")
	}
	switch value.Detail {
	case "", "auto", "low", "high", "original":
	default:
		return 0, errors.New("llm: unsupported tool image detail")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return 0, fmt.Errorf("llm: decode tool image: %w", err)
	}
	if len(data) == 0 || len(data) > maxChatToolImageGroupBytes {
		return 0, errors.New("llm: tool image exceeds its byte limit")
	}
	return len(data), nil
}

func rejectToolImages(messages []Message) error {
	for _, message := range messages {
		if toolMessageHasImages(message) {
			return errors.New("llm: this provider does not support tool-image transport")
		}
	}
	return nil
}
