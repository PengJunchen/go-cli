package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const transcriptViewportOverscanLines = 2

var (
	errTranscriptViewportEmptyKey      = errors.New("transcript viewport row key is empty")
	errTranscriptViewportDuplicateKey  = errors.New("transcript viewport row key is duplicated")
	errTranscriptViewportScopeRequired = errors.New("transcript viewport loaded scope is required")
	errTranscriptViewportStaleRevision = errors.New("transcript viewport row revision is stale")
	errTranscriptViewportMissingRow    = errors.New("transcript viewport row key is not loaded")
)

// transcriptViewportRow is an immutable presentation input. Key is only a
// source/view identity supplied by the caller; SourceOffset is never persisted
// or used as a durable identity.
type transcriptViewportRow struct {
	key               string
	baseText          string
	baseLines         []string
	text              string
	lines             []string
	lineWidth         int
	searchText        string
	receiptOutcome    string
	revision          uint64
	expanded          bool
	viewTruncated     bool
	sourceTruncated   *bool
	excludeFromSearch bool
	toolPane          bool
}

type transcriptViewportAnchor struct {
	rowKey       string
	sourceOffset int
}

type transcriptViewportLine struct {
	rowKey       string
	sourceOffset int
	text         string
}

type transcriptViewportWindow struct {
	lines       []transcriptViewportLine
	startLine   int
	totalLines  int
	loadedScope string
}

type transcriptViewportRowMetadata struct {
	key            string
	text           string
	renderedLines  []string
	lineOffsets    []int
	renderOnDemand bool
	revision       uint64
	width          int
	expanded       bool
	contentVersion uint64
	lineCount      int
}

type transcriptViewportCacheKey struct {
	key            string
	revision       uint64
	width          int
	expanded       bool
	contentVersion uint64
	localStart     int
	localEnd       int
	renderedStart  int
	renderedEnd    int
}

// transcriptViewport owns only loaded row descriptors, line-count metadata,
// an anchor/selection/search key, and the last visible-plus-overscan window.
// It is not a transcript store and does not claim to bound source snapshot
// memory or full-history search.
type transcriptViewport struct {
	width  int
	height int

	rows        []transcriptViewportRow
	indexByKey  map[string]int
	rowMetadata []*transcriptViewportRowMetadata
	lineTree    []int
	totalLines  int

	anchor       transcriptViewportAnchor
	anchorLine   int
	followTail   bool
	selectedKey  string
	searchHitKey string
	loadedScope  string

	renderCache        []transcriptViewportLine
	cacheKeys          []transcriptViewportCacheKey
	cacheStart         int
	cacheEnd           int
	cacheWidth         int
	cacheHeight        int
	cacheValid         bool
	nextContentVersion uint64
	setRowsCalls       uint64
	sameRowsCalls      uint64
	updateRowCalls     uint64
	appendRowCalls     uint64
}

func newTranscriptViewport(width, height int) *transcriptViewport {
	return &transcriptViewport{
		width:      width,
		height:     height,
		indexByKey: make(map[string]int),
		lineTree:   []int{0},
		followTail: true,
	}
}

