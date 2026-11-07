package core

import (
	"encoding/base64"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func toolResultContentBlocks(result *tools.ToolResult) ([]llm.ContentBlock, error) {
	if result == nil || len(result.Images) == 0 || result.IsError {
		return nil, nil
	}
	images, err := tools.ValidateToolImages(result.Images)
	if err != nil {
		return nil, err
	}
	blocks := make([]llm.ContentBlock, 0, len(images)+1)
	blocks = append(blocks, llm.ContentBlock{Type: "text", Text: result.Output})
	for _, item := range images {
		blocks = append(blocks, llm.ContentBlock{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:" + item.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(item.Data), Detail: "high"}})
	}
	return blocks, nil
}

// attachToolContentBlocks extends the same canonical receipt occurrence. It
// does not create a user input or alter the tool-call/source identity.
func attachToolContentBlocks(messages *[]llm.Message, transcript *[]TranscriptEntry, blocks []llm.ContentBlock) {
	if blocks == nil {
		return
	}
	copied := cloneAgentMessage(AgentMessage{ContentBlocks: blocks})
	(*messages)[len(*messages)-1].ContentBlocks = copied.ContentBlocks
	copied = cloneAgentMessage(AgentMessage{ContentBlocks: blocks})
	(*transcript)[len(*transcript)-1].Message.ContentBlocks = copied.ContentBlocks
}
