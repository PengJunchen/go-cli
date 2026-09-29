package tui

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

const (
	maxStreamingRenderLines        = 256
	maxStreamingRenderBytes        = 64 * 1024
	maxPendingTerminalControlBytes = 4 * 1024
	maxPendingGraphemeBytes        = 1024
)

// StreamRenderWindow is a bounded presentation snapshot. HasHiddenPrefix is
// sticky until Reset: true means the renderer has evicted content and this
// snapshot must not be presented as the complete message.
type StreamRenderWindow struct {
	Content         string
	HasHiddenPrefix bool
}

// StreamRenderLines is a bounded preview represented as separate immutable
// line strings so an app can update a live viewport without joining and
// rewrapping the entire retained window after every chunk.
type StreamRenderLines struct {
	Lines           []string
	CurrentLine     string
	HasCurrentLine  bool
	NextLine        string
	HasNextLine     bool
	HasHiddenPrefix bool
}

type terminalControlMode uint8

const (
	terminalControlNone terminalControlMode = iota
	terminalControlEscape
	terminalControlCSI
	terminalControlOSC
)

type streamingMarkdownStats struct {
	inputBytes          int64
	parserBytes         int64
	markdownRenderCalls int64
	markdownRenderBytes int64
	renderedLines       int
	renderedBytes       int
	retainedSourceBytes int
	pendingControlBytes int
	discardingControl   bool
}

// StreamingMarkdownRenderer supports a bounded chunk API for live previews and
// retains RenderIncremental as the compatibility path for existing callers.
// RenderIncremental still accepts the complete accumulated source; callers
// should migrate only when they can also provide canonical source to Finish.
type StreamingMarkdownRenderer struct {
	md              *MarkdownRenderer
	stableRendered  string // cached rendered output for the stable prefix
	stableCount     int    // number of leading lines considered stable (cached)
	unstableCount   int    // trailing lines re-rendered on every call
	lastAccumulated string // previous accumulated input (for pure-append detection)

	codeBlockBuffer strings.Builder // buffers content inside an incomplete code block
	inCodeBlock     bool            // true when the accumulated content has an unclosed ```

	// Chunk mode is deliberately separate from RenderIncremental, which stays
	// available until the app is switched over. It stores only a finite
	// presentation tail; the caller remains the owner of canonical/provisional
	// source and supplies it once to Finish for full-document Markdown parsing.
	windowLines       []string
	windowLineParts   []string
	windowLineBytes   int
	windowDataBytes   int
	windowNewlines    int
	windowLineOpen    bool
	windowWidth       int
	windowHidden      bool
	windowFinished    bool
	column            int
	fenceLineState    uint8
	fenceTicks        uint8
	lineIsFence       bool
	graphemePending   string
	utf8Pending       []byte
	controlMode       terminalControlMode
	controlBytes      []byte
	controlEsc        bool
	discardingControl bool

	// sourceTail is a byte ring whose entries are complete graphemes or safe
	// terminal sequences. It exists only to reflow the bounded presentation
	// window; it is not a source of transcript truth.
	sourceTail       []byte
	sourceTailStart  int
	sourceTailSize   int
	sourceTailUnits  []uint32
	sourceUnitsStart int
	sourceUnitsSize  int
	sourceTailHidden bool

	inputBytes          int64
	parserBytes         int64
	markdownRenderCalls int64
	markdownRenderBytes int64
}

// NewStreamingMarkdownRenderer returns a StreamingMarkdownRenderer that wraps
// md and treats the last 2 lines as unstable.
func NewStreamingMarkdownRenderer(md *MarkdownRenderer) *StreamingMarkdownRenderer {
	return &StreamingMarkdownRenderer{md: md, unstableCount: 2}
}

// AppendChunk consumes only the newly received bytes and returns the retained
// safe preview window. Markdown syntax is finalized by Finish, which renders
// the caller-owned canonical source once so document-wide references and block
// semantics are preserved.
func (s *StreamingMarkdownRenderer) AppendChunk(ctx context.Context, chunk string, opts RenderOpts) StreamRenderWindow {
	s.appendChunk(ctx, chunk, opts)
	return s.Window()
}

// AppendChunkLines appends a chunk and returns the retained preview as lines.
// Lines borrows the renderer's completed-line slice and remains valid only
// until the next mutation; CurrentLine and NextLine hold the open grapheme row.
func (s *StreamingMarkdownRenderer) AppendChunkLines(ctx context.Context, chunk string, opts RenderOpts) StreamRenderLines {
	s.appendChunk(ctx, chunk, opts)
	return s.WindowLines()
}

