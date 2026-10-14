package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptViewportSearchAndScroll(t *testing.T) {
	rows := makeTranscriptViewportRows(10_000)
	rows[12].text = "repeated search text"
	rows[8_001].text = "repeated search text"
	v := newTranscriptViewport(80, 5)

	require.NoError(t, v.SetRows(rows, "loaded session s, branch main"))
	window := v.Window()
	require.Len(t, window.lines, 5)
	assert.Equal(t, 9_995, window.startLine)
	assert.Equal(t, "entry-09995", window.lines[0].rowKey)
	assert.Equal(t, "loaded session s, branch main", window.loadedScope)
	assert.True(t, v.FollowTail())

	assert.True(t, v.Select("entry-09999"))
	first, ok := v.Search("repeated search text", "")
	require.True(t, ok)
	assert.Equal(t, "entry-00012", first)
	assert.Equal(t, first, v.SearchHitKey())
	assert.Equal(t, "entry-09999", v.SelectedKey(), "selection remains independent from search")
	assert.False(t, v.FollowTail(), "navigating to an offscreen hit leaves the live tail")
	assert.Equal(t, "entry-00012", v.Anchor().rowKey)

	second, ok := v.Search("repeated search text", first)
	require.True(t, ok)
	assert.Equal(t, "entry-08001", second, "duplicate text resolves to the next distinct keyed row")
	wrapped, ok := v.Search("repeated search text", second)
	require.True(t, ok)
	assert.Equal(t, first, wrapped, "search cycles by key, not by copied result text")

	_, ok = v.Search("no matching row", "")
	assert.False(t, ok)
	assert.Empty(t, v.SearchHitKey())
	_, ok = v.Search("", "")
	assert.False(t, ok, "empty query clears search rather than matching every row")
	assert.Empty(t, v.SearchHitKey())
	_, ok = v.Search("\x1b[2J", "")
	assert.False(t, ok, "a query removed by terminal sanitization is still empty")
	assert.Empty(t, v.SearchHitKey())
	assert.Equal(t, "loaded session s, branch main", v.Window().loadedScope)

	v.ScrollLines(-10_000)
	assert.Equal(t, 0, v.Window().startLine)
	assert.Equal(t, "entry-00000", v.Anchor().rowKey)
	v.ScrollLines(5_000)
	assert.Equal(t, 5_000, v.Window().startLine)
	assert.Equal(t, "entry-05000", v.Anchor().rowKey)
	v.End()
	assert.True(t, v.FollowTail())
	assert.Equal(t, 9_995, v.Window().startLine)
}

func TestTranscriptViewportFollowTailAndAppend(t *testing.T) {
	rows := makeTranscriptViewportRows(20)
	v := newTranscriptViewport(40, 4)
	require.NoError(t, v.SetRows(rows, "loaded snapshot"))
	assert.Equal(t, 16, v.Window().startLine)
	assert.True(t, v.FollowTail())

	v.ScrollLines(-2)
	anchor := v.Anchor()
	assert.Equal(t, "entry-00014", anchor.rowKey)
	assert.False(t, v.FollowTail())

	rows = append(rows, transcriptViewportRow{key: "entry-00020", text: "row 20", revision: 1})
	require.NoError(t, v.SetRows(rows, "loaded snapshot"))
	assert.Equal(t, anchor, v.Anchor(), "append preserves a manually scrolled source position")
	assert.False(t, v.FollowTail(), "append does not re-enable follow-tail after manual scroll")
	assert.Equal(t, 14, v.Window().startLine)

	v.End()
	assert.True(t, v.FollowTail())
	assert.Equal(t, 17, v.Window().startLine)
	assert.Equal(t, "entry-00017", v.Anchor().rowKey)

	assert.True(t, v.Select("entry-00002"))
	assert.Equal(t, "entry-00002", v.SelectedKey())
	assert.Equal(t, "entry-00002", v.Window().lines[0].rowKey)
	assert.False(t, v.FollowTail())
}

func TestTranscriptViewportSelectDoesNotMovePartiallyVisibleRow(t *testing.T) {
	rows := []transcriptViewportRow{
		{key: "long", text: "zero\none\ntwo\nthree\nfour", revision: 1},
		{key: "tail", text: "tail", revision: 1},
	}
	v := newTranscriptViewport(20, 3)
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	initial := v.Anchor()
	assert.Equal(t, "long", initial.rowKey, "the tail viewport shows the end of the long row")

	assert.True(t, v.Select("long"))
	assert.Equal(t, initial, v.Anchor(), "a row already intersecting the viewport does not jump to its top")
}

