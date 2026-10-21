//go:build !headless

package tui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pengjunchen/go-cli/internal/core"
)

const maxCanonicalTranscriptPreviewBytes = maxStreamingRenderBytes

type canonicalToolProgressState uint8

const (
	canonicalToolProgressRunning canonicalToolProgressState = iota + 1
	canonicalToolProgressCompleted
)

type liveToolOccurrence struct {
	Key                      ToolOccurrenceKey
	CanonicalTranscriptIndex uint64
}

func (m *teaModel) installTranscriptSnapshotLocked(snapshot CanonicalTranscriptSnapshot) error {
	m.ensureCanonicalToolProgressLocked()
	rows := canonicalTranscriptViewportRows(snapshot)
	assistantKeys, toolEntryKeys := canonicalTranscriptOccurrenceIndexes(snapshot)
	m.canonicalAssistantKeys = assistantKeys
	m.canonicalToolEntryKeys = toolEntryKeys
	for occurrence := range toolEntryKeys {
		m.canonicalToolProgress[occurrence] = canonicalToolProgressCompleted
	}
	matchedLive := make([]AssistantOccurrenceKey, 0)
	matchedKeys := make(map[string]struct{})
	for occurrence, key := range m.liveAssistantRows {
		if _, exists := assistantKeys[occurrence]; exists {
			matchedLive = append(matchedLive, occurrence)
			matchedKeys[key] = struct{}{}
		}
	}
	matchedTools := make([]liveToolOccurrence, 0)
	for occurrence := range m.liveToolRows {
		if _, exists := toolEntryKeys[occurrence]; exists {
			matchedTools = append(matchedTools, occurrence)
			matchedKeys[m.liveToolRows[occurrence]] = struct{}{}
		}
	}
	applyLiveToolMetadataToCanonicalRows(snapshot, rows, m.liveToolTruncation, m.liveToolReceipts)
	for _, key := range m.liveRowOrder {
		if row, ok := m.liveTranscriptRows[key]; ok {
			if _, matched := matchedKeys[key]; !matched {
				rows = append(rows, row)
			}
		}
	}
	if m.transcriptViewport == nil {
		m.transcriptViewport = newTranscriptViewport(m.width, 0)
	}
	if m.toolTranscriptViewport == nil {
		m.toolTranscriptViewport = newTranscriptViewport(m.width, 0)
	}
	toolRows := filterTranscriptToolRows(rows, true)
	mainRows := mainTranscriptRows(rows, m.width >= splitWidthThreshold)
	loadedScope := canonicalTranscriptLoadedScope(snapshot)
	m.transcriptLoadedScope = loadedScope
	if err := m.transcriptViewport.SetRows(mainRows, loadedScope); err != nil {
		return err
	}
	if err := m.toolTranscriptViewport.SetRows(toolRows, loadedScope); err != nil {
		return err
	}
	for _, occurrence := range matchedLive {
		delete(m.liveTranscriptRows, m.liveAssistantRows[occurrence])
		delete(m.liveAssistantRows, occurrence)
		delete(m.liveAssistantRenderers, occurrence)
	}
	for _, occurrence := range matchedTools {
		delete(m.liveTranscriptRows, m.liveToolRows[occurrence])
		delete(m.liveToolRows, occurrence)
		delete(m.liveToolRenderers, occurrence)
		delete(m.liveToolTruncation, occurrence)
		delete(m.liveToolReceipts, occurrence)
	}
	remainingOrder := m.liveRowOrder[:0]
	for _, key := range m.liveRowOrder {
		if _, exists := m.liveTranscriptRows[key]; exists {
			remainingOrder = append(remainingOrder, key)
		}
	}
	for index := len(remainingOrder); index < len(m.liveRowOrder); index++ {
		m.liveRowOrder[index] = ""
	}
	m.liveRowOrder = remainingOrder
	m.hasTranscriptToolRows = len(toolRows) > 0
	m.transcriptSplitActive = m.width >= splitWidthThreshold
	m.transcriptSnapshot = snapshot.Clone()
	m.hasTranscriptSnapshot = true
	m.spinnerActive = m.hasRunningToolLocked()
	return nil
}