func (s *StreamingMarkdownRenderer) appendChunk(ctx context.Context, chunk string, opts RenderOpts) {
	if s.windowFinished {
		s.Reset()
	}
	s.ensureChunkWidth(ctx, opts)
	s.inputBytes += int64(len(chunk))
	s.parserBytes += int64(len(chunk))
	for i := 0; i < len(chunk); {
		if s.controlMode == terminalControlNone && len(s.utf8Pending) == 0 && chunk[i] >= 0x20 && chunk[i] <= 0x7e {
			end := i + 1
			for end < len(chunk) && chunk[end] >= 0x20 && chunk[end] <= 0x7e {
				end++
			}
			for j := i; j < end; j++ {
				s.consumeFenceByte(chunk[j])
			}
			s.consumeASCIITextRun(chunk[i:end])
			i = end
			continue
		}
		b := chunk[i]
		s.consumeFenceByte(b)
		s.consumePreviewByte(b)
		i++
	}
}

// Finish performs the one full-document Markdown render and replaces the
// provisional preview with its bounded rendered tail. The source argument is
// borrowed for this call only; the renderer does not retain a full copy.
// Repeated calls return the finished window without rendering the document again.
func (s *StreamingMarkdownRenderer) Finish(ctx context.Context, canonicalSource string, opts RenderOpts) StreamRenderWindow {
	if s.windowFinished {
		return s.Window()
	}
	s.ensureChunkWidth(ctx, opts)
	if s.lineIsFence {
		s.inCodeBlock = !s.inCodeBlock
		s.lineIsFence = false
	}
	s.flushGrapheme()
	s.markdownRenderCalls++
	s.markdownRenderBytes += int64(len(canonicalSource))
	rendered := safeTerminalText(s.md.Render(ctx, canonicalSource, opts))
	s.replaceWindowWithText(rendered, false)
	s.setSourceTail(canonicalSource)
	s.windowFinished = true
	return s.Window()
}

// Window returns the current finite presentation window. It never claims to be
// the entire message when HasHiddenPrefix is true.
func (s *StreamingMarkdownRenderer) Window() StreamRenderWindow {
	pending, pendingWrap := s.windowPending()
	var content strings.Builder
	content.Grow(s.windowDataBytes + len(pending) + s.windowNewlines + boolInt(pendingWrap))
	for i, line := range s.windowLines {
		if i > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(line)
	}
	if s.windowLineOpen || len(s.windowLineParts) > 0 || pending != "" {
		if len(s.windowLines) > 0 {
			content.WriteByte('\n')
		}
		for _, part := range s.windowLineParts {
			content.WriteString(part)
		}
		if pendingWrap {
			content.WriteByte('\n')
		}
		content.WriteString(pending)
	}
	return StreamRenderWindow{
		Content:         content.String(),
		HasHiddenPrefix: s.windowHidden,
	}
}

// WindowLines returns the same bounded safe preview as Window without joining
// retained lines into one string.
func (s *StreamingMarkdownRenderer) WindowLines() StreamRenderLines {
	pending, pendingWrap := s.windowPending()
	window := StreamRenderLines{Lines: s.windowLines, HasHiddenPrefix: s.windowHidden}
	if s.windowLineOpen || len(s.windowLineParts) > 0 || pending != "" {
		var current strings.Builder
		current.Grow(s.windowLineBytes + len(pending))
		for _, part := range s.windowLineParts {
			current.WriteString(part)
		}
		if pendingWrap {
			window.CurrentLine = current.String()
			window.HasCurrentLine = true
			window.NextLine = pending
			window.HasNextLine = true
		} else {
			current.WriteString(pending)
			window.CurrentLine = current.String()
			window.HasCurrentLine = true
		}
	}
	return window
}

