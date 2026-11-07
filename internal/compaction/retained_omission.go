package compaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// RetainedExchangeDigest binds the exact provider-visible exchange and each
// source coordinate. Runtime-only cache IDs and estimates are not durable.
func RetainedExchangeDigest(items []TurnItem) (string, error) {
	type member struct {
		Source         llm.SourceRef      `json:"source"`
		Role           string             `json:"role"`
		Content        string             `json:"content"`
		ContentBlocks  []llm.ContentBlock `json:"content_blocks,omitempty"`
		Usage          *llm.Usage         `json:"usage,omitempty"`
		ToolCalls      []llm.ToolCall     `json:"tool_calls,omitempty"`
		ToolCallID     string             `json:"tool_call_id,omitempty"`
		ToolName       string             `json:"tool_name,omitempty"`
		ReceiptOutcome string             `json:"receipt_outcome,omitempty"`
	}
	values := make([]member, len(items))
	for i, item := range items {
		content := item.Content
		if item.Role == RoleTool {
			content = item.ToolResult
		}
		values[i] = member{item.Runtime.Source, item.Role, content, item.ContentBlocks, item.Usage, item.ToolCalls, item.ToolCallID, item.ToolName, item.ReceiptOutcome}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode retained exchange: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// EncodeRetainedSourceRanges returns the stable runtime representation of the
// typed retained exchange list.
func EncodeRetainedSourceRanges(ranges []llm.RetainedSourceRange) (string, error) {
	if len(ranges) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(ranges)
	if err != nil {
		return "", fmt.Errorf("encode retained source ranges: %w", err)
	}
	return string(encoded), nil
}

// DecodeRetainedSourceRanges rejects non-canonical metadata encodings.
func DecodeRetainedSourceRanges(encoded string) ([]llm.RetainedSourceRange, error) {
	if encoded == "" {
		return nil, nil
	}
	var ranges []llm.RetainedSourceRange
	if err := json.Unmarshal([]byte(encoded), &ranges); err != nil {
		return nil, fmt.Errorf("decode retained source ranges: %w", err)
	}
	canonical, err := EncodeRetainedSourceRanges(ranges)
	if err != nil || canonical != encoded || len(ranges) == 0 {
		return nil, fmt.Errorf("non-canonical retained source ranges")
	}
	return ranges, nil
}

func coveredSource(source llm.SourceRef, through llm.SourceThrough) bool {
	switch source.Kind {
	case llm.SourcePriorPublished:
		return source.PriorOrdinal < through.PriorHistoryCount
	case llm.SourceRunTranscript:
		return source.TranscriptIndex < through.TranscriptCount
	default:
		return false
	}
}

func sourceOrder(source llm.SourceRef) (int, uint64) {
	if source.Kind == llm.SourcePriorPublished {
		return 0, source.PriorOrdinal
	}
	return 1, source.TranscriptIndex
}

// retainedOmissionReduced validates the declared complete exchanges in order,
// then removes the declared overlaps so the legacy prefix integrity checker
// can validate the single high-water cursor and ordinary suffix unchanged.
func retainedOmissionReduced(items []TurnItem) ([]TurnItem, error) {
	markerIndex := -1
	var ranges []llm.RetainedSourceRange
	for i, item := range items {
		if item.Runtime.Source.RetainedSourceRanges == "" {
			continue
		}
		if markerIndex >= 0 {
			return nil, fmt.Errorf("multiple retained omission markers")
		}
		var err error
		ranges, err = DecodeRetainedSourceRanges(item.Runtime.Source.RetainedSourceRanges)
		if err != nil {
			return nil, err
		}
		markerIndex = i
	}
	if markerIndex < 0 {
		return items, nil
	}
	marker := items[markerIndex]
	if marker.Runtime.Source.Kind != llm.SourceRunSummary || !marker.Runtime.Source.RetainCurrentInput || !marker.IsCompaction || marker.Role != RoleUser {
		return nil, fmt.Errorf("retained ranges require an anchored marker")
	}
	normalized, groups, err := groupTurnItems(items)
	if err != nil {
		return nil, err
	}
	position := markerIndex + 1
	previousKind, previousIndex := -1, uint64(0)
	for rangeIndex, retained := range ranges {
		if retained.Start.Validate() != nil || retained.End.Validate() != nil || retained.Start.Kind != retained.End.Kind || (retained.Start.Kind != llm.SourcePriorPublished && retained.Start.Kind != llm.SourceRunTranscript) {
			return nil, fmt.Errorf("invalid retained range %d", rangeIndex)
		}
		kind, start := sourceOrder(retained.Start)
		endKind, end := sourceOrder(retained.End)
		if kind != endKind || end < start || (rangeIndex > 0 && (kind < previousKind || (kind == previousKind && start <= previousIndex))) {
			return nil, fmt.Errorf("overlapping or reordered retained range %d", rangeIndex)
		}
		if !coveredSource(retained.Start, marker.Runtime.Source.Through) || !coveredSource(retained.End, marker.Runtime.Source.Through) || (kind == 1 && (start == 0 || retained.Start.RunID != marker.Runtime.Source.RunID || retained.End.RunID != marker.Runtime.Source.RunID)) {
			return nil, fmt.Errorf("retained range outside the covered source")
		}
		endPosition := -1
		for _, group := range groups {
			if group.start == position {
				endPosition = group.end
				break
			}
		}
		if endPosition < 0 || uint64(endPosition-position) != end-start+1 {
			return nil, fmt.Errorf("retained range does not match a complete exchange")
		}
		members := normalized[position:endPosition]
		if members[0].Runtime.Source != retained.Start || members[len(members)-1].Runtime.Source != retained.End {
			return nil, fmt.Errorf("retained range endpoints differ from exchange")
		}
		for i, item := range members {
			source := item.Runtime.Source
			memberKind, memberIndex := sourceOrder(source)
			if source.Validate() != nil || source.Kind != retained.Start.Kind || memberKind != kind || memberIndex != start+uint64(i) || (kind == 0 && (source.HistoryRevision != retained.Start.HistoryRevision || source.BranchID != retained.Start.BranchID)) || (kind == 1 && (source.RunID != retained.Start.RunID || source.PriorSourceCount != retained.Start.PriorSourceCount)) {
				return nil, fmt.Errorf("retained exchange has noncontiguous or foreign sources")
			}
		}
		digest, err := RetainedExchangeDigest(members)
		if err != nil || digest != retained.Digest {
			return nil, fmt.Errorf("retained exchange digest mismatch")
		}
		previousKind, previousIndex = kind, end
		position = endPosition
	}
	reduced := append([]TurnItem(nil), items[:markerIndex+1]...)
	reduced[markerIndex].Runtime.Source.RetainedSourceRanges = ""
	reduced = append(reduced, items[position:]...)
	return reduced, nil
}

func truncateRetainingImageExchanges(items []TurnItem, groups []itemGroup, currentIndex int, runID string, maxTokens int, estimator TokenEstimator) (CompactionResult, error) {
	fail := func(reason string) (CompactionResult, error) {
		return CompactionResult{Items: items}, imageCompactionBlocked(reason)
	}
	if currentIndex < 0 || items[currentIndex].Role != RoleUser || items[currentIndex].IsCompaction {
		return fail("the original current input cannot be retained")
	}
	marker := TurnItem{Role: RoleUser, Content: DurableTruncationMarker, IsCompaction: true, Runtime: llm.RuntimeMetadata{CompactionMarker: true, Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: runID, RetainCurrentInput: true}}}
	selected := make([]bool, len(groups))
	mandatory := estimateTokens([]TurnItem{marker}, estimator)
	protected := false
	for i, group := range groups {
		members := items[group.start:group.end]
		hasImage := false
		for _, item := range members {
			for _, block := range item.ContentBlocks {
				if block.Type != "text" && block.Type != "image_url" {
					return fail("an unsupported content modality cannot be omitted")
				}
			}
			hasImage = hasImage || itemHasImage(item)
		}
		if hasImage && group.start != currentIndex {
			protected = true
		}
		latestToolExchange := i == len(groups)-1 && members[0].Role == RoleAssistant && len(members[0].ToolCalls) > 0
		if group.system || group.start == currentIndex || hasImage || latestToolExchange {
			for _, item := range members {
				if group.system && item.Runtime.Source.Kind != llm.SourceEphemeral {
					return fail("source-backed system instruction cannot be omitted")
				}
			}
			selected[i] = true
			mandatory = saturatingTokenAdd(mandatory, estimateTokens(members, estimator))
		}
	}
	if !protected {
		return fail("no source-qualified image exchange can be retained")
	}
	if mandatory > maxTokens {
		return fail("the complete protected exchanges and omission marker exceed the budget")
	}
	remaining := maxTokens - mandatory
	for i := len(groups) - 1; i >= 0; i-- {
		if selected[i] {
			continue
		}
		members := items[groups[i].start:groups[i].end]
		if len(members) == 1 && members[0].Runtime.Source.Kind == llm.SourceRunSummary {
			continue
		}
		for _, item := range members {
			if item.Runtime.Source.Kind == llm.SourceEphemeral || item.Runtime.Source.Kind == llm.SourceNone || item.Runtime.Source.Kind == llm.SourcePendingRunInput {
				return fail("an unsupported source cannot be omitted")
			}
			for _, block := range item.ContentBlocks {
				if block.Type != "text" && block.Type != "image_url" {
					return fail("an unsupported content modality cannot be omitted")
				}
			}
		}
		cost := estimateTokens(members, estimator)
		if cost <= remaining {
			selected[i] = true
			remaining -= cost
		}
	}
	cut := -1
	for i, group := range groups {
		if !selected[i] && !(group.end == group.start+1 && items[group.start].Runtime.Source.Kind == llm.SourceRunSummary) {
			cut = group.end
		}
	}
	if cut <= currentIndex+1 {
		return fail("no covered text exchange can be safely omitted")
	}
	through, _, err := sourceThroughForPrefix(items[:cut])
	if err != nil {
		return CompactionResult{Items: items}, err
	}
	if through.TranscriptCount < 2 {
		return fail("the source boundary does not cover a later transcript item")
	}
	var previous llm.SourceThrough
	for _, item := range items {
		if item.Runtime.Source.Kind == llm.SourceRunSummary {
			previous = item.Runtime.Source.Through
		}
	}
	if through.PriorHistoryCount < previous.PriorHistoryCount || through.TranscriptCount < previous.TranscriptCount || through == previous {
		return fail("the omission boundary does not advance")
	}
	retained := make([]llm.RetainedSourceRange, 0)
	retainedItems := make([]TurnItem, 0)
	for i, group := range groups {
		if group.end > cut || !selected[i] || group.system || group.start == currentIndex {
			continue
		}
		members := items[group.start:group.end]
		if members[0].Runtime.Source.Kind == llm.SourceRunSummary {
			continue
		}
		digest, err := RetainedExchangeDigest(members)
		if err != nil {
			return CompactionResult{Items: items}, err
		}
		retained = append(retained, llm.RetainedSourceRange{Start: members[0].Runtime.Source, End: members[len(members)-1].Runtime.Source, Digest: digest})
		retainedItems = append(retainedItems, members...)
	}
	if len(retained) == 0 {
		return fail("no protected exchange lies inside the omitted prefix")
	}
	encoded, err := EncodeRetainedSourceRanges(retained)
	if err != nil {
		return CompactionResult{Items: items}, err
	}
	marker.Runtime.Source.Through = through
	marker.Runtime.Source.RetainedSourceRanges = encoded
	result := make([]TurnItem, 0, len(items))
	for _, group := range groups {
		if group.system {
			result = append(result, items[group.start:group.end]...)
		}
	}
	result = append(result, items[currentIndex], marker)
	result = append(result, retainedItems...)
	result = append(result, items[cut:]...)
	if err := ValidateRunRequestIntegrity(result); err != nil {
		return CompactionResult{Items: items}, err
	}
	if err := ValidateImageGroupRetention(items, result); err != nil {
		return CompactionResult{Items: items}, err
	}
	if estimateTokens(result, estimator) > maxTokens {
		return fail("the retained omission exceeds the budget")
	}
	return CompactionResult{Items: result, Strategy: StrategyTruncating, NewProjection: &NewProjection{Summary: DurableTruncationMarker, Through: through, RetainCurrentInput: true, RetainedSourceRanges: retained}}, nil
}

// SourceBoundaryForProjection maps a monotonically advancing source cursor to
// its exact complete-group boundary, including after an earlier omission.
func SourceBoundaryForProjection(items []TurnItem, through llm.SourceThrough) (int, error) {
	if err := ValidateRunRequestIntegrity(items); err != nil {
		return -1, err
	}
	_, groups, err := groupTurnItems(items)
	if err != nil {
		return -1, err
	}
	var previous llm.SourceThrough
	for _, item := range items {
		if item.Runtime.Source.Kind == llm.SourceRunSummary {
			previous = item.Runtime.Source.Through
		}
	}
	if through.PriorHistoryCount < previous.PriorHistoryCount || through.TranscriptCount < previous.TranscriptCount || through == previous {
		return -1, fmt.Errorf("source boundary does not advance")
	}
	for _, group := range groups {
		source := items[group.end-1].Runtime.Source
		match := (through.TranscriptCount > 0 && source.Kind == llm.SourceRunTranscript && source.TranscriptIndex+1 == through.TranscriptCount && source.PriorSourceCount == through.PriorHistoryCount) || (through.TranscriptCount == 0 && through.PriorHistoryCount > 0 && source.Kind == llm.SourcePriorPublished && source.PriorOrdinal+1 == through.PriorHistoryCount)
		if !match {
			continue
		}
		actual, _, err := sourceThroughForPrefix(items[:group.end])
		if err != nil {
			return -1, err
		}
		if actual == through {
			return group.end - 1, nil
		}
	}
	return -1, fmt.Errorf("source boundary does not end at a complete exchange")
}

// ValidateRetainedOmission proves that the new typed omission is exactly a
// partition of the original complete groups, retaining declared exchanges
// unchanged and omitting only supported text-only groups.
func ValidateRetainedOmission(before, after []TurnItem, projection *NewProjection) error {
	if projection == nil || projection.Summary != DurableTruncationMarker || !projection.RetainCurrentInput || len(projection.RetainedSourceRanges) == 0 {
		return fmt.Errorf("invalid retained omission projection")
	}
	if err := ValidateCompactionEligible(before); err != nil {
		return err
	}
	for _, item := range before {
		for _, block := range item.ContentBlocks {
			if block.Type != "text" && block.Type != "image_url" {
				return fmt.Errorf("retained omission contains an unsupported content modality")
			}
		}
	}
	if err := ValidateRunRequestIntegrity(after); err != nil {
		return err
	}
	if err := ValidateImageGroupRetention(before, after); err != nil {
		return err
	}
	boundary, err := SourceBoundaryForProjection(before, projection.Through)
	if err != nil {
		return err
	}
	encoded, err := EncodeRetainedSourceRanges(projection.RetainedSourceRanges)
	if err != nil {
		return err
	}
	_, groups, err := groupTurnItems(before)
	if err != nil {
		return err
	}
	var systems []TurnItem
	var anchor *TurnItem
	var retained []TurnItem
	rangeIndex, omitted := 0, 0
	for groupIndex, group := range groups {
		members := before[group.start:group.end]
		if group.system {
			systems = append(systems, members...)
			continue
		}
		first := members[0].Runtime.Source
		if first.Kind == llm.SourceRunTranscript && first.TranscriptIndex == 0 {
			if len(members) != 1 || anchor != nil {
				return fmt.Errorf("invalid retained current input")
			}
			copy := members[0]
			anchor = &copy
			continue
		}
		if group.start > boundary || first.Kind == llm.SourceRunSummary {
			continue
		}
		if rangeIndex < len(projection.RetainedSourceRanges) && first == projection.RetainedSourceRanges[rangeIndex].Start {
			r := projection.RetainedSourceRanges[rangeIndex]
			digest, digestErr := RetainedExchangeDigest(members)
			if digestErr != nil || members[len(members)-1].Runtime.Source != r.End || digest != r.Digest {
				return fmt.Errorf("retained range does not match the source exchange")
			}
			retained = append(retained, members...)
			rangeIndex++
			continue
		}
		if groupIndex == len(groups)-1 && members[0].Role == RoleAssistant && len(members[0].ToolCalls) > 0 {
			return fmt.Errorf("omission removes the latest complete tool exchange")
		}
		for _, item := range members {
			if item.Runtime.Source.Kind != llm.SourcePriorPublished && item.Runtime.Source.Kind != llm.SourceRunTranscript {
				return fmt.Errorf("omission includes unsupported source")
			}
			for _, block := range item.ContentBlocks {
				if block.Type != "text" {
					return fmt.Errorf("omission includes a protected content modality")
				}
			}
		}
		omitted++
	}
	if anchor == nil || rangeIndex != len(projection.RetainedSourceRanges) || omitted == 0 {
		return fmt.Errorf("retained ranges do not partition the source")
	}
	marker := TurnItem{Role: RoleUser, Content: projection.Summary, IsCompaction: true, Runtime: llm.RuntimeMetadata{CompactionMarker: true, Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: anchor.Runtime.Source.RunID, Through: projection.Through, RetainCurrentInput: true, RetainedSourceRanges: encoded}}}
	expected := append(systems, *anchor, marker)
	expected = append(expected, retained...)
	expected = append(expected, before[boundary+1:]...)
	if len(expected) != len(after) {
		return fmt.Errorf("retained omission changes the source partition")
	}
	for i := range expected {
		actual := after[i]
		expected[i].EstimatedTokens, actual.EstimatedTokens = 0, 0
		if !reflect.DeepEqual(expected[i], actual) {
			return fmt.Errorf("retained omission changes source item %d", i)
		}
	}
	return nil
}