func (m *teaModel) ensureCanonicalToolProgressLocked() {
	if m.canonicalToolProgress == nil {
		m.canonicalToolProgress = make(map[liveToolOccurrence]canonicalToolProgressState)
	}
	if m.quiescentToolRuns == nil {
		m.quiescentToolRuns = make(map[string]struct{})
	}
}

// reconcileCanonicalToolProgressLocked closes only the presentation state for
// one quiescent run. It does not establish or modify resource completion proof.
func (m *teaModel) reconcileCanonicalToolProgressLocked(observation RunObservation) bool {
	if observation.RunID == "" || observation.ExecutionState != core.RunExecutionQuiescent {
		return false
	}
	m.ensureCanonicalToolProgressLocked()
	_, alreadyQuiescent := m.quiescentToolRuns[observation.RunID]
	m.quiescentToolRuns[observation.RunID] = struct{}{}
	changed := !alreadyQuiescent
	for occurrence := range m.canonicalToolProgress {
		if occurrence.Key.RunID == observation.RunID {
			delete(m.canonicalToolProgress, occurrence)
			changed = true
		}
	}
	m.pruneQuiescentToolRunsLocked()
	wasActive := m.spinnerActive
	m.spinnerActive = m.hasRunningToolLocked()
	return changed || wasActive != m.spinnerActive
}

func (m *teaModel) pruneQuiescentToolRunsLocked() {
	if m.expectedRunID == "" {
		return
	}
	for runID := range m.quiescentToolRuns {
		if runID != m.expectedRunID {
			delete(m.quiescentToolRuns, runID)
		}
	}
}

func (m *teaModel) recordCanonicalToolProgressLocked(event AgentEvent, occurrence liveToolOccurrence) {
	m.ensureCanonicalToolProgressLocked()
	m.pruneQuiescentToolRunsLocked()
	key := occurrence.Key
	if m.expectedRunID != "" && key.RunID != m.expectedRunID {
		return
	}
	if !m.canonicalToolOccurrenceFitsSnapshotLocked(occurrence) {
		return
	}
	if m.runObservation.RunID == key.RunID && m.runObservation.ExecutionState == core.RunExecutionQuiescent {
		m.quiescentToolRuns[key.RunID] = struct{}{}
	}
	if _, quiescent := m.quiescentToolRuns[key.RunID]; quiescent {
		return
	}

	switch event.ContentType {
	case ContentTypeToolCall:
		if event.Type != "" && event.Type != "tool_call" {
			return
		}
		if m.canonicalToolProgress[occurrence] == canonicalToolProgressCompleted {
			return
		}
		m.canonicalToolProgress[occurrence] = canonicalToolProgressRunning
	case ContentTypeToolResult:
		if event.Type != "" && event.Type != "tool_result" {
			return
		}
		m.canonicalToolProgress[occurrence] = canonicalToolProgressCompleted
	default:
		return
	}
	m.spinnerActive = m.hasRunningToolLocked()
}

func (m *teaModel) canonicalToolOccurrenceFitsSnapshotLocked(occurrence liveToolOccurrence) bool {
	coordinateFound := false
	coordinateAmbiguous := false
	keyFound := false
	for _, row := range m.transcriptSnapshot.Rows {
		if row.Kind != TranscriptRowAssistant || row.AssistantOccurrence == nil ||
			row.AssistantOccurrence.RunID != occurrence.Key.RunID {
			continue
		}
		atCoordinate := row.AssistantOccurrence.CanonicalTranscriptIndex == occurrence.CanonicalTranscriptIndex
		if atCoordinate {
			if coordinateFound {
				coordinateAmbiguous = true
			}
			coordinateFound = true
		}
		for _, call := range row.Calls {
			if call.Key != occurrence.Key {
				continue
			}
			if !atCoordinate || keyFound {
				return false
			}
			keyFound = true
		}
	}
	if coordinateAmbiguous {
		return false
	}
	if coordinateFound {
		return keyFound
	}
	// The live run may emit an exact, fully identified tool event before the
	// durable snapshot refresh includes that assistant coordinate. Permit that
	// first observation unless the snapshot associates the full key elsewhere.
	return !keyFound
}

