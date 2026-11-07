package compaction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// ErrImageCompactionBlocked means a local compaction would lose image evidence.
// It does not assert unknown tool execution or create a workspace quarantine.
var ErrImageCompactionBlocked = errors.New("compaction: protected image exchange cannot be replaced")

// ImageCompactionBlockedError identifies a recoverable local capacity/projection failure.
type ImageCompactionBlockedError struct{ Reason string }

func (e *ImageCompactionBlockedError) Error() string {
	return ErrImageCompactionBlocked.Error() + ": " + e.Reason
}
func (e *ImageCompactionBlockedError) Unwrap() error { return ErrImageCompactionBlocked }

func imageCompactionBlocked(reason string) error { return &ImageCompactionBlockedError{Reason: reason} }

func itemHasImage(item TurnItem) bool {
	for _, block := range item.ContentBlocks {
		if block.Type == "image_url" {
			return true
		}
	}
	return false
}

// ValidateImageGroupCut rejects a prefix boundary that covers any part of a
// protected exchange, including its assistant and same-group text receipts.
func ValidateImageGroupCut(items []TurnItem, cut int) error {
	normalized, groups, err := groupTurnItems(items)
	if err != nil {
		return err
	}
	for _, group := range groups {
		for _, item := range normalized[group.start:group.end] {
			if itemHasImage(item) && cut > group.start {
				return imageCompactionBlocked("the replacement prefix covers an image exchange")
			}
		}
	}
	return nil
}

type imageGroupSnapshot struct {
	key     string
	payload []byte
}

func imageGroupKey(item TurnItem) (string, error) {
	source := item.Runtime.Source
	switch source.Kind {
	case llm.SourcePriorPublished, llm.SourceRunTranscript:
		data, err := json.Marshal(source)
		return string(data), err
	default:
		if item.ID == "" {
			return "", imageCompactionBlocked("an image exchange has no stable source identity")
		}
		return "legacy:" + item.ID, nil
	}
}

func imageGroupPayload(items []TurnItem) ([]byte, error) {
	values := append([]TurnItem(nil), items...)
	runtime := make([]llm.RuntimeMetadata, len(items))
	for i := range values {
		values[i].EstimatedTokens = 0
		runtime[i] = values[i].Runtime
	}
	return json.Marshal(struct {
		Items   []TurnItem
		Runtime []llm.RuntimeMetadata
	}{values, runtime})
}

func captureImageGroups(items []TurnItem) ([]imageGroupSnapshot, error) {
	normalized, groups, err := groupTurnItems(items)
	if err != nil {
		return nil, err
	}
	var result []imageGroupSnapshot
	for _, group := range groups {
		protected := false
		for _, item := range normalized[group.start:group.end] {
			protected = protected || itemHasImage(item)
		}
		if !protected {
			continue
		}
		key, err := imageGroupKey(normalized[group.start])
		if err != nil {
			return nil, err
		}
		payload, err := imageGroupPayload(normalized[group.start:group.end])
		if err != nil {
			return nil, err
		}
		result = append(result, imageGroupSnapshot{key: key, payload: payload})
	}
	return result, nil
}

func validateCapturedImageGroups(expected []imageGroupSnapshot, after []TurnItem) error {
	normalized, groups, err := groupTurnItems(after)
	if err != nil {
		if len(expected) == 0 {
			return err
		}
		return imageCompactionBlocked("the result has an incomplete image exchange")
	}
	next := 0
	seen := make(map[string]bool, len(expected))
	positions := make(map[string]int, len(expected))
	for i, group := range expected {
		if _, duplicate := positions[group.key]; duplicate {
			return imageCompactionBlocked("the original image source is ambiguous")
		}
		positions[group.key] = i
	}
	for _, group := range groups {
		members := normalized[group.start:group.end]
		key, keyErr := imageGroupKey(members[0])
		position, protected := positions[key]
		hasImage := false
		for _, item := range members {
			hasImage = hasImage || itemHasImage(item)
		}
		if !protected {
			if hasImage {
				return imageCompactionBlocked("the result introduced or changed an image source")
			}
			continue
		}
		if keyErr != nil || seen[key] || position != next {
			return imageCompactionBlocked("an image exchange was duplicated or reordered")
		}
		payload, err := imageGroupPayload(members)
		if err != nil {
			return fmt.Errorf("encode image exchange: %w", err)
		}
		if !bytes.Equal(payload, expected[position].payload) {
			return imageCompactionBlocked("an image exchange or its source changed")
		}
		seen[key] = true
		next++
	}
	if next != len(expected) {
		return imageCompactionBlocked("an image exchange was removed")
	}
	return nil
}

// ValidateImageGroupRetention preserves source-qualified complete exchanges.
// Tool-call IDs may be reused in a different closed exchange.
func ValidateImageGroupRetention(before, after []TurnItem) error {
	expected, err := captureImageGroups(before)
	if err != nil {
		return err
	}
	return validateCapturedImageGroups(expected, after)
}

// CompactPreservingImages validates a strategy before its result is adopted.
// Callers retain their existing defensive input isolation and source checks.
func CompactPreservingImages(ctx context.Context, delegate Compactor, items []TurnItem, maxTokens int, estimator TokenEstimator) (CompactionResult, error) {
	expected, err := captureImageGroups(items)
	if err != nil {
		return CompactionResult{Items: items}, err
	}
	result, err := delegate.Compact(ctx, items, maxTokens, estimator)
	if err != nil {
		return result, err
	}
	if err := validateCapturedImageGroups(expected, result.Items); err != nil {
		return CompactionResult{Items: items}, err
	}
	return result, nil
}