// SetRows atomically replaces the loaded source projection. Unchanged keyed
// rows reuse their line-count metadata across snapshots; rendered fragments
// remain limited to the visible window and fixed overscan.
func (v *transcriptViewport) SetRows(rows []transcriptViewportRow, loadedScope string) error {
	v.setRowsCalls++
	if strings.TrimSpace(loadedScope) == "" {
		return errTranscriptViewportScopeRequired
	}

	newIndex := make(map[string]int, len(rows))
	for i, row := range rows {
		if row.key == "" {
			return fmt.Errorf("%w at row %d", errTranscriptViewportEmptyKey, i)
		}
		if _, exists := newIndex[row.key]; exists {
			return fmt.Errorf("%w: %q", errTranscriptViewportDuplicateKey, row.key)
		}
		newIndex[row.key] = i
	}

	if v.sameRows(rows) {
		v.loadedScope = loadedScope
		return nil
	}

	oldAnchor := v.anchor
	oldAnchorIndex, hadOldAnchor := v.indexByKey[oldAnchor.rowKey]
	oldFollowTail := v.followTail
	oldMetadata := make(map[string]*transcriptViewportRowMetadata, len(v.rowMetadata))
	oldRows := make(map[string]transcriptViewportRow, len(v.rows))
	for index, meta := range v.rowMetadata {
		oldMetadata[meta.key] = meta
		oldRows[meta.key] = v.rows[index]
	}

	newRows := append([]transcriptViewportRow(nil), rows...)
	newMetadata := make([]*transcriptViewportRowMetadata, len(newRows))
	for i, row := range newRows {
		if previous, ok := oldMetadata[row.key]; ok && previous.width == v.width &&
			sameTranscriptViewportRow(row, oldRows[row.key]) {
			newMetadata[i] = previous
			continue
		}
		newMetadata[i] = v.newTranscriptViewportRowMetadata(row, v.width)
	}

	v.rows = newRows
	v.indexByKey = newIndex
	v.rowMetadata = newMetadata
	v.loadedScope = loadedScope
	v.rebuildLineIndex()
	if _, ok := v.indexByKey[v.selectedKey]; !ok {
		v.selectedKey = ""
	}
	if _, ok := v.indexByKey[v.searchHitKey]; !ok {
		v.searchHitKey = ""
	}

	switch {
	case len(v.rows) == 0:
		v.anchor = transcriptViewportAnchor{}
		v.anchorLine = 0
		v.followTail = true
	case oldFollowTail && v.width > 0 && v.height > 0:
		v.followTail = true
		v.setAnchorAtLine(v.maximumStartLine())
	case oldFollowTail:
		v.followTail = true
		if index, ok := v.indexByKey[oldAnchor.rowKey]; ok {
			v.restoreAnchor(index, oldAnchor.sourceOffset)
		} else {
			v.setAnchorAtLine(0)
		}
	case oldAnchor.rowKey != "":
		if index, ok := v.indexByKey[oldAnchor.rowKey]; ok {
			v.restoreAnchor(index, oldAnchor.sourceOffset)
		} else {
			nearest := 0
			if hadOldAnchor {
				nearest = oldAnchorIndex
			}
			if nearest >= len(v.rows) {
				nearest = len(v.rows) - 1
			}
			v.setAnchorAtLine(v.linePrefix(nearest))
		}
	default:
		v.setAnchorAtLine(0)
	}
	v.markRenderCacheDirty()
	return nil
}

func (v *transcriptViewport) sameRows(rows []transcriptViewportRow) bool {
	v.sameRowsCalls++
	if len(rows) != len(v.rows) {
		return false
	}
	for i, row := range rows {
		if !sameTranscriptViewportRow(row, v.rows[i]) {
			return false
		}
	}
	return true
}

func sameTranscriptViewportRow(a, b transcriptViewportRow) bool {
	if a.key != b.key || a.baseText != b.baseText || a.text != b.text || a.lineWidth != b.lineWidth ||
		a.searchText != b.searchText || a.receiptOutcome != b.receiptOutcome || a.revision != b.revision ||
		a.expanded != b.expanded || a.viewTruncated != b.viewTruncated || a.sourceTruncated != b.sourceTruncated ||
		a.excludeFromSearch != b.excludeFromSearch || a.toolPane != b.toolPane ||
		!sameTranscriptViewportLines(a.baseLines, b.baseLines) || !sameTranscriptViewportLines(a.lines, b.lines) {
		return false
	}
	return true
}

func sameTranscriptViewportLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// AppendRow adds one newly observed live row without rebuilding the loaded
// projection. Rows are appended in arrival order; later chunks should use
// updateRow with the same private or durable key.
func (v *transcriptViewport) AppendRow(row transcriptViewportRow) error {
	v.appendRowCalls++
	if row.key == "" {
		return errTranscriptViewportEmptyKey
	}
	if _, exists := v.indexByKey[row.key]; exists {
		return fmt.Errorf("%w: %q", errTranscriptViewportDuplicateKey, row.key)
	}
	index := len(v.rows)
	v.rows = append(v.rows, row)
	meta := v.newTranscriptViewportRowMetadata(row, v.width)
	v.rowMetadata = append(v.rowMetadata, meta)
	v.indexByKey[row.key] = index
	newPosition := index + 1
	newTree := append(v.lineTree, 0)
	lowBit := newPosition & -newPosition
	priorRangeStart := newPosition - lowBit
	priorRange := v.linePrefix(newPosition-1) - v.linePrefix(priorRangeStart)
	newTree[newPosition] = priorRange + meta.lineCount
	v.lineTree = newTree
	v.totalLines += meta.lineCount
	if v.followTail && v.width > 0 && v.height > 0 {
		v.setAnchorAtLine(v.maximumStartLine())
	}
	v.markRenderCacheDirty()
	return nil
}

func (v *transcriptViewport) newTranscriptViewportRowMetadata(row transcriptViewportRow, width int) *transcriptViewportRowMetadata {
	v.nextContentVersion++
	if row.lines != nil {
		rendered, offsets := transcriptViewportWrapLines(row.lines, width, row.lineWidth)
		if row.baseLines != nil {
			if len(rendered) == 0 {
				rendered = []string{""}
			}
			return &transcriptViewportRowMetadata{
				key:            row.key,
				text:           row.text,
				renderOnDemand: true,
				revision:       row.revision,
				width:          width,
				expanded:       row.expanded,
				contentVersion: v.nextContentVersion,
				lineCount:      len(rendered),
			}
		}
		if len(rendered) == 0 {
			rendered = []string{""}
			offsets = []int{0}
		}
		return &transcriptViewportRowMetadata{
			key:            row.key,
			text:           row.text,
			renderedLines:  rendered,
			lineOffsets:    offsets,
			revision:       row.revision,
			width:          width,
			expanded:       row.expanded,
			contentVersion: v.nextContentVersion,
			lineCount:      len(rendered),
		}
	}
	count := len(transcriptViewportSourceLineOffsets(row.text, width))
	return &transcriptViewportRowMetadata{
		key:            row.key,
		text:           row.text,
		revision:       row.revision,
		width:          width,
		expanded:       row.expanded,
		contentVersion: v.nextContentVersion,
		lineCount:      count,
	}
}

func transcriptViewportWrapLines(lines []string, width, sourceWidth int) ([]string, []int) {
	if width == sourceWidth {
		offsets := make([]int, 0, len(lines))
		sourceOffset := 0
		for index, line := range lines {
			safe, _ := transcriptViewportSafeText(line)
			offsets = append(offsets, sourceOffset)
			sourceOffset += len(safe)
			if index < len(lines)-1 {
				sourceOffset++
			}
		}
		return lines, offsets
	}
	wrapped := make([]string, 0, len(lines))
	offsets := make([]int, 0, len(lines))
	sourceOffset := 0
	for index, line := range lines {
		safe, _ := transcriptViewportSafeText(line)
		if width <= 0 || width == sourceWidth || ansi.StringWidth(safe) <= width {
			wrapped = append(wrapped, safe)
			offsets = append(offsets, sourceOffset)
		} else {
			lineText, lineOffsets := transcriptViewportWrapWithOffsets(safe, width)
			parts := strings.Split(lineText, "\n")
			for partIndex, part := range parts {
				wrapped = append(wrapped, part)
				offset := 0
				if partIndex < len(lineOffsets) {
					offset = lineOffsets[partIndex]
				}
				offsets = append(offsets, sourceOffset+offset)
			}
		}
		sourceOffset += len(safe)
		if index < len(lines)-1 {
			sourceOffset++
		}
	}
	return wrapped, offsets
}