func canonicalTranscriptOccurrenceIndexes(snapshot CanonicalTranscriptSnapshot) (
	map[AssistantOccurrenceKey]struct{},
	map[liveToolOccurrence]string,
) {
	assistantKeys := make(map[AssistantOccurrenceKey]struct{})
	toolAssistantIndices := make(map[ToolOccurrenceKey]uint64)
	ambiguousToolOccurrences := make(map[ToolOccurrenceKey]struct{})
	for _, row := range snapshot.Rows {
		if row.Kind != TranscriptRowAssistant || row.AssistantOccurrence == nil {
			continue
		}
		assistantKeys[*row.AssistantOccurrence] = struct{}{}
		for _, call := range row.Calls {
			if _, duplicate := toolAssistantIndices[call.Key]; duplicate {
				ambiguousToolOccurrences[call.Key] = struct{}{}
				continue
			}
			toolAssistantIndices[call.Key] = row.AssistantOccurrence.CanonicalTranscriptIndex
		}
	}
	toolEntryKeys := make(map[liveToolOccurrence]string)
	for _, row := range snapshot.Rows {
		if row.Kind != TranscriptRowTool || row.Occurrence == nil {
			continue
		}
		if _, ambiguous := ambiguousToolOccurrences[*row.Occurrence]; ambiguous {
			continue
		}
		index, exists := toolAssistantIndices[*row.Occurrence]
		if !exists {
			continue
		}
		occurrence := liveToolOccurrence{Key: *row.Occurrence, CanonicalTranscriptIndex: index}
		if _, duplicate := toolEntryKeys[occurrence]; duplicate {
			delete(toolEntryKeys, occurrence)
			continue
		}
		toolEntryKeys[occurrence] = row.Key.EntryID
	}
	return assistantKeys, toolEntryKeys
}

func mainTranscriptRows(rows []transcriptViewportRow, split bool) []transcriptViewportRow {
	if !split {
		return rows
	}
	return filterTranscriptToolRows(rows, false)
}