func (s *StreamingMarkdownRenderer) windowPending() (string, bool) {
	pending := s.graphemePending
	pendingWrap := false
	if pending != "" {
		width := streamingGraphemeWidth(pending)
		pendingWrap = width > 0 && s.windowColumn() > 0 && s.windowColumn()+width > s.windowWidth
		pendingBytes := len(pending)
		if pendingWrap {
			pendingBytes++
		}
		for s.windowDataBytes+s.windowNewlines+pendingBytes > maxStreamingRenderBytes && len(s.windowLines) > 0 {
			s.dropFirstWindowLine()
		}
		if s.windowDataBytes+s.windowNewlines+pendingBytes > maxStreamingRenderBytes {
			// Keep the previous complete content and omit this complete grapheme;
			// never cut its UTF-8 bytes to meet the output budget.
			pending = ""
			pendingWrap = false
			s.windowHidden = true
		}
	}
	visibleRows := len(s.windowLines)
	if s.windowLineOpen || len(s.windowLineParts) > 0 || pending != "" {
		visibleRows++
	}
	if pendingWrap {
		visibleRows++
	}
	for visibleRows > maxStreamingRenderLines && len(s.windowLines) > 0 {
		s.dropFirstWindowLine()
		visibleRows--
	}
	if visibleRows > maxStreamingRenderLines && pendingWrap {
		// The current row cannot be evicted here, so omit only the virtual row.
		pending = ""
		pendingWrap = false
		s.windowHidden = true
	}
	return pending, pendingWrap
}

// Resize reflows only the retained tail. If Finish has already run, it renders
// only the retained source suffix, never the full caller-owned message.
func (s *StreamingMarkdownRenderer) Resize(ctx context.Context, opts RenderOpts) StreamRenderWindow {
	width := normalizedStreamWidth(opts.Width)
	if width == s.windowWidth {
		return s.Window()
	}
	s.windowWidth = width
	tail := s.sourceTailString()
	if s.windowFinished {
		s.markdownRenderCalls++
		s.markdownRenderBytes += int64(len(tail))
		tail = safeTerminalText(s.md.Render(ctx, tail, opts))
		s.replaceWindowWithText(tail, false)
	} else {
		s.replaceWindowWithText(wrapCellText(tail, width), true)
	}
	if s.sourceTailHidden {
		s.windowHidden = true
	}
	return s.Window()
}

func (s *StreamingMarkdownRenderer) ensureChunkWidth(ctx context.Context, opts RenderOpts) {
	width := normalizedStreamWidth(opts.Width)
	if s.windowWidth == 0 {
		s.windowWidth = width
		return
	}
	if s.windowWidth != width {
		s.Resize(ctx, opts)
	}
}

func normalizedStreamWidth(width int) int {
	if width <= 0 {
		return 80
	}
	return width
}

func (s *StreamingMarkdownRenderer) consumeFenceByte(b byte) {
	if b == '\n' {
		if s.lineIsFence {
			s.inCodeBlock = !s.inCodeBlock
		}
		s.lineIsFence = false
		s.fenceLineState = 0
		s.fenceTicks = 0
		return
	}
	if s.lineIsFence || s.fenceLineState == 3 {
		return
	}
	switch s.fenceLineState {
	case 0: // optional indentation before a fence
		if b == ' ' || b == '\t' {
			return
		}
		if b == '`' {
			s.fenceTicks = 1
			s.fenceLineState = 1
			return
		}
		s.fenceLineState = 3
	case 1:
		if b == '`' {
			s.fenceTicks++
			if s.fenceTicks == 3 {
				s.lineIsFence = true
			}
			return
		}
		s.fenceLineState = 3
	}
}

func (s *StreamingMarkdownRenderer) consumePreviewByte(b byte) {
	if s.controlMode != terminalControlNone {
		s.consumeControlByte(b)
		return
	}
	if len(s.utf8Pending) > 0 {
		s.utf8Pending = append(s.utf8Pending, b)
		s.decodeUTF8Pending()
		return
	}
	switch {
	case b == 0x1b:
		s.startControl(terminalControlEscape, b)
	case b == 0x9b:
		s.startControl(terminalControlCSI, b)
	case b == 0x9d:
		s.startControl(terminalControlOSC, b)
	case b < utf8.RuneSelf:
		s.consumeASCII(b)
	case b >= 0x80 && b <= 0x9f:
		// Raw C1 controls are not UTF-8 text and are never passed through.
	case !utf8.RuneStart(b):
		s.pushRune(utf8.RuneError)
	default:
		s.utf8Pending = append(s.utf8Pending[:0], b)
		s.decodeUTF8Pending()
	}
}