func (v *transcriptViewport) updateRow(row transcriptViewportRow) error {
	v.updateRowCalls++
	index, ok := v.indexByKey[row.key]
	if !ok {
		if row.key == "" {
			return errTranscriptViewportEmptyKey
		}
		return fmt.Errorf("%w: %q", errTranscriptViewportMissingRow, row.key)
	}
	previous := v.rows[index]
	if row.revision < previous.revision || (row.revision == previous.revision && !sameTranscriptViewportRow(row, previous)) {
		return fmt.Errorf("%w: %q", errTranscriptViewportStaleRevision, row.key)
	}
	if sameTranscriptViewportRow(row, previous) {
		return nil
	}

	oldAnchor := v.anchor
	oldFollowTail := v.followTail
	oldLineCount := v.rowMetadata[index].lineCount
	v.rows[index] = row
	v.rowMetadata[index] = v.newTranscriptViewportRowMetadata(row, v.width)
	newLineCount := v.rowMetadata[index].lineCount
	v.addLineCount(index, newLineCount-oldLineCount)
	v.totalLines = v.linePrefix(len(v.rowMetadata))

	if oldFollowTail && v.width > 0 && v.height > 0 {
		v.setAnchorAtLine(v.maximumStartLine())
	} else if oldAnchor.rowKey == row.key {
		v.restoreAnchor(index, oldAnchor.sourceOffset)
	} else if v.totalLines == 0 {
		v.setAnchorAtLine(0)
	}
	v.markRenderCacheDirty()
	return nil
}

func (v *transcriptViewport) rebuildLineIndex() {
	v.lineTree = make([]int, len(v.rowMetadata)+1)
	for index, meta := range v.rowMetadata {
		count := meta.lineCount
		if count < 1 {
			count = 1
		}
		v.addLineCount(index, count)
	}
	v.totalLines = v.linePrefix(len(v.rowMetadata))
}

func (v *transcriptViewport) addLineCount(index, delta int) {
	for position := index + 1; position < len(v.lineTree); position += position & -position {
		v.lineTree[position] += delta
	}
}

// linePrefix returns the number of rendered lines in rows before rowCount.
func (v *transcriptViewport) linePrefix(rowCount int) int {
	if rowCount > len(v.rowMetadata) {
		rowCount = len(v.rowMetadata)
	}
	if rowCount < 0 {
		rowCount = 0
	}
	lines := 0
	for position := rowCount; position > 0; position -= position & -position {
		lines += v.lineTree[position]
	}
	return lines
}

// Resize reflows keyed source rows at the supplied cell dimensions. A zero or
// negative dimension produces an empty window without discarding the anchor.
func (v *transcriptViewport) Resize(width, height int) {
	widthChanged := width != v.width
	heightChanged := height != v.height
	if !widthChanged && !heightChanged {
		return
	}
	oldAnchor := v.anchor
	v.width = width
	v.height = height
	if widthChanged && width > 0 {
		for i, row := range v.rows {
			v.rowMetadata[i] = v.newTranscriptViewportRowMetadata(row, width)
		}
		v.rebuildLineIndex()
	}
	if width <= 0 || height <= 0 {
		if widthChanged && width > 0 && !v.followTail && oldAnchor.rowKey != "" {
			if index, ok := v.indexByKey[oldAnchor.rowKey]; ok {
				v.restoreAnchor(index, oldAnchor.sourceOffset)
			}
		}
		v.invalidateRenderCache()
		return
	}

	if v.followTail {
		v.setAnchorAtLine(v.maximumStartLine())
	} else if widthChanged && width > 0 && oldAnchor.rowKey != "" {
		if index, ok := v.indexByKey[oldAnchor.rowKey]; ok {
			v.restoreAnchor(index, oldAnchor.sourceOffset)
		}
	}
	v.invalidateRenderCache()
}

// ScrollLines moves the top-visible source position by wrapped cell lines.
// Any manual movement pauses follow-tail; End explicitly resumes it.
func (v *transcriptViewport) ScrollLines(delta int) {
	if delta == 0 || v.width <= 0 || v.height <= 0 || v.totalLines == 0 {
		return
	}
	current := v.currentStartLine()
	maximum := v.maximumStartLine()
	target := current
	if delta < 0 {
		if delta < -current {
			target = 0
		} else {
			target += delta
		}
	} else if delta > maximum-current {
		target = maximum
	} else {
		target += delta
	}
	v.followTail = false
	v.setAnchorAtLine(target)
	v.markRenderCacheDirty()
}