func filterTranscriptToolRows(rows []transcriptViewportRow, toolRows bool) []transcriptViewportRow {
	filtered := make([]transcriptViewportRow, 0, len(rows))
	for _, row := range rows {
		if row.toolPane == toolRows {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (m *teaModel) rebuildTranscriptPaneLayoutLocked() error {
	if !m.hasTranscriptSnapshot || m.transcriptViewport == nil || m.toolTranscriptViewport == nil {
		return nil
	}
	rows := canonicalTranscriptViewportRows(m.transcriptSnapshot)
	applyLiveToolMetadataToCanonicalRows(m.transcriptSnapshot, rows, m.liveToolTruncation, m.liveToolReceipts)
	for _, key := range m.liveRowOrder {
		if row, ok := m.liveTranscriptRows[key]; ok {
			rows = append(rows, row)
		}
	}
	loadedScope := m.transcriptLoadedScope
	if err := m.transcriptViewport.SetRows(mainTranscriptRows(rows, m.width >= splitWidthThreshold), loadedScope); err != nil {
		return err
	}
	if err := m.toolTranscriptViewport.SetRows(filterTranscriptToolRows(rows, true), loadedScope); err != nil {
		return err
	}
	m.hasTranscriptToolRows = m.toolTranscriptViewportRowCount() > 0
	m.transcriptSplitActive = m.width >= splitWidthThreshold
	return nil
}

func applyLiveToolMetadataToCanonicalRows(
	snapshot CanonicalTranscriptSnapshot,
	rows []transcriptViewportRow,
	facts map[liveToolOccurrence]*bool,
	receipts map[liveToolOccurrence]string,
) {
	for occurrence := range facts {
		applyLiveToolMetadataToCanonicalRow(snapshot, rows, occurrence, facts[occurrence], receipts[occurrence])
	}
	for occurrence, receipt := range receipts {
		if _, hasFact := facts[occurrence]; !hasFact {
			applyLiveToolMetadataToCanonicalRow(snapshot, rows, occurrence, nil, receipt)
		}
	}
}

func applyLiveToolMetadataToCanonicalRow(
	snapshot CanonicalTranscriptSnapshot,
	rows []transcriptViewportRow,
	occurrence liveToolOccurrence,
	fact *bool,
	receipt string,
) {
	if !canonicalTranscriptHasTool(snapshot, occurrence) {
		return
	}
	for index, source := range snapshot.Rows {
		if source.Kind != TranscriptRowTool || source.Occurrence == nil ||
			source.Occurrence.RunID != occurrence.Key.RunID ||
			source.Occurrence.AssistantTranscriptOrdinal != occurrence.Key.AssistantTranscriptOrdinal ||
			source.Occurrence.ToolCallID != occurrence.Key.ToolCallID {
			continue
		}
		if rows[index].sourceTruncated == nil && fact != nil {
			value := *fact
			rows[index].sourceTruncated = &value
		}
		if receipt != "" {
			rows[index].receiptOutcome = receipt
		}
		composeTranscriptViewportRowText(&rows[index])
		return
	}
}

func (m *teaModel) toolTranscriptViewportRowCount() int {
	if m.toolTranscriptViewport == nil {
		return 0
	}
	return len(m.toolTranscriptViewport.rows)
}

func canonicalTranscriptHasTool(snapshot CanonicalTranscriptSnapshot, occurrence liveToolOccurrence) bool {
	for _, row := range snapshot.Rows {
		if row.Kind != TranscriptRowAssistant || row.AssistantOccurrence == nil ||
			row.AssistantOccurrence.RunID != occurrence.Key.RunID ||
			row.AssistantOccurrence.CanonicalTranscriptIndex != occurrence.CanonicalTranscriptIndex {
			continue
		}
		for _, call := range row.Calls {
			if call.Key == occurrence.Key {
				return true
			}
		}
	}
	return false
}

func composeTranscriptViewportRowText(row *transcriptViewportRow) {
	if row == nil {
		return
	}
	base := row.baseText
	if base == "" {
		base = row.text
	}
	row.text = base
	if row.baseLines != nil {
		row.lines = append([]string(nil), row.baseLines...)
	}
	appendLabel := func(label string) {
		row.text += "\n" + label
		if row.baseLines != nil {
			row.lines = append(row.lines, label)
		}
	}
	if row.sourceTruncated != nil {
		if *row.sourceTruncated {
			appendLabel("[source output truncated]")
		} else {
			appendLabel("[source output not truncated]")
		}
	}
	if row.receiptOutcome != "" {
		appendLabel("[receipt: " + safeTerminalText(row.receiptOutcome) + "]")
	}
	if row.viewTruncated {
		appendLabel("[view truncated]")
	}
}

func canonicalTranscriptViewportRows(snapshot CanonicalTranscriptSnapshot) []transcriptViewportRow {
	rows := make([]transcriptViewportRow, 0, len(snapshot.Rows))
	for index, source := range snapshot.Rows {
		content := source.Message.Content
		if content == "" && source.Compaction != nil {
			content = source.Compaction.Summary
		}
		previewLines, viewTruncated := canonicalTranscriptPreviewLines(content)
		header := transcriptRowHeader(source)
		baseLines := make([]string, 0, len(previewLines)+1)
		baseLines = append(baseLines, header)
		baseLines = append(baseLines, previewLines...)
		revision := source.SourceSeq
		if revision == 0 {
			revision = uint64(index + 1)
		}
		var sourceTruncated *bool
		if source.SourceTruncated != nil {
			value := *source.SourceTruncated
			sourceTruncated = &value
		}
		rows = append(rows, transcriptViewportRow{
			key:             source.Key.EntryID,
			baseText:        header,
			baseLines:       baseLines,
			searchText:      content,
			revision:        revision,
			expanded:        true,
			viewTruncated:   viewTruncated,
			sourceTruncated: sourceTruncated,
			receiptOutcome:  source.Message.ReceiptOutcome,
			toolPane:        source.Kind == TranscriptRowTool,
		})
		composeTranscriptViewportRowText(&rows[len(rows)-1])
	}
	return rows
}

func canonicalTranscriptPreviewLines(source string) ([]string, bool) {
	if len(source) <= maxCanonicalTranscriptPreviewBytes {
		if source == "" {
			return nil, false
		}
		preview := safeTerminalText(source)
		if containsTerminalControl(source) || !utf8.ValidString(source) {
			return []string{preview}, false
		}
		return strings.Split(preview, "\n"), false
	}
	start := len(source) - maxCanonicalTranscriptPreviewBytes
	for start < len(source) && !utf8.RuneStart(source[start]) {
		start++
	}
	preview := safeTerminalText(source[start:])
	lines := []string{"[earlier content hidden]"}
	if containsTerminalControl(source[start:]) || !utf8.ValidString(source[start:]) {
		lines = append(lines, preview)
	} else {
		lines = append(lines, strings.Split(preview, "\n")...)
	}
	return lines, true
}

func transcriptRowHeader(row CanonicalTranscriptRow) string {
	switch row.Kind {
	case TranscriptRowUser:
		return "[user]"
	case TranscriptRowAssistant:
		return "[assistant]"
	case TranscriptRowSystem:
		return "[system]"
	case TranscriptRowCompaction:
		if row.Compaction != nil && row.Compaction.CompactedThroughEntryID != "" {
			return "[compaction through " + safeTerminalText(row.Compaction.CompactedThroughEntryID) + "]"
		}
		return "[compaction]"
	case TranscriptRowTool:
		name := row.Message.ToolName
		if name == "" {
			name = "tool"
		}
		return "[tool: " + safeTerminalText(name) + "]"
	default:
		return "[transcript]"
	}
}

func boundedCanonicalTranscriptPreview(source string) (string, bool) {
	if len(source) <= maxCanonicalTranscriptPreviewBytes {
		return safeTerminalText(source), false
	}
	start := len(source) - maxCanonicalTranscriptPreviewBytes
	for start < len(source) && !utf8.RuneStart(source[start]) {
		start++
	}
	preview := "[earlier content hidden]\n" + safeTerminalText(source[start:])
	return preview, true
}

func canonicalTranscriptLoadedScope(snapshot CanonicalTranscriptSnapshot) string {
	sessionID, _ := transcriptViewportSafeText(snapshot.PhysicalSessionID)
	branchID, _ := transcriptViewportSafeText(snapshot.BranchID)
	return fmt.Sprintf("Session: %s | Branch: %s | Loaded rows: %d | loaded projection only",
		sessionID,
		branchID,
		len(snapshot.Rows),
	)
}

func (m *teaModel) searchTranscriptLocked(query string) (string, bool) {
	if !m.hasTranscriptSnapshot || len(m.transcriptSnapshot.Rows) == 0 {
		return "", false
	}
	safeQuery, _ := transcriptViewportSafeText(query)
	needle := strings.ToLower(safeQuery)
	if needle == "" {
		return "", false
	}
	start := 0
	if m.transcriptSearchAfter != "" {
		for index, row := range m.transcriptSnapshot.Rows {
			if row.Key.EntryID == m.transcriptSearchAfter {
				start = (index + 1) % len(m.transcriptSnapshot.Rows)
				break
			}
		}
	}
	for offset := 0; offset < len(m.transcriptSnapshot.Rows); offset++ {
		index := (start + offset) % len(m.transcriptSnapshot.Rows)
		row := m.transcriptSnapshot.Rows[index]
		source := row.Message.Content
		if source == "" && row.Compaction != nil {
			source = row.Compaction.Summary
		}
		safeSource, _ := transcriptViewportSafeText(source)
		if !strings.Contains(strings.ToLower(safeSource), needle) {
			continue
		}
		key := row.Key.EntryID
		m.transcriptSearchAfter = key
		m.selectedTranscriptKey = key
		m.selectedTranscriptTool = row.Kind == TranscriptRowTool && m.transcriptSplitActive
		viewport := m.selectedTranscriptViewportLocked()
		viewport.SelectSearchHit(key)
		return key, true
	}
	return "", false
}

func (m *teaModel) selectedTranscriptViewportLocked() *transcriptViewport {
	if m.selectedTranscriptTool && m.transcriptSplitActive && m.toolTranscriptViewport != nil {
		return m.toolTranscriptViewport
	}
	return m.transcriptViewport
}

func (m *teaModel) handleCanonicalAuxiliaryEvent(ctx context.Context, event AgentEvent) tea.Cmd {
	contentType := event.ContentType
	renderer, ok := m.reg.Get(contentType)
	if !ok || renderer == nil {
		renderer, _ = m.reg.Get(DefaultContentType)
	}
	content := event.Content
	if renderer != nil {
		theme := Theme(DarkTheme{})
		if m.themeMgr != nil {
			theme = m.themeMgr.Get()
		}
		content = renderer.Render(ctx, event.Content, RenderOpts{
			Theme: theme, Width: m.renderWidth(), ContentType: contentType, Stream: event.Stream,
		})
	}
	content, _ = boundedCanonicalTranscriptPreview(safeTerminalText(content))
	if content == "" {
		return nil
	}
	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	m.liveStatus = content
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
	return nil
}

func (m *teaModel) handleTranscriptSearchKeyLocked(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.transcriptSearchMode = false
	case tea.KeyEnter:
		m.searchTranscriptLocked(m.transcriptSearchInput)
	case tea.KeyBackspace:
		m.transcriptSearchInput = m.transcriptSearchInput[:moveInputCursorLeft(m.transcriptSearchInput, len(m.transcriptSearchInput))]
	case tea.KeyRunes:
		m.transcriptSearchInput += string(msg.Runes)
	case tea.KeySpace:
		m.transcriptSearchInput += " "
	}
	return nil
}

func (m *teaModel) renderCanonicalTranscriptViewLocked() string {
	footer := m.renderFooterLocked()
	bodyRows := max(0, m.height-transcriptFooterRows(footer, m.width))
	if bodyRows == 0 {
		return footer
	}

	scope := m.transcriptLoadedScope
	viewportHeight := max(0, bodyRows-1)
	width := m.width
	if width <= 0 {
		width = 80
	}
	var view strings.Builder
	view.WriteString(scope)
	if width >= splitWidthThreshold && m.hasTranscriptToolRows {
		leftWidth := width / 2
		rightWidth := width - leftWidth - 1
		m.transcriptViewport.Resize(leftWidth, viewportHeight)
		m.toolTranscriptViewport.Resize(rightWidth, viewportHeight)
		left := transcriptViewportWindowText(m.transcriptViewport.Window())
		right := transcriptViewportWindowText(m.toolTranscriptViewport.Window())
		left = lipgloss.NewStyle().Width(leftWidth).Height(viewportHeight).Render(left)
		right = lipgloss.NewStyle().Width(rightWidth).Height(viewportHeight).Render(right)
		if viewportHeight > 0 {
			view.WriteByte('\n')
			view.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left, "│", right))
		}
	} else {
		m.transcriptViewport.Resize(width, viewportHeight)
		if windowText := transcriptViewportWindowText(m.transcriptViewport.Window()); windowText != "" {
			view.WriteByte('\n')
			view.WriteString(windowText)
		}
	}
	view.WriteString(footer)
	return view.String()
}