func (s *StreamingMarkdownRenderer) decodeUTF8Pending() {
	for len(s.utf8Pending) > 0 && utf8.FullRune(s.utf8Pending) {
		r, size := utf8.DecodeRune(s.utf8Pending)
		if size <= 0 {
			size = 1
		}
		first := s.utf8Pending[0]
		remaining := append([]byte(nil), s.utf8Pending[size:]...)
		s.utf8Pending = s.utf8Pending[:0]
		if r == utf8.RuneError && size == 1 && first >= 0x80 && first <= 0x9f {
			// Invalid standalone C1 bytes are terminal controls, not text.
		} else {
			s.pushRune(r)
		}
		if len(remaining) == 0 {
			return
		}
		s.utf8Pending = append(s.utf8Pending, remaining...)
	}
}

func (s *StreamingMarkdownRenderer) consumeASCII(b byte) {
	switch {
	case b == '\n':
		s.flushGrapheme()
		s.appendSourcePart("\n")
		s.commitWindowLine()
	case b == '\t':
		s.flushGrapheme()
		spaces := terminalTabWidth - s.windowColumn()%terminalTabWidth
		for i := 0; i < spaces; i++ {
			s.emitGrapheme(" ", 1)
		}
	case b == '\r' || b < 0x20 || b == 0x7f:
		// Drop unsafe or non-rendering C0 controls.
	default:
		s.pushRune(rune(b))
	}
}

func (s *StreamingMarkdownRenderer) consumeASCIITextRun(text string) {
	if text == "" {
		return
	}
	// Keep the final byte as the possible start of a grapheme that continues in
	// the next chunk. The preceding ASCII run is already complete and can be
	// wrapped in slices instead of allocating a string per byte.
	if s.graphemePending != "" {
		s.flushGrapheme()
	}
	complete := text[:len(text)-1]
	for len(complete) > 0 {
		room := s.windowWidth - s.windowColumn()
		if room <= 0 {
			s.commitWindowLine()
			room = s.windowWidth
		}
		if room > maxStreamingRenderBytes {
			room = maxStreamingRenderBytes
		}
		n := len(complete)
		if n > room {
			n = room
		}
		part := complete[:n]
		s.appendSourcePart(part)
		s.appendWindowPart(part)
		s.setWindowColumn(s.windowColumn() + n)
		complete = complete[n:]
		if len(complete) > 0 {
			s.commitWindowLine()
		}
	}
	s.graphemePending = strings.Clone(text[len(text)-1:])
}

func (s *StreamingMarkdownRenderer) pushRune(r rune) {
	if unicode.IsControl(r) {
		return
	}
	text := string(r)
	if s.graphemePending == "" {
		s.graphemePending = text
		return
	}
	candidate := s.graphemePending + text
	if len(candidate) > maxPendingGraphemeBytes {
		// The canonical source remains with the caller. Drop only this oversized
		// display cluster and keep a safe replacement in the bounded preview.
		s.graphemePending = string(utf8.RuneError)
		s.windowHidden = true
		return
	}
	seq, width, n := candidate, 0, 0
	if len(candidate) == 1 && candidate[0] >= 0x20 && candidate[0] <= 0x7e {
		width, n = 1, 1
	} else {
		seq, width, n, _ = ansi.GraphemeWidth.DecodeSequenceInString(candidate, ansi.NormalState, ansi.NewParser())
	}
	if n <= 0 || n >= len(candidate) {
		s.graphemePending = candidate
		return
	}
	s.emitGrapheme(seq, width)
	s.graphemePending = candidate[n:]
}

func (s *StreamingMarkdownRenderer) flushGrapheme() {
	for s.graphemePending != "" {
		seq, width, n := s.graphemePending, 0, 0
		if len(seq) == 1 && seq[0] >= 0x20 && seq[0] <= 0x7e {
			width, n = 1, 1
		} else {
			seq, width, n, _ = ansi.GraphemeWidth.DecodeSequenceInString(s.graphemePending, ansi.NormalState, ansi.NewParser())
		}
		if n <= 0 {
			seq = s.graphemePending
			n = len(seq)
			width = ansi.StringWidth(seq)
		}
		if n > len(s.graphemePending) {
			n = len(s.graphemePending)
		}
		s.emitGrapheme(seq, width)
		s.graphemePending = s.graphemePending[n:]
	}
}

func streamingGraphemeWidth(text string) int {
	if len(text) == 1 && text[0] >= 0x20 && text[0] <= 0x7e {
		return 1
	}
	return ansi.StringWidth(text)
}