// End moves to the latest loaded row and resumes following appended content.
func (v *transcriptViewport) End() {
	v.followTail = true
	if v.width > 0 && v.height > 0 {
		v.setAnchorAtLine(v.maximumStartLine())
	}
	v.markRenderCacheDirty()
}

// Select records a keyed selection and scrolls only when the row is outside
// the current window. Selection is independent of the top-visible anchor.
func (v *transcriptViewport) Select(key string) bool {
	index, ok := v.indexByKey[key]
	if !ok {
		return false
	}
	v.selectedKey = key
	v.followTail = false
	v.ensureRowVisible(index)
	return true
}

// SelectRelative moves the keyed selection through currently loaded rows,
// clamping at either end. With no prior selection, Down selects the first
// row and Up selects the last row.
func (v *transcriptViewport) SelectRelative(delta int) string {
	if len(v.rows) == 0 || delta == 0 {
		return v.selectedKey
	}
	index, exists := v.indexByKey[v.selectedKey]
	if !exists {
		if delta > 0 {
			index = -1
		} else {
			index = len(v.rows)
		}
	}
	if delta < 0 {
		if delta < -index {
			index = 0
		} else {
			index += delta
		}
	} else if delta > len(v.rows)-1-index {
		index = len(v.rows) - 1
	} else {
		index += delta
	}
	v.Select(v.rows[index].key)
	return v.selectedKey
}

func (v *transcriptViewport) SelectSearchHit(key string) bool {
	if !v.Select(key) {
		return false
	}
	v.searchHitKey = key
	return true
}

// Search finds the next case-insensitive match in the currently loaded source
// rows. It stores a key, not copied result bodies, and scrolls an offscreen hit
// into view. A non-empty loaded scope is always returned by Window.
func (v *transcriptViewport) Search(query, afterKey string) (string, bool) {
	if query == "" || len(v.rows) == 0 {
		v.searchHitKey = ""
		return "", false
	}
	safeQuery, _ := transcriptViewportSafeText(query)
	needle := strings.ToLower(safeQuery)
	if needle == "" {
		v.searchHitKey = ""
		return "", false
	}
	start := 0
	if index, ok := v.indexByKey[afterKey]; ok {
		start = (index + 1) % len(v.rows)
	}
	for step := 0; step < len(v.rows); step++ {
		index := (start + step) % len(v.rows)
		if v.rows[index].excludeFromSearch {
			continue
		}
		searchText := v.rows[index].searchText
		if searchText == "" {
			searchText = v.rows[index].text
		}
		safeRow, _ := transcriptViewportSafeText(searchText)
		if strings.Contains(strings.ToLower(safeRow), needle) {
			v.searchHitKey = v.rows[index].key
			v.followTail = false
			v.ensureRowVisible(index)
			return v.searchHitKey, true
		}
	}
	v.searchHitKey = ""
	return "", false
}

func (v *transcriptViewport) ensureRowVisible(index int) {
	if v.width <= 0 || v.height <= 0 || index < 0 || index >= len(v.rows) {
		return
	}
	start := v.currentStartLine()
	end := v.totalLines
	if v.height <= v.totalLines-start {
		end = start + v.height
	}
	rowStart := v.linePrefix(index)
	rowEnd := v.linePrefix(index + 1)
	if rowStart < end && rowEnd > start {
		return
	}
	target := rowStart
	if rowEnd > end && rowEnd-rowStart <= v.height {
		target = rowEnd - v.height
	}
	v.followTail = false
	v.setAnchorAtLine(target)
	v.markRenderCacheDirty()
}