func transcriptViewportWindowText(window transcriptViewportWindow) string {
	if len(window.lines) == 0 {
		return ""
	}
	var text strings.Builder
	for index, line := range window.lines {
		if index > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(line.text)
	}
	return text.String()
}

func transcriptFooterRows(footer string, width int) int {
	if footer == "" {
		return 0
	}
	if width <= 0 {
		return strings.Count(footer, "\n") + 1
	}
	return lipgloss.Height(lipgloss.NewStyle().Width(width).Render(footer))
}

func (m *teaModel) handleCanonicalTranscriptEvent(event AgentEvent) tea.Cmd {
	if event.ContentType == ContentTypeToolCall || event.ContentType == ContentTypeToolOutput || event.ContentType == ContentTypeToolResult {
		return m.handleCanonicalToolEvent(event)
	}
	isAssistant := event.Type == "message" || event.ContentType == ContentTypeAssistant
	if !isAssistant || event.RunID == "" || event.CanonicalTranscriptIndex == nil {
		return nil
	}
	occurrence := AssistantOccurrenceKey{RunID: event.RunID, CanonicalTranscriptIndex: *event.CanonicalTranscriptIndex}
	notify := m.shouldNotify()
	var view string
	m.mu.Lock()
	if _, exists := m.canonicalAssistantKeys[occurrence]; exists {
		m.mu.Unlock()
		return nil
	}
	renderer := m.liveAssistantRenderers[occurrence]
	if renderer == nil {
		renderer = NewStreamingMarkdownRenderer(NewMarkdownRenderer())
		m.liveAssistantRenderers[occurrence] = renderer
	}
	if event.Incremental || m.liveAssistantRows[occurrence] == "" {
		lineWidth := m.renderWidth()
		window := renderer.AppendChunkLines(context.Background(), event.Content, RenderOpts{Width: lineWidth, ContentType: ContentTypeAssistant})
		key := m.liveAssistantRows[occurrence]
		row := m.liveTranscriptRows[key]
		if key == "" {
			m.liveRowSequence++
			key = fmt.Sprintf("\x00live-assistant-%d", m.liveRowSequence)
			m.liveAssistantRows[occurrence] = key
			m.liveRowOrder = append(m.liveRowOrder, key)
		}
		row.key = key
		row.revision++
		row.expanded = true
		row.excludeFromSearch = true
		row.viewTruncated = row.viewTruncated || window.HasHiddenPrefix
		row.baseText = "[assistant · live]"
		row.text = row.baseText
		row.lineWidth = lineWidth
		row.lines = liveTranscriptRowLines(row.baseText, window, nil, "", row.viewTruncated)
		if _, exists := m.liveTranscriptRows[key]; exists {
			if err := m.transcriptViewport.updateRow(row); err != nil {
				slog.Debug("tui.transcript.update_live_row_failed", "err", err)
			}
		} else {
			if err := m.transcriptViewport.AppendRow(row); err != nil {
				slog.Debug("tui.transcript.append_live_row_failed", "err", err)
			}
		}
		m.liveTranscriptRows[key] = row
	}
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
	return nil
}