func TestTranscriptViewportSelectingVisibleRowPausesFollowTail(t *testing.T) {
	rows := makeTranscriptViewportRows(5)
	v := newTranscriptViewport(40, 3)
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	assert.Equal(t, "entry-00002", v.Window().lines[0].rowKey)

	anchor := v.Anchor()
	require.True(t, v.Select("entry-00003"), "a visible non-tail row can be selected")
	assert.False(t, v.FollowTail(), "explicit selection pauses follow-tail even when the row is visible")
	assert.Equal(t, anchor, v.Anchor(), "selecting a visible row does not move the viewport")

	rows = append(rows,
		transcriptViewportRow{key: "entry-00005", text: "row 5", revision: 1},
		transcriptViewportRow{key: "entry-00006", text: "row 6", revision: 1},
	)
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	assert.Equal(t, anchor, v.Anchor(), "appends preserve the selection-time viewport")
	assert.Equal(t, "entry-00003", v.Window().lines[1].rowKey)
	assert.False(t, v.FollowTail())
}

func TestTranscriptViewportVisibleSearchHitPausesFollowTail(t *testing.T) {
	rows := makeTranscriptViewportRows(5)
	rows[3].text = "find this visible result"
	v := newTranscriptViewport(40, 3)
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	assert.Equal(t, "entry-00002", v.Window().lines[0].rowKey)

	anchor := v.Anchor()
	hit, ok := v.Search("visible result", "")
	require.True(t, ok)
	assert.Equal(t, "entry-00003", hit)
	assert.False(t, v.FollowTail(), "a successful search pauses follow-tail even when its hit is visible")
	assert.Equal(t, anchor, v.Anchor(), "finding a visible hit does not move the viewport")

	rows = append(rows,
		transcriptViewportRow{key: "entry-00005", text: "row 5", revision: 1},
		transcriptViewportRow{key: "entry-00006", text: "row 6", revision: 1},
	)
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	assert.Equal(t, anchor, v.Anchor(), "appends preserve the search-time viewport")
	assert.Contains(t, windowLineTexts(v.Window()), "find this visible result")
	assert.False(t, v.FollowTail())
}

func TestTranscriptViewportRemovedTailAnchorClampsToLastSurvivor(t *testing.T) {
	rows := makeTranscriptViewportRows(10)
	v := newTranscriptViewport(40, 2)
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	v.ScrollLines(-1)
	assert.Equal(t, "entry-00007", v.Anchor().rowKey)

	rows = rows[:5]
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	assert.Equal(t, "entry-00004", v.Anchor().rowKey, "a removed anchor clamps to the nearest surviving ordinal")
	assert.Equal(t, 4, v.Window().startLine)
	assert.Equal(t, "entry-00004", v.Window().lines[0].rowKey)
}

func TestTranscriptViewportResizeKeepsLogicalAnchor(t *testing.T) {
	text := "\x1b[31mA世界👩🏽‍💻B\x1b[0m\nnext\tline\x1b]52;c;payload\a"
	v := newTranscriptViewport(4, 2)
	require.NoError(t, v.SetRows([]transcriptViewportRow{{
		key:      "entry-cjk",
		text:     text,
		revision: 1,
	}}, "loaded viewport sample"))
	assert.Equal(t, len(strings.Split(wrapCellText(text, 4), "\n")), v.rowMetadata[0].lineCount)
	assert.Len(t, transcriptViewportSourceLineOffsets(text, 4), v.rowMetadata[0].lineCount)

	v.ScrollLines(-2)
	anchor := v.Anchor()
	assert.Equal(t, "entry-cjk", anchor.rowKey)
	assertViewportOffsetAtGraphemeBoundary(t, text, anchor.sourceOffset)

	v.Resize(3, 2)
	assert.Equal(t, anchor, v.Anchor(), "width reflow preserves the logical source location")
	assert.Equal(t, len(strings.Split(wrapCellText(text, 3), "\n")), v.rowMetadata[0].lineCount)
	assert.Len(t, transcriptViewportSourceLineOffsets(text, 3), v.rowMetadata[0].lineCount)
	window := v.Window()
	require.NotEmpty(t, window.lines)
	for _, line := range window.lines {
		assert.LessOrEqual(t, cellWidth(line.text), 3)
		assert.NotContains(t, line.text, "payload", "unsafe controls must be removed before rendering")
	}
	assert.Equal(t, "loaded viewport sample", window.loadedScope)
}