func (s *StreamingMarkdownRenderer) emitGrapheme(text string, width int) {
	if text == "" {
		return
	}
	if width < 0 {
		width = ansi.StringWidth(text)
	}
	if width > 0 && s.windowColumn() > 0 && s.windowColumn()+width > s.windowWidth {
		s.commitWindowLine()
	}
	s.appendSourcePart(text)
	s.appendWindowPart(text)
	s.setWindowColumn(s.windowColumn() + width)
}

func (s *StreamingMarkdownRenderer) windowColumn() int { return s.column }

func (s *StreamingMarkdownRenderer) setWindowColumn(width int) { s.column = width }

func (s *StreamingMarkdownRenderer) startControl(mode terminalControlMode, first byte) {
	s.controlMode = mode
	s.controlBytes = append(s.controlBytes[:0], first)
	s.controlEsc = false
}

func (s *StreamingMarkdownRenderer) consumeControlByte(b byte) {
	if !s.discardingControl {
		if len(s.controlBytes) >= maxPendingTerminalControlBytes {
			s.controlBytes = s.controlBytes[:0]
			s.discardingControl = true
		} else {
			s.controlBytes = append(s.controlBytes, b)
		}
	}
	complete := false
	switch s.controlMode {
	case terminalControlEscape:
		switch {
		case b == '[':
			s.controlMode = terminalControlCSI
		case b == ']':
			s.controlMode = terminalControlOSC
		case b >= 0x20 && b <= 0x2f:
			// ESC intermediate byte; wait for a final byte.
		case b >= 0x30 && b <= 0x7e:
			complete = true
		default:
			complete = true
		}
	case terminalControlCSI:
		complete = b >= 0x40 && b <= 0x7e
	case terminalControlOSC:
		complete = b == 0x07 || b == 0x9c || (s.controlEsc && b == '\\')
		s.controlEsc = b == 0x1b
	}
	if complete {
		s.finishControl()
	}
}

func (s *StreamingMarkdownRenderer) finishControl() {
	if !s.discardingControl {
		safe := safeTerminalText(string(s.controlBytes))
		if safe != "" {
			s.flushGrapheme()
			s.appendSourcePart(safe)
			s.appendWindowPart(safe)
		}
	}
	s.controlMode = terminalControlNone
	s.controlBytes = s.controlBytes[:0]
	s.controlEsc = false
	s.discardingControl = false
}

func (s *StreamingMarkdownRenderer) appendSourcePart(part string) {
	if part == "" {
		return
	}
	if s.sourceTail == nil {
		s.sourceTail = make([]byte, maxStreamingRenderBytes)
		s.sourceTailUnits = make([]uint32, maxStreamingRenderBytes)
	}
	if len(part) > maxStreamingRenderBytes {
		sourceUnits := s.sourceTailSize > 0
		s.sourceTailStart, s.sourceTailSize = 0, 0
		s.sourceUnitsStart, s.sourceUnitsSize = 0, 0
		s.sourceTailHidden = true
		if sourceUnits {
			s.windowHidden = true
		}
		return
	}
	for s.sourceTailSize+len(part) > len(s.sourceTail) || s.sourceUnitsSize == len(s.sourceTailUnits) {
		s.dropSourceTailUnit()
		s.sourceTailHidden = true
		s.windowHidden = true
	}
	capBytes := len(s.sourceTail)
	end := (s.sourceTailStart + s.sourceTailSize) % capBytes
	first := copy(s.sourceTail[end:], part)
	if first < len(part) {
		copy(s.sourceTail, part[first:])
	}
	s.sourceTailSize += len(part)
	unitIndex := (s.sourceUnitsStart + s.sourceUnitsSize) % len(s.sourceTailUnits)
	s.sourceTailUnits[unitIndex] = uint32(len(part))
	s.sourceUnitsSize++
}

func (s *StreamingMarkdownRenderer) dropSourceTailUnit() {
	if s.sourceUnitsSize == 0 {
		return
	}
	n := int(s.sourceTailUnits[s.sourceUnitsStart])
	s.sourceTailStart = (s.sourceTailStart + n) % len(s.sourceTail)
	s.sourceTailSize -= n
	s.sourceUnitsStart = (s.sourceUnitsStart + 1) % len(s.sourceTailUnits)
	s.sourceUnitsSize--
}