func (m *teaModel) handleCanonicalToolEvent(event AgentEvent) tea.Cmd {
	if event.RunID == "" || event.CanonicalTranscriptIndex == nil || event.AssistantTranscriptOrdinal == nil || event.ToolCallID == "" {
		return nil
	}
	occurrence := liveToolOccurrence{
		Key: ToolOccurrenceKey{
			RunID:                      event.RunID,
			AssistantTranscriptOrdinal: *event.AssistantTranscriptOrdinal,
			ToolCallID:                 event.ToolCallID,
		},
		CanonicalTranscriptIndex: *event.CanonicalTranscriptIndex,
	}
	notify := m.shouldNotify()
	var view string
	m.mu.Lock()
	m.recordCanonicalToolProgressLocked(event, occurrence)
	entryKey, canonicalMatch := m.canonicalToolEntryKeys[occurrence]
	if event.SourceTruncated != nil {
		value := *event.SourceTruncated
		m.liveToolTruncation[occurrence] = &value
	}
	if event.ReceiptOutcome != "" {
		m.liveToolReceipts[occurrence] = event.ReceiptOutcome
	}
	if canonicalMatch {
		if index, exists := m.toolTranscriptViewport.indexByKey[entryKey]; exists {
			row := m.toolTranscriptViewport.rows[index]
			if event.SourceTruncated != nil {
				value := *event.SourceTruncated
				row.sourceTruncated = &value
			}
			row.receiptOutcome = m.liveToolReceipts[occurrence]
			row.revision++
			composeTranscriptViewportRowText(&row)
			m.updateToolTranscriptRowLocked(row)
		}
	}
	if !canonicalMatch {
		m.updateLiveToolRowLocked(event, occurrence)
	}
	if notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
	return nil
}