func TestTranscriptViewportWindowAndCacheBounds(t *testing.T) {
	rows := makeTranscriptViewportRows(1_000)
	v := newTranscriptViewport(80, 3)
	require.NoError(t, v.SetRows(rows, "loaded window"))
	window := v.Window()
	require.Len(t, window.lines, 3)
	assert.LessOrEqual(t, len(v.renderCache), 3+transcriptViewportOverscanLines)

	cache := append([]transcriptViewportLine(nil), v.renderCache...)
	second := v.Window()
	assert.Equal(t, cache, v.renderCache, "stable windows reuse bounded rendered fragments")
	assert.Equal(t, window.lines, second.lines)

	// A large real terminal remains at its requested dimensions; the model does
	// not silently clamp a valid height or width to an arbitrary product limit.
	v.Resize(300, 300)
	window = v.Window()
	require.Len(t, window.lines, 300)
	assert.Equal(t, 300, v.width)
	assert.Equal(t, 300, v.height)
	assert.LessOrEqual(t, len(v.renderCache), 300+transcriptViewportOverscanLines)

	anchor := v.Anchor()
	v.Resize(0, 10)
	assert.Empty(t, v.Window().lines)
	assert.Equal(t, anchor, v.Anchor(), "empty dimensions do not discard the logical anchor")
	v.Resize(10, 0)
	assert.Empty(t, v.Window().lines)
	assert.Equal(t, anchor, v.Anchor())
	v.Resize(-1, -1)
	assert.Empty(t, v.Window().lines)
	assert.Equal(t, anchor, v.Anchor())
	assert.Equal(t, "loaded window", v.Window().loadedScope)
}

func TestTranscriptViewportReusesUnchangedRenderedRows(t *testing.T) {
	rows := makeTranscriptViewportRows(30)
	v := newTranscriptViewport(80, 3)
	require.NoError(t, v.SetRows(rows, "loaded window"))
	_ = v.Window()
	before := viewportCacheVersions(v.cacheKeys)

	rows[0].text = "updated offscreen row"
	rows[0].revision++
	require.NoError(t, v.SetRows(rows, "loaded window"))
	_ = v.Window()
	afterOffscreenUpdate := viewportCacheVersions(v.cacheKeys)
	assert.Equal(t, before, afterOffscreenUpdate, "offscreen changes do not invalidate visible rendered fragments")

	rows[29].text = "updated visible row"
	require.NoError(t, v.SetRows(rows, "loaded window"))
	window := v.Window()
	afterVisibleUpdate := viewportCacheVersions(v.cacheKeys)
	for key, version := range afterOffscreenUpdate {
		if key == "entry-00029" {
			assert.NotEqual(t, version, afterVisibleUpdate[key], "changed visible row gets a fresh rendered fragment")
			continue
		}
		assert.Equal(t, version, afterVisibleUpdate[key], "unchanged visible rows keep their rendered fragments")
	}
	assert.Contains(t, window.lines[len(window.lines)-1].text, "updated visible row")
}

func viewportCacheVersions(keys []transcriptViewportCacheKey) map[string]uint64 {
	versions := make(map[string]uint64, len(keys))
	for _, key := range keys {
		versions[key.key] = key.contentVersion
	}
	return versions
}

func TestTranscriptViewportSetRowsReusesMetadataAndRejectsBadKeys(t *testing.T) {
	rows := []transcriptViewportRow{
		{key: "a", text: "first\nsecond", revision: 1},
		{key: "b", text: "世界", revision: 1},
	}
	v := newTranscriptViewport(10, 2)
	require.NoError(t, v.SetRows(rows, "loaded snapshot"))
	_ = v.Window()
	metaA := v.rowMetadata[0]
	metaB := v.rowMetadata[1]
	require.True(t, v.cacheValid)

	reordered := []transcriptViewportRow{rows[1], rows[0]}
	require.NoError(t, v.SetRows(reordered, "loaded snapshot"))
	assert.Same(t, metaB, v.rowMetadata[0], "keyed line-count metadata survives row reorder")
	assert.Same(t, metaA, v.rowMetadata[1])

	changed := []transcriptViewportRow{
		{key: "b", text: "世界 expanded", revision: 2, expanded: true},
		rows[0],
	}
	require.NoError(t, v.SetRows(changed, "loaded snapshot"))
	assert.NotSame(t, metaB, v.rowMetadata[0], "revision/content change invalidates line-count metadata")
	assert.Same(t, metaA, v.rowMetadata[1])

	before := v.Window()
	valid := []transcriptViewportRow{{key: "safe", text: "still loaded", revision: 1}}
	require.Error(t, v.SetRows([]transcriptViewportRow{{text: "missing key"}}, "loaded snapshot"))
	require.Error(t, v.SetRows([]transcriptViewportRow{{key: "dup"}, {key: "dup"}}, "loaded snapshot"))
	require.Error(t, v.SetRows(valid, ""), "every search result must carry an explicit loaded-scope label")
	assert.Equal(t, before.lines, v.Window().lines, "invalid replacement is atomic")
}