// Window returns only visible lines. A separate fixed two-line overscan is
// retained internally for the next small movement; no full-row rendered layout
// or offscreen row text is cached.
func (v *transcriptViewport) Window() transcriptViewportWindow {
	result := transcriptViewportWindow{
		startLine:   v.currentStartLine(),
		totalLines:  v.totalLines,
		loadedScope: v.loadedScope,
	}
	if v.width <= 0 || v.height <= 0 || v.totalLines == 0 {
		v.invalidateRenderCache()
		return result
	}

	visibleStart := v.currentStartLine()
	visibleCount := v.height
	if remaining := v.totalLines - visibleStart; visibleCount > remaining {
		visibleCount = remaining
	}
	if visibleCount <= 0 {
		return result
	}
	before := transcriptViewportOverscanLines / 2
	after := transcriptViewportOverscanLines - before
	cacheStart := visibleStart - before
	if cacheStart < 0 {
		cacheStart = 0
	}
	cacheEnd := visibleStart + visibleCount
	if after > v.totalLines-cacheEnd {
		cacheEnd = v.totalLines
	} else {
		cacheEnd += after
	}
	if !v.cacheValid || v.cacheStart != cacheStart || v.cacheEnd != cacheEnd || v.cacheWidth != v.width || v.cacheHeight != v.height {
		v.renderRange(cacheStart, cacheEnd)
	}
	visibleOffset := visibleStart - v.cacheStart
	visibleEnd := visibleOffset + visibleCount
	if visibleOffset < 0 || visibleOffset > len(v.renderCache) {
		return result
	}
	if visibleEnd > len(v.renderCache) {
		visibleEnd = len(v.renderCache)
	}
	result.lines = append([]transcriptViewportLine(nil), v.renderCache[visibleOffset:visibleEnd]...)
	return result
}

func (v *transcriptViewport) renderRange(start, end int) {
	previousLines := v.renderCache
	previousKeys := v.cacheKeys
	previousByKey := make(map[string]transcriptViewportCacheKey, len(previousKeys))
	for _, key := range previousKeys {
		previousByKey[key.key] = key
	}
	newLines := make([]transcriptViewportLine, 0, end-start)
	newKeys := make([]transcriptViewportCacheKey, 0, len(previousKeys)+1)
	v.cacheStart = start
	v.cacheEnd = end
	v.cacheWidth = v.width
	v.cacheHeight = v.height
	if start >= end || len(v.rows) == 0 {
		v.renderCache = newLines
		v.cacheKeys = newKeys
		v.cacheValid = true
		return
	}

	index := v.rowIndexForLine(start)
	for index < len(v.rows) && v.linePrefix(index) < end {
		row := v.rows[index]
		meta := v.rowMetadata[index]
		rowStart := v.linePrefix(index)
		localStart := start - rowStart
		if localStart < 0 {
			localStart = 0
		}
		localEnd := end - rowStart
		if localEnd > meta.lineCount {
			localEnd = meta.lineCount
		}
		segment := transcriptViewportCacheKey{
			key:            row.key,
			revision:       row.revision,
			width:          v.width,
			expanded:       row.expanded,
			contentVersion: meta.contentVersion,
			localStart:     localStart,
			localEnd:       localEnd,
			renderedStart:  len(newLines),
		}
		if previous, ok := previousByKey[row.key]; ok &&
			previous.revision == segment.revision && previous.width == segment.width &&
			previous.expanded == segment.expanded && previous.contentVersion == segment.contentVersion &&
			previous.localStart <= localStart && previous.localEnd >= localEnd &&
			previous.renderedEnd-previous.renderedStart == previous.localEnd-previous.localStart {
			from := previous.renderedStart + localStart - previous.localStart
			to := from + localEnd - localStart
			if from >= previous.renderedStart && to <= previous.renderedEnd && to <= len(previousLines) && from <= to {
				newLines = append(newLines, previousLines[from:to]...)
				segment.renderedEnd = len(newLines)
				newKeys = append(newKeys, segment)
				index++
				continue
			}
		}
		wrapped := meta.renderedLines
		offsets := meta.lineOffsets
		if meta.renderOnDemand {
			wrapped, offsets = transcriptViewportWrapLines(row.lines, v.width, row.lineWidth)
		} else if wrapped == nil {
			wrappedText, textOffsets := transcriptViewportWrapWithOffsets(row.text, v.width)
			wrapped = strings.Split(wrappedText, "\n")
			offsets = textOffsets
		}
		for local := localStart; local < localEnd; local++ {
			if local >= len(wrapped) {
				break
			}
			offset := 0
			if local < len(offsets) {
				offset = offsets[local]
			}
			newLines = append(newLines, transcriptViewportLine{
				rowKey:       row.key,
				sourceOffset: offset,
				// Split strings share the full temporary wrapped row. Clone each
				// visible fragment so the bounded cache does not retain offscreen text.
				text: strings.Clone(wrapped[local]),
			})
		}
		segment.renderedEnd = len(newLines)
		newKeys = append(newKeys, segment)
		index++
	}
	v.renderCache = newLines
	v.cacheKeys = newKeys
	v.cacheValid = true
}