func (s *StreamingMarkdownRenderer) sourceTailString() string {
	if s.sourceTailSize == 0 {
		return ""
	}
	out := make([]byte, s.sourceTailSize)
	first := copy(out, s.sourceTail[s.sourceTailStart:])
	if first < len(out) {
		copy(out[first:], s.sourceTail[:len(out)-first])
	}
	return string(out)
}

func (s *StreamingMarkdownRenderer) setSourceTail(text string) {
	if s.sourceTail == nil {
		s.sourceTail = make([]byte, maxStreamingRenderBytes)
		s.sourceTailUnits = make([]uint32, maxStreamingRenderBytes)
	}
	s.sourceTailStart, s.sourceTailSize = 0, 0
	s.sourceUnitsStart, s.sourceUnitsSize = 0, 0
	start := len(text) - maxStreamingRenderBytes
	if start < 0 {
		start = 0
	}
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	if start > 0 {
		start = nextGraphemeBoundary(text, start)
		sourceTail := text[start:]
		if newline := strings.IndexByte(sourceTail, '\n'); newline >= 0 && newline+1 < len(sourceTail) {
			start += newline + 1
		}
		s.sourceTailHidden = true
		s.windowHidden = true
	}
	if start < len(text) {
		part := text[start:]
		// A canonical suffix can contain many graphemes. Store it as one bounded
		// ring unit so resize never splits its UTF-8 bytes.
		if len(part) <= maxStreamingRenderBytes {
			copy(s.sourceTail, part)
			s.sourceTailSize = len(part)
			s.sourceTailUnits[0] = uint32(len(part))
			s.sourceUnitsSize = 1
		}
	}
}

func nextGraphemeBoundary(text string, offset int) int {
	state := -1
	for position := 0; position < len(text); {
		cluster, _, _, nextState := uniseg.FirstGraphemeClusterInString(text[position:], state)
		size := len(cluster)
		if size <= 0 {
			_, size = utf8.DecodeRuneInString(text[position:])
		}
		if size <= 0 {
			size = 1
		}
		next := position + size
		if next >= offset {
			return next
		}
		state = nextState
		position = next
	}
	return len(text)
}

func (s *StreamingMarkdownRenderer) appendWindowPart(part string) {
	if part == "" {
		return
	}
	if len(part) > maxStreamingRenderBytes || s.windowLineBytes+len(part) > maxStreamingRenderBytes {
		for s.windowLineBytes+len(part) > maxStreamingRenderBytes && len(s.windowLineParts) > 0 {
			s.windowLineBytes -= len(s.windowLineParts[0])
			s.windowDataBytes -= len(s.windowLineParts[0])
			s.windowLineParts[0] = ""
			s.windowLineParts = s.windowLineParts[1:]
			s.windowHidden = true
		}
		if len(part) > maxStreamingRenderBytes || s.windowLineBytes+len(part) > maxStreamingRenderBytes {
			s.windowHidden = true
			return
		}
	}
	s.windowLineParts = append(s.windowLineParts, strings.Clone(part))
	s.windowLineBytes += len(part)
	s.windowDataBytes += len(part)
	s.windowLineOpen = true
	s.enforceWindowBounds()
}

func (s *StreamingMarkdownRenderer) commitWindowLine() {
	line := strings.Join(s.windowLineParts, "")
	s.windowLines = append(s.windowLines, line)
	s.windowLineParts = nil
	s.windowLineBytes = 0
	s.column = 0
	s.windowLineOpen = true
	s.windowNewlines++
	s.enforceWindowBounds()
}

func (s *StreamingMarkdownRenderer) enforceWindowBounds() {
	for len(s.windowLines)+boolInt(s.windowLineOpen) > maxStreamingRenderLines {
		s.dropFirstWindowLine()
	}
	for s.windowDataBytes+s.windowNewlines > maxStreamingRenderBytes && len(s.windowLines) > 0 {
		s.dropFirstWindowLine()
	}
	for s.windowLineBytes+s.windowNewlines > maxStreamingRenderBytes && len(s.windowLineParts) > 0 {
		s.windowDataBytes -= len(s.windowLineParts[0])
		s.windowLineBytes -= len(s.windowLineParts[0])
		s.windowLineParts[0] = ""
		s.windowLineParts = s.windowLineParts[1:]
		s.windowHidden = true
	}
	if s.windowDataBytes+s.windowNewlines > maxStreamingRenderBytes {
		// An individual completed row can exceed the byte cache budget (for
		// example with an unusually wide terminal); evict the entire row rather
		// than cut through a grapheme or terminal sequence.
		for len(s.windowLines) > 0 && s.windowDataBytes+s.windowNewlines > maxStreamingRenderBytes {
			s.dropFirstWindowLine()
		}
	}
}

