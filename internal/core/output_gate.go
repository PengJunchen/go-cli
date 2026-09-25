package core

import (
	"context"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/llm"
)

const (
	guardedStreamMaxBytes     = llm.GuardedStreamMaxBytes
	guardedStreamMaxChunks    = llm.GuardedStreamMaxChunks
	guardedStreamMaxToolCalls = llm.GuardedStreamMaxToolCalls
)

// CandidateOutputResult is the safe text decision for one complete logical
// assistant response. Tool arguments remain structured and are reviewed by
// the normal tool approval and schema paths.
type CandidateOutputResult struct {
	Allowed   bool
	Sanitized string
}

// CompleteResponseGuard checks plain text only after all provider
// continuations have completed successfully.
type CompleteResponseGuard interface {
	MaxCandidateBytes() int
	CheckCandidate(context.Context, string) (*CandidateOutputResult, error)
}

type guardedStreamBudget struct {
	bytes  int
	chunks int
}

func (b *guardedStreamBudget) accountChunk(chunk llm.MessageChunk) error {
	b.chunks++
	if b.chunks > guardedStreamMaxChunks {
		return fmt.Errorf("core: stream exceeds %d chunks", guardedStreamMaxChunks)
	}
	// Chunks are transport fragments, not independently retained payloads.
	// Count their UTF-8 text once as it is appended; the complete candidate
	// later accounts for tool calls and metadata in one serialization.
	textBytes := llm.RetainedStreamChunkTextBytes(chunk)
	if textBytes > guardedStreamMaxBytes-b.bytes {
		return fmt.Errorf("core: stream exceeds %d-byte candidate limit", guardedStreamMaxBytes)
	}
	b.bytes += textBytes
	return nil
}

func candidatePayloadBytes(message *llm.Message) (int, error) {
	encodedBytes, err := llm.RetainedMessagePayloadBytes(message)
	if err != nil {
		return 0, fmt.Errorf("core: encode complete response: %w", err)
	}
	return encodedBytes, nil
}