func TestTranscriptViewportUpdateRowReindexesOnlyChangedRow(t *testing.T) {
	rows := makeTranscriptViewportRows(10_000)
	rows[4_999].text = "selected anchor"
	v := newTranscriptViewport(16, 3)
	require.NoError(t, v.SetRows(rows, "loaded session s, branch main"))
	hit, found := v.Search("selected anchor", "")
	require.True(t, found)
	require.Equal(t, "entry-04999", hit)
	require.True(t, v.Select("entry-04999"))
	v.ScrollLines(-1_000)
	anchor := v.Anchor()
	_ = v.Window()
	before := viewportCacheVersions(v.cacheKeys)

	updated := transcriptViewportRow{key: "entry-04999", text: strings.TrimSuffix(strings.Repeat("changed\n", 50), "\n"), revision: 2}
	require.NoError(t, v.updateRow(updated))
	assert.Equal(t, anchor.rowKey, v.Anchor().rowKey)
	assert.Equal(t, anchor.sourceOffset, v.Anchor().sourceOffset)
	assert.Equal(t, "entry-04999", v.SelectedKey())
	assert.Equal(t, "entry-04999", v.SearchHitKey())
	assert.Equal(t, "loaded session s, branch main", v.Window().loadedScope)
	assert.Equal(t, 50, v.rowMetadata[v.indexByKey[updated.key]].lineCount)
	assert.Equal(t, 10_049, v.totalLines, "the indexed total includes only the changed row's line-count delta")
	after := viewportCacheVersions(v.cacheKeys)
	for key, version := range before {
		if key == updated.key {
			assert.NotEqual(t, version, after[key], "the visible updated row gets a new cache version")
			continue
		}
		assert.Equal(t, version, after[key], "unmodified visible rows retain cached fragments")
	}

	require.ErrorIs(t, v.updateRow(transcriptViewportRow{key: updated.key, text: "stale", revision: 1}), errTranscriptViewportStaleRevision)
	assert.Equal(t, updated.text, v.rows[v.indexByKey[updated.key]].text, "a stale revision cannot mutate the row")
}

func TestTranscriptViewportEmptyAndCellSafeRows(t *testing.T) {
	v := newTranscriptViewport(5, 3)
	require.NoError(t, v.SetRows(nil, "loaded empty snapshot"))
	window := v.Window()
	assert.Empty(t, window.lines)
	assert.Equal(t, "loaded empty snapshot", window.loadedScope)
	_, ok := v.Search("anything", "")
	assert.False(t, ok)

	rows := []transcriptViewportRow{
		{key: "cjk", text: "世界\tA👩🏽‍💻B\nnext", revision: 1},
		{key: "controls", text: "safe\x1b[2Jtext\x1b]52;c;payload\a", revision: 1},
	}
	require.NoError(t, v.SetRows(rows, "loaded projection"))
	for _, row := range rows {
		want := len(strings.Split(wrapCellText(row.text, 5), "\n"))
		assert.Equal(t, want, v.rowMetadata[v.indexByKey[row.key]].lineCount)
		assert.Len(t, transcriptViewportSourceLineOffsets(row.text, 5), want)
	}
	for _, line := range v.Window().lines {
		assert.LessOrEqual(t, cellWidth(line.text), 5)
		assert.Equal(t, line.text, safeTerminalText(line.text), "rendered rows contain only permitted terminal sequences")
		assert.NotContains(t, line.text, "payload")
	}
}

func TestTranscriptViewportWrapMatchesCellHelper(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		width int
	}{
		{name: "plain", text: "ASCII text\nnext", width: 5},
		{name: "cjk graphemes", text: "世界👩🏽‍💻B", width: 3},
		{name: "tabs", text: "a\tb", width: 4},
		{name: "sgr", text: "\x1b[31mred 世界\x1b[0m", width: 5},
		{name: "safe link", text: "\x1b]8;;https://example.test\x1b\\label\x1b]8;;\x1b\\", width: 4},
		{name: "unsafe controls", text: "a\x1b[2Jb\x1b]52;c;payload\a", width: 4},
		{name: "incomplete control", text: "printable\x1b]52;c;", width: 8},
		{name: "non-positive width", text: "\x1b[31mplain\x1b[0m", width: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, wrapCellText(tc.text, tc.width), transcriptViewportWrap(tc.text, tc.width))
		})
	}
}