func (s *StreamingMarkdownRenderer) dropFirstWindowLine() {
	if len(s.windowLines) == 0 {
		return
	}
	s.windowDataBytes -= len(s.windowLines[0])
	s.windowLines[0] = ""
	s.windowLines = s.windowLines[1:]
	if s.windowNewlines > 0 {
		s.windowNewlines--
	}
	s.windowHidden = true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *StreamingMarkdownRenderer) replaceWindowWithText(text string, keepLastOpen bool) {
	s.windowLines = nil
	s.windowLineParts = nil
	s.windowLineBytes = 0
	s.windowDataBytes = 0
	s.windowNewlines = 0
	s.windowLineOpen = false
	s.column = 0
	if text == "" {
		return
	}
	parts := strings.Split(text, "\n")
	if keepLastOpen {
		last := len(parts) - 1
		for i, line := range parts[:last] {
			s.appendWindowLine(line, i > 0)
		}
		if len(parts) > 1 {
			s.windowNewlines++
		}
		s.windowLineOpen = true
		if len(parts[last]) <= maxStreamingRenderBytes {
			if parts[last] != "" {
				s.windowLineParts = append(s.windowLineParts, strings.Clone(parts[last]))
				s.windowLineBytes = len(parts[last])
				s.windowDataBytes += len(parts[last])
			}
			s.column = cellWidth(parts[last])
		} else {
			s.windowHidden = true
		}
	} else {
		for i, line := range parts {
			if i == len(parts)-1 && line == "" {
				break
			}
			s.appendWindowLine(line, i > 0)
		}
	}
	s.enforceWindowBounds()
}

func (s *StreamingMarkdownRenderer) appendWindowLine(line string, separator bool) {
	if separator {
		s.windowNewlines++
	}
	if len(line) > maxStreamingRenderBytes {
		for len(s.windowLines) > 0 {
			s.dropFirstWindowLine()
		}
		s.windowHidden = true
		return
	}
	s.windowLines = append(s.windowLines, strings.Clone(line))
	s.windowDataBytes += len(line)
	s.enforceWindowBounds()
}

func (s *StreamingMarkdownRenderer) stats() streamingMarkdownStats {
	window := s.Window()
	lineCount := 0
	if len(s.windowLines) > 0 || s.windowLineOpen || len(s.windowLineParts) > 0 || window.Content != "" {
		lineCount = strings.Count(window.Content, "\n") + 1
	}
	return streamingMarkdownStats{
		inputBytes:          s.inputBytes,
		parserBytes:         s.parserBytes,
		markdownRenderCalls: s.markdownRenderCalls,
		markdownRenderBytes: s.markdownRenderBytes,
		renderedLines:       lineCount,
		renderedBytes:       len(window.Content),
		retainedSourceBytes: s.sourceTailSize,
		pendingControlBytes: len(s.controlBytes),
		discardingControl:   s.discardingControl,
	}
}

