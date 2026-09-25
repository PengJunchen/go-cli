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
)

// transcriptViewportRow is an immutable presentation input. Key is only a
// source/view identity supplied by the caller; SourceOffset is never persisted
// or used as a durable identity.
type transcriptViewportRow struct {
	key      string
	text     string
	revision uint64
	expanded bool
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
	linePrefix  []int
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
}

func newTranscriptViewport(width, height int) *transcriptViewport {
	return &transcriptViewport{
		width:      width,
		height:     height,
		indexByKey: make(map[string]int),
		followTail: true,
	}
}

// SetRows atomically replaces the loaded source projection. Unchanged keyed
// rows reuse their line-count metadata across snapshots; rendered fragments
// remain limited to the visible window and fixed overscan.
func (v *transcriptViewport) SetRows(rows []transcriptViewportRow, loadedScope string) error {
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
	for _, meta := range v.rowMetadata {
		oldMetadata[meta.key] = meta
	}

	newRows := append([]transcriptViewportRow(nil), rows...)
	newMetadata := make([]*transcriptViewportRowMetadata, len(newRows))
	for i, row := range newRows {
		if previous, ok := oldMetadata[row.key]; ok && previous.width == v.width &&
			previous.revision == row.revision && previous.text == row.text && previous.expanded == row.expanded {
			newMetadata[i] = previous
			continue
		}
		newMetadata[i] = v.newTranscriptViewportRowMetadata(row, v.width)
	}

	v.rows = newRows
	v.indexByKey = newIndex
	v.rowMetadata = newMetadata
	v.loadedScope = loadedScope
	v.rebuildLinePrefix()
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
			if hadOldAnchor && oldAnchorIndex < len(v.rows) {
				nearest = oldAnchorIndex
			}
			if nearest >= len(v.rows) {
				nearest = len(v.rows) - 1
			}
			v.setAnchorAtLine(v.linePrefix[nearest])
		}
	default:
		v.setAnchorAtLine(0)
	}
	v.markRenderCacheDirty()
	return nil
}

func (v *transcriptViewport) sameRows(rows []transcriptViewportRow) bool {
	if len(rows) != len(v.rows) {
		return false
	}
	for i, row := range rows {
		if row != v.rows[i] {
			return false
		}
	}
	return true
}

func (v *transcriptViewport) newTranscriptViewportRowMetadata(row transcriptViewportRow, width int) *transcriptViewportRowMetadata {
	v.nextContentVersion++
	count := 1
	if width > 0 {
		count = len(transcriptViewportSourceLineOffsets(row.text, width))
	}
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

func (v *transcriptViewport) rebuildLinePrefix() {
	v.linePrefix = make([]int, len(v.rowMetadata)+1)
	for i, meta := range v.rowMetadata {
		count := meta.lineCount
		if count < 1 {
			count = 1
		}
		v.linePrefix[i+1] = v.linePrefix[i] + count
	}
	v.totalLines = v.linePrefix[len(v.linePrefix)-1]
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
		v.rebuildLinePrefix()
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
	v.ensureRowVisible(index)
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
		safeRow, _ := transcriptViewportSafeText(v.rows[index].text)
		if strings.Contains(strings.ToLower(safeRow), needle) {
			v.searchHitKey = v.rows[index].key
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
	rowStart := v.linePrefix[index]
	rowEnd := v.linePrefix[index+1]
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
	for index < len(v.rows) && v.linePrefix[index] < end {
		row := v.rows[index]
		meta := v.rowMetadata[index]
		localStart := start - v.linePrefix[index]
		if localStart < 0 {
			localStart = 0
		}
		localEnd := end - v.linePrefix[index]
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
		wrapped := strings.Split(transcriptViewportWrap(row.text, v.width), "\n")
		offsets := transcriptViewportSourceLineOffsets(row.text, v.width)
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
	return sort.Search(len(v.rows), func(i int) bool { return v.linePrefix[i+1] > line })
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
	localLine := line - v.linePrefix[index]
	offsets := transcriptViewportSourceLineOffsets(v.rows[index].text, v.width)
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
	sourceOffset = transcriptViewportSafeOffset(v.rows[index].text, sourceOffset)
	offsets := transcriptViewportSourceLineOffsets(v.rows[index].text, v.width)
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
	return v.linePrefix[index] + localLine
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
	safe, _ := transcriptViewportSafeText(text)
	starts := []int{0}
	if width <= 0 || safe == "" {
		return starts
	}

	state := ansi.NormalState
	column := 0
	consumed := 0
	for len(safe) > 0 {
		seq, seqWidth, n, nextState := ansi.GraphemeWidth.DecodeSequenceInString(safe, state, nil)
		if n <= 0 {
			n = 1
			seqWidth = 0
			nextState = ansi.NormalState
		}
		if seq == "\n" {
			starts = append(starts, consumed+n)
			column = 0
		} else if nextState == ansi.NormalState && !isTerminalSequence(seq) && seqWidth > 0 {
			if column > 0 && column+seqWidth > width {
				starts = append(starts, consumed)
				column = 0
			}
			column += seqWidth
		}
		consumed += n
		safe = safe[n:]
		state = nextState
	}
	return starts
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

// transcriptViewportWrap uses the same grapheme-aware ANSI hard wrapper as
// wrapCellText after the input has been proven safe. Rich/control text is sent
// through wrapCellText so the established sanitizer remains authoritative.
func transcriptViewportWrap(text string, width int) string {
	if width <= 0 {
		safe, _ := transcriptViewportSafeText(text)
		return safe
	}
	if transcriptViewportIsSafePlainText(text) {
		return ansi.GraphemeWidth.Hardwrap(text, width, true)
	}
	return wrapCellText(text, width)
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