func (v *transcriptViewport) rowIndexForLine(line int) int {
	if len(v.rows) == 0 {
		return 0
	}
	if line < 0 {
		line = 0
	}
	if line >= v.totalLines {
		line = v.totalLines - 1
	}
	index := 0
	lines := 0
	step := 1
	for step<<1 < len(v.lineTree) {
		step <<= 1
	}
	for step > 0 {
		next := index + step
		if next < len(v.lineTree) && lines+v.lineTree[next] <= line {
			index = next
			lines += v.lineTree[next]
		}
		step >>= 1
	}
	if index >= len(v.rows) {
		return len(v.rows) - 1
	}
	return index
}

func (v *transcriptViewport) setAnchorAtLine(line int) {
	if len(v.rows) == 0 || v.totalLines == 0 {
		v.anchor = transcriptViewportAnchor{}
		v.anchorLine = 0
		return
	}
	if line < 0 {
		line = 0
	}
	if line >= v.totalLines {
		line = v.totalLines - 1
	}
	index := v.rowIndexForLine(line)
	localLine := line - v.linePrefix(index)
	offsets := v.rowLineOffsets(index)
	if localLine >= len(offsets) {
		localLine = len(offsets) - 1
	}
	if localLine < 0 {
		localLine = 0
	}
	v.anchor = transcriptViewportAnchor{rowKey: v.rows[index].key, sourceOffset: offsets[localLine]}
	v.anchorLine = localLine
}

func (v *transcriptViewport) restoreAnchor(index, sourceOffset int) {
	if index < 0 || index >= len(v.rows) {
		v.setAnchorAtLine(0)
		return
	}
	offsets := v.rowLineOffsets(index)
	if len(v.rowMetadata[index].lineOffsets) > 0 {
		localLine := sort.Search(len(offsets), func(i int) bool { return offsets[i] > sourceOffset }) - 1
		if localLine < 0 {
			localLine = 0
		}
		if localLine >= len(offsets) {
			localLine = len(offsets) - 1
		}
		sourceOffset = offsets[localLine]
	} else {
		sourceOffset = transcriptViewportSafeOffsetRow(v.rows[index], sourceOffset)
	}
	localLine := sort.Search(len(offsets), func(i int) bool { return offsets[i] > sourceOffset }) - 1
	if localLine < 0 {
		localLine = 0
	}
	if localLine >= v.rowMetadata[index].lineCount {
		localLine = v.rowMetadata[index].lineCount - 1
	}
	v.anchor = transcriptViewportAnchor{rowKey: v.rows[index].key, sourceOffset: sourceOffset}
	v.anchorLine = localLine
}

func (v *transcriptViewport) rowLineOffsets(index int) []int {
	if index >= 0 && index < len(v.rowMetadata) && len(v.rowMetadata[index].lineOffsets) > 0 {
		return v.rowMetadata[index].lineOffsets
	}
	if index < 0 || index >= len(v.rows) {
		return nil
	}
	if v.rowMetadata[index].renderOnDemand {
		_, offsets := transcriptViewportWrapLines(v.rows[index].lines, v.width, v.rows[index].lineWidth)
		return offsets
	}
	return transcriptViewportSourceLineOffsets(v.rows[index].text, v.width)
}

func (v *transcriptViewport) currentStartLine() int {
	if v.followTail {
		return v.maximumStartLine()
	}
	index, ok := v.indexByKey[v.anchor.rowKey]
	if !ok || len(v.rows) == 0 {
		return 0
	}
	localLine := v.anchorLine
	if localLine < 0 {
		localLine = 0
	}
	if localLine >= v.rowMetadata[index].lineCount {
		localLine = v.rowMetadata[index].lineCount - 1
	}
	return v.linePrefix(index) + localLine
}