// RenderIncremental renders accumulated markdown, caching the stable prefix.
// It returns the combined rendered output (stable prefix + unstable tail).
//
// Cache rules:
//   - If accumulated has at most unstableCount lines, the whole buffer is
//     rendered directly (nothing is worth caching yet) and the cache resets.
//   - If the accumulated buffer did not grow by pure append since the last call
//     (e.g. a new message replaced the buffer), the cache is invalidated and the
//     new stable prefix is re-rendered.
//   - If the stable line count grew (a previously-unstable line became stable),
//     the enlarged stable prefix is re-rendered and re-cached.
//   - Otherwise (pure append within the existing stable region) the cached
//     stable output is reused and only the unstable tail is re-rendered.
//
// The KEY invariant: the whole accumulated buffer is never re-rendered on every
// token; only the unstable tail is, plus an occasional re-render of the stable
// prefix when it actually grows.
func (s *StreamingMarkdownRenderer) RenderIncremental(ctx context.Context, accumulated string, opts RenderOpts) string {
	lines := strings.Split(accumulated, "\n")

	// Too few lines to partition: render everything and reset the cache so the
	// next call starts fresh once stable lines appear.
	if len(lines) <= s.unstableCount {
		s.invalidate(accumulated)
		return s.md.Render(ctx, accumulated, opts)
	}

	stableCount := len(lines) - s.unstableCount

	// When an incomplete code block is detected (odd number of ``` markers),
	// ensure the opening fence and everything after it stays in the unstable
	// region. This prevents the renderer from caching a partial code block in
	// the stable prefix, which would produce broken output until the closing
	// fence arrives. The entire code block is re-rendered on each call until it
	// completes.
	if s.hasIncompleteCodeBlock(accumulated) {
		s.inCodeBlock = true
		// Find the last ``` fence — this is the opening of the incomplete block.
		lastFenceIdx := -1
		for i, line := range lines {
			if isFenceLine(line) {
				lastFenceIdx = i
			}
		}
		// Move the stable boundary back to before the fence so the entire
		// incomplete code block stays in the unstable region.
		if lastFenceIdx >= 0 && lastFenceIdx < stableCount {
			stableCount = lastFenceIdx
		}
		// Track the buffered code-block content for diagnostics / future use.
		s.codeBlockBuffer.Reset()
		s.codeBlockBuffer.WriteString(strings.Join(lines[stableCount:], "\n"))
	} else {
		s.inCodeBlock = false
		s.codeBlockBuffer.Reset()
	}

	// Invalidate when the stable region grew (more lines became stable) or when
	// the buffer is not a pure append of the previous one (replacement/reset).
	if stableCount != s.stableCount || !strings.HasPrefix(accumulated, s.lastAccumulated) {
		stablePart := strings.Join(lines[:stableCount], "\n")
		s.stableRendered = s.md.Render(ctx, stablePart, opts)
		s.stableCount = stableCount
	}

	s.lastAccumulated = accumulated

	// Always re-render the unstable tail.
	unstablePart := strings.Join(lines[stableCount:], "\n")
	unstableRendered := s.md.Render(ctx, unstablePart, opts)

	if s.stableRendered == "" {
		return unstableRendered
	}
	return s.stableRendered + "\n" + unstableRendered
}

// isFenceLine reports whether line is a markdown code fence marker: a line
// that, after optional leading whitespace, starts with ```. This avoids
// counting triple backticks that appear inline in text (e.g. inside a sentence)
// which are not fence delimiters.
func isFenceLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "```")
}

// hasIncompleteCodeBlock reports whether content contains an odd number of
// code fence markers (lines starting with ```), indicating that a code block
// has been opened but not yet closed.
func (s *StreamingMarkdownRenderer) hasIncompleteCodeBlock(content string) bool {
	count := 0
	for _, line := range strings.Split(content, "\n") {
		if isFenceLine(line) {
			count++
		}
	}
	return count%2 != 0
}

// invalidate clears the stable cache and records the current accumulated input
// as the new append-detection baseline.
func (s *StreamingMarkdownRenderer) invalidate(accumulated string) {
	s.stableRendered = ""
	s.stableCount = 0
	s.lastAccumulated = accumulated
}

// Reset clears all cached state. Call it when a new streaming message starts so
// stale rendered lines from the previous message are not reused.
func (s *StreamingMarkdownRenderer) Reset() {
	s.stableRendered = ""
	s.stableCount = 0
	s.lastAccumulated = ""
	s.inCodeBlock = false
	s.codeBlockBuffer.Reset()
	s.windowLines = nil
	s.windowLineParts = nil
	s.windowLineBytes = 0
	s.windowDataBytes = 0
	s.windowNewlines = 0
	s.windowLineOpen = false
	s.windowWidth = 0
	s.windowHidden = false
	s.windowFinished = false
	s.column = 0
	s.fenceLineState = 0
	s.fenceTicks = 0
	s.lineIsFence = false
	s.graphemePending = ""
	s.utf8Pending = nil
	s.controlMode = terminalControlNone
	s.controlBytes = nil
	s.controlEsc = false
	s.discardingControl = false
	s.sourceTailStart = 0
	s.sourceTailSize = 0
	s.sourceUnitsStart = 0
	s.sourceUnitsSize = 0
	s.sourceTailHidden = false
	clear(s.sourceTail)
	clear(s.sourceTailUnits)
	clear(s.controlBytes)
	clear(s.utf8Pending)
	s.inputBytes = 0
	s.parserBytes = 0
	s.markdownRenderCalls = 0
	s.markdownRenderBytes = 0
}