func TestTranscriptViewportNarrowWidthOffsetsMatchActualWrappedGraphemes(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "variation selector", text: "✈️x"},
		{name: "regional indicator flag", text: "🇨🇳x"},
		{name: "skin tone and ZWJ", text: "👩🏽‍💻x"},
		{name: "ANSI-styled wide grapheme", text: "\x1b[31m世\x1b[0mx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const width = 1
			wrapped := transcriptViewportWrap(tc.text, width)
			assert.Equal(t, wrapCellText(tc.text, width), wrapped)
			lines := strings.Split(wrapped, "\n")
			offsets := transcriptViewportSourceLineOffsets(tc.text, width)
			assert.Len(t, offsets, len(lines), "source-offset metadata must describe the actual wrap output")

			v := newTranscriptViewport(width, len(lines))
			require.NoError(t, v.SetRows([]transcriptViewportRow{{key: "narrow", text: tc.text, revision: 1}}, "loaded projection"))
			assert.Equal(t, len(lines), v.rowMetadata[0].lineCount)
			window := v.Window()
			require.Len(t, window.lines, len(lines))
			for i, line := range window.lines {
				assert.Equal(t, lines[i], line.text, "the visible row must retain every line returned by the wrapper")
				assert.Equal(t, offsets[i], line.sourceOffset)
				assertViewportOffsetAtGraphemeBoundary(t, tc.text, line.sourceOffset)
			}
			assert.Contains(t, strings.Join(windowLineTexts(window), "\n"), "x", "trailing source text must not be hidden by stale line counts")
		})
	}
}

func TestTranscriptViewportSourceRowsAreCopied(t *testing.T) {
	rows := []transcriptViewportRow{{key: "entry", text: "original", revision: 1}}
	v := newTranscriptViewport(80, 2)
	require.NoError(t, v.SetRows(rows, "loaded snapshot"))
	rows[0] = transcriptViewportRow{key: "changed", text: "caller mutation", revision: 2}

	window := v.Window()
	require.Len(t, window.lines, 1)
	assert.Equal(t, "entry", window.lines[0].rowKey)
	assert.Contains(t, window.lines[0].text, "original")
}

func makeTranscriptViewportRows(n int) []transcriptViewportRow {
	rows := make([]transcriptViewportRow, n)
	for i := range rows {
		rows[i] = transcriptViewportRow{
			key:      fmt.Sprintf("entry-%05d", i),
			text:     fmt.Sprintf("row %05d", i),
			revision: 1,
		}
	}
	return rows
}

func assertViewportOffsetAtGraphemeBoundary(t *testing.T, source string, offset int) {
	t.Helper()
	safe := safeTerminalText(source)
	require.GreaterOrEqual(t, offset, 0)
	require.LessOrEqual(t, offset, len(safe))

	boundaries := map[int]struct{}{0: {}}
	parser := ansi.NewParser()
	state := ansi.NormalState
	position := 0
	for len(safe) > 0 {
		_, _, n, nextState := ansi.GraphemeWidth.DecodeSequenceInString(safe, state, parser)
		if n <= 0 {
			n = 1
			nextState = ansi.NormalState
		}
		position += n
		boundaries[position] = struct{}{}
		safe = safe[n:]
		state = nextState
	}
	_, ok := boundaries[offset]
	assert.True(t, ok, "offset %d splits an ANSI token or Unicode grapheme", offset)
}

func BenchmarkTranscriptViewportWindow(b *testing.B) {
	for _, count := range []int{1_000, 5_000, 10_000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			rows := makeTranscriptViewportRows(count)
			v := newTranscriptViewport(100, 40)
			if err := v.SetRows(rows, "loaded benchmark projection"); err != nil {
				b.Fatal(err)
			}
			v.ScrollLines(-(count / 2))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%2 == 0 {
					v.ScrollLines(1)
				} else {
					v.ScrollLines(-1)
				}
				_ = v.Window()
			}
		})
	}
}

func TestTranscriptViewportSearchScopeDoesNotIncludeMissingRows(t *testing.T) {
	v := newTranscriptViewport(80, 3)
	require.NoError(t, v.SetRows([]transcriptViewportRow{{key: "loaded", text: "visible source", revision: 1}}, "loaded branch only"))
	_, ok := v.Search("future page", "")
	assert.False(t, ok, "viewport search is limited to currently loaded rows")
	assert.Equal(t, "loaded branch only", v.Window().loadedScope)
	assert.NotContains(t, strings.Join(windowLineTexts(v.Window()), "\n"), "future page")
}

func windowLineTexts(window transcriptViewportWindow) []string {
	texts := make([]string, len(window.lines))
	for i, line := range window.lines {
		texts[i] = line.text
	}
	return texts
}