func (v *transcriptViewport) maximumStartLine() int {
	if v.height <= 0 || v.totalLines <= v.height {
		return 0
	}
	return v.totalLines - v.height
}

func (v *transcriptViewport) invalidateRenderCache() {
	v.renderCache = nil
	v.cacheKeys = nil
	v.cacheValid = false
}

func (v *transcriptViewport) markRenderCacheDirty() {
	v.cacheValid = false
}

func (v *transcriptViewport) Anchor() transcriptViewportAnchor { return v.anchor }
func (v *transcriptViewport) FollowTail() bool                 { return v.followTail }
func (v *transcriptViewport) SelectedKey() string              { return v.selectedKey }
func (v *transcriptViewport) SearchHitKey() string             { return v.searchHitKey }

func transcriptViewportSourceLineOffsets(text string, width int) []int {
	_, offsets := transcriptViewportWrapWithOffsets(text, width)
	return offsets
}

// transcriptViewportWrapWithOffsets derives source offsets from the exact
// wrapped string used for rendering. Hardwrap preserves every input byte and
// only inserts line-feed bytes, so each output newline either consumes the
// matching source newline or marks a wrap boundary at the current source byte.
func transcriptViewportWrapWithOffsets(text string, width int) (string, []int) {
	safe, _ := transcriptViewportSafeText(text)
	wrapped := safe
	if width > 0 {
		wrapped = ansi.GraphemeWidth.Hardwrap(safe, width, true)
	}

	starts := []int{0}
	sourceOffset := 0
	for i := 0; i < len(wrapped); i++ {
		if wrapped[i] == '\n' {
			if sourceOffset < len(safe) && safe[sourceOffset] == '\n' {
				sourceOffset++
			}
			starts = append(starts, sourceOffset)
			continue
		}
		sourceOffset++
	}
	return wrapped, starts
}

func transcriptViewportSafeOffset(text string, offset int) int {
	safe, _ := transcriptViewportSafeText(text)
	if offset <= 0 {
		return 0
	}
	if offset >= len(safe) {
		return len(safe)
	}

	state := ansi.NormalState
	position := 0
	for len(safe) > 0 {
		_, _, n, nextState := ansi.GraphemeWidth.DecodeSequenceInString(safe, state, nil)
		if n <= 0 {
			n = 1
			nextState = ansi.NormalState
		}
		if position+n > offset {
			return position
		}
		position += n
		safe = safe[n:]
		state = nextState
	}
	return position
}

func transcriptViewportSafeOffsetRow(row transcriptViewportRow, offset int) int {
	if row.lines == nil {
		return transcriptViewportSafeOffset(row.text, offset)
	}

	sourceOffset := 0
	for index, line := range row.lines {
		safe, _ := transcriptViewportSafeText(line)
		if offset <= sourceOffset+len(safe) {
			return sourceOffset + transcriptViewportSafeOffset(safe, offset-sourceOffset)
		}
		sourceOffset += len(safe)
		if index < len(row.lines)-1 {
			sourceOffset++
		}
	}
	return sourceOffset
}

// transcriptViewportSafeText avoids allocating ANSI parser scratch for the
// common valid-UTF-8 printable case, where safeTerminalText is an identity.
// All control-containing or malformed input still goes through the shared
// sanitizer before it is measured, searched, or rendered.
func transcriptViewportSafeText(text string) (string, bool) {
	if transcriptViewportIsSafePlainText(text) {
		return text, true
	}
	return safeTerminalText(text), false
}

// transcriptViewportWrap exposes the sanitized hard-wrap string used by the
// viewport. transcriptViewportWrapWithOffsets derives layout metadata from
// that same string so rendering and source positions cannot diverge.
func transcriptViewportWrap(text string, width int) string {
	wrapped, _ := transcriptViewportWrapWithOffsets(text, width)
	return wrapped
}

func transcriptViewportIsSafePlainText(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if r != '\n' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}