func (m *teaModel) updateLiveToolRowLocked(event AgentEvent, occurrence liveToolOccurrence) {
	key := m.liveToolRows[occurrence]
	row := m.liveTranscriptRows[key]
	if key == "" {
		m.liveRowSequence++
		key = fmt.Sprintf("\x00live-tool-%d", m.liveRowSequence)
		m.liveToolRows[occurrence] = key
		m.liveRowOrder = append(m.liveRowOrder, key)
	}
	row.key = key
	row.revision++
	row.expanded = true
	row.excludeFromSearch = true
	row.toolPane = true
	row.sourceTruncated = m.liveToolTruncation[occurrence]
	var body StreamRenderLines
	lineWidth := m.renderWidth()
	switch event.ContentType {
	case ContentTypeToolCall:
		content := safeTerminalText(event.Content)
		if content != "" {
			body.Lines = strings.Split(content, "\n")
		}
	case ContentTypeToolOutput:
		renderer := m.liveToolRenderers[occurrence]
		if renderer == nil {
			renderer = NewStreamingMarkdownRenderer(NewMarkdownRenderer())
			m.liveToolRenderers[occurrence] = renderer
		}
		window := renderer.AppendChunkLines(context.Background(), event.Content, RenderOpts{Width: lineWidth, ContentType: ContentTypeToolOutput, Stream: event.Stream})
		body = window
		row.viewTruncated = row.viewTruncated || window.HasHiddenPrefix
	case ContentTypeToolResult:
		if renderer := m.liveToolRenderers[occurrence]; renderer != nil {
			window := renderer.WindowLines()
			body = window
			row.viewTruncated = row.viewTruncated || window.HasHiddenPrefix
		}
		if event.ReceiptOutcome != "" {
			m.liveToolReceipts[occurrence] = event.ReceiptOutcome
		}
	default:
		return
	}
	row.baseText = "[tool " + safeTerminalText(event.ToolCallID) + "]"
	if event.Stream != "" {
		row.baseText += " [" + safeTerminalText(event.Stream) + "]"
	}
	row.receiptOutcome = m.liveToolReceipts[occurrence]
	row.text = row.baseText
	row.lineWidth = lineWidth
	row.lines = liveTranscriptRowLines(row.baseText, body, row.sourceTruncated, row.receiptOutcome, row.viewTruncated)
	m.upsertToolTranscriptRowLocked(row)
	m.liveTranscriptRows[key] = row
}

func liveTranscriptRowLines(header string, body StreamRenderLines, sourceTruncated *bool, receipt string, viewTruncated bool) []string {
	lineCount := 1 + len(body.Lines)
	if body.HasCurrentLine {
		lineCount++
	}
	if body.HasNextLine {
		lineCount++
	}
	if sourceTruncated != nil {
		lineCount++
	}
	if receipt != "" {
		lineCount++
	}
	if viewTruncated {
		lineCount++
	}
	lines := make([]string, 0, lineCount)
	lines = append(lines, header)
	lines = append(lines, body.Lines...)
	if body.HasCurrentLine {
		lines = append(lines, body.CurrentLine)
	}
	if body.HasNextLine {
		lines = append(lines, body.NextLine)
	}
	if sourceTruncated != nil {
		if *sourceTruncated {
			lines = append(lines, "[source output truncated]")
		} else {
			lines = append(lines, "[source output not truncated]")
		}
	}
	if receipt != "" {
		lines = append(lines, "[receipt: "+safeTerminalText(receipt)+"]")
	}
	if viewTruncated {
		lines = append(lines, "[view truncated]")
	}
	return lines
}

func (m *teaModel) updateToolTranscriptRowLocked(row transcriptViewportRow) {
	if m.toolTranscriptViewport != nil {
		if err := m.toolTranscriptViewport.updateRow(row); err != nil {
			slog.Debug("tui.transcript.update_tool_pane_failed", "err", err)
		}
	}
	if !m.transcriptSplitActive && m.transcriptViewport != nil {
		if err := m.transcriptViewport.updateRow(row); err != nil {
			slog.Debug("tui.transcript.update_main_tool_failed", "err", err)
		}
	}
}

func (m *teaModel) upsertToolTranscriptRowLocked(row transcriptViewportRow) {
	m.hasTranscriptToolRows = true
	if m.toolTranscriptViewport != nil {
		if _, exists := m.toolTranscriptViewport.indexByKey[row.key]; exists {
			if err := m.toolTranscriptViewport.updateRow(row); err != nil {
				slog.Debug("tui.transcript.update_tool_pane_failed", "err", err)
			}
		} else if err := m.toolTranscriptViewport.AppendRow(row); err != nil {
			slog.Debug("tui.transcript.append_tool_pane_failed", "err", err)
		}
	}
	if !m.transcriptSplitActive && m.transcriptViewport != nil {
		if _, exists := m.transcriptViewport.indexByKey[row.key]; exists {
			if err := m.transcriptViewport.updateRow(row); err != nil {
				slog.Debug("tui.transcript.update_main_tool_failed", "err", err)
			}
		} else if err := m.transcriptViewport.AppendRow(row); err != nil {
			slog.Debug("tui.transcript.append_main_tool_failed", "err", err)
		}
	}
}
