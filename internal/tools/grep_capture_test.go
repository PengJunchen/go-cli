package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGrepMatchCaptureWholeRecordBudgets(t *testing.T) {
	one := match{File: "a", Line: 1, Content: "x"} // exactly six serialized bytes

	t.Run("negative limits clamp to zero", func(t *testing.T) {
		capture := newGrepMatchCapture(-1, -1)
		assert.False(t, capture.Add(one))
		assert.Empty(t, capture.Matches())
		assert.True(t, capture.Truncated())
		assert.Zero(t, capture.RetainedBytes())
	})

	t.Run("exact limits wait for an omitted record", func(t *testing.T) {
		capture := newGrepMatchCapture(1, 6)
		assert.True(t, capture.Add(one))
		assert.False(t, capture.Truncated())
		assert.Equal(t, 6, capture.RetainedBytes())
		assert.False(t, capture.Add(one))
		assert.True(t, capture.Truncated())
		assert.Len(t, capture.Matches(), 1)
	})

	t.Run("zero limits truncate only on a real match", func(t *testing.T) {
		capture := newGrepMatchCapture(0, 0)
		assert.False(t, capture.Truncated())
		assert.False(t, capture.Add(one))
		assert.True(t, capture.Truncated())
	})

	t.Run("byte overflow drops the entire record", func(t *testing.T) {
		capture := newGrepMatchCapture(5, 5)
		assert.False(t, capture.Add(one))
		assert.Empty(t, capture.Matches())
		assert.Zero(t, capture.RetainedBytes())
		assert.True(t, capture.Truncated())
	})

	t.Run("quoted path bytes are charged", func(t *testing.T) {
		path := "a\nb"
		want := strconv.Quote(path) + ":1:x\n"
		capture := newGrepMatchCapture(1, len(want))
		assert.True(t, capture.Add(match{File: path, Line: 1, Content: "x"}))
		assert.Equal(t, len(want), capture.RetainedBytes())
		assert.Equal(t, strconv.Quote(path)+":1:x", buildMatchOutput(capture.Matches()))
	})
}

func TestGrepRipgrepJSONWriterSplitTextBytesAndFinalFrame(t *testing.T) {
	pathBytes := []byte{'a', ':', 0xff, '\n', 'b'}
	pathEncoded := base64.StdEncoding.EncodeToString(pathBytes)
	lineEncoded := base64.StdEncoding.EncodeToString([]byte("needle\r\n"))
	frame := []byte(fmt.Sprintf(
		`{"type":"match","data":{"path":{"bytes":%q},"lines":{"bytes":%q},"line_number":7}}`,
		pathEncoded, lineEncoded,
	))
	capture := newGrepMatchCapture(2, 200)
	writer := newGrepRipgrepJSONWriter(capture)
	for offset := 0; offset < len(frame); offset += 3 {
		end := offset + 3
		if end > len(frame) {
			end = len(frame)
		}
		n, err := writer.Write(frame[offset:end])
		require.NoError(t, err)
		require.Equal(t, end-offset, n)
	}
	require.NoError(t, writer.Finish())
	require.Equal(t, []match{{File: string(pathBytes), Line: 7, Content: "needle"}}, capture.Matches())
	assert.Equal(t, strconv.Quote(string(pathBytes))+":7:needle", buildMatchOutput(capture.Matches()))

	finalCapture := newGrepMatchCapture(1, 100)
	finalWriter := newGrepRipgrepJSONWriter(finalCapture)
	finalFrame := []byte(`{"type":"match","data":{"path":{"text":"final.txt"},"lines":{"text":"ends\r"},"line_number":1}}`)
	_, err := finalWriter.Write(finalFrame)
	require.NoError(t, err)
	require.NoError(t, finalWriter.Finish())
	require.Equal(t, "ends", finalCapture.Matches()[0].Content)
}

func TestGrepRipgrepJSONWriterProtocolAndFrameLimits(t *testing.T) {
	for name, frame := range map[string]string{
		"invalid JSON":         "not-json",
		"missing match fields": `{"type":"match","data":{"line_number":1}}`,
		"invalid base64 bytes": `{"type":"match","data":{"path":{"bytes":"%%%"},"lines":{"text":"needle"},"line_number":1}}`,
		"missing event type":   `{"data":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			writer := newGrepRipgrepJSONWriter(newGrepMatchCapture(2, 100))
			_, err := writer.Write([]byte(frame + "\n"))
			require.NoError(t, err, "capture errors are latched so the pipe can drain")
			require.ErrorIs(t, writer.Finish(), errGrepOutputProtocol)
		})
	}

	writer := newGrepRipgrepJSONWriter(newGrepMatchCapture(1, 100))
	oversized := bytes.Repeat([]byte{'x'}, grepMaxJSONFrameBytes+1)
	n, err := writer.Write(oversized)
	require.NoError(t, err)
	require.Equal(t, len(oversized), n)
	require.ErrorIs(t, writer.CaptureError(), errGrepCaptureLimit)
	assert.Nil(t, writer.frame, "oversized event storage is released while later bytes drain")
	n, err = writer.Write([]byte("later bytes are drained"))
	require.NoError(t, err)
	require.Equal(t, len("later bytes are drained"), n)
	require.ErrorIs(t, writer.Finish(), errGrepCaptureLimit)
}

func TestGrepPureGoReadDirOneDFSObservedOrderAndBoundedWidth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "nested-dir"), 0o700))
	for name, content := range map[string]string{
		"z-last.txt":  "needle z\n",
		"a-first.txt": "needle a\n",
		filepath.Join("nested-dir", "m-middle.txt"): "needle nested\n",
		filepath.Join("nested-dir", "ignore.bin"):   "needle ignored by glob\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}

	want := observedGrepFiles(t, root, "*.txt")
	tracker := &grepDirectoryTracker{}
	open := func(path string) (grepDirectoryReader, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		tracker.active++
		if tracker.active > tracker.maxActive {
			tracker.maxActive = tracker.active
		}
		return &trackedGrepDirectory{File: file, tracker: tracker}, nil
	}
	capture := newGrepMatchCapture(2, 1<<20)
	truncated, err := grepPureGoWithOpen(context.Background(), mustCompile(t, "needle"), root, "*.txt", PathWhitelist{}, capture, open)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Zero(t, tracker.active, "all directory handles close after traversal")
	require.LessOrEqual(t, tracker.maxActive, 2, "active handles track DFS depth, not sibling count")
	assert.Zero(t, tracker.badReadDirN, "the walker must request one directory entry at a time")
	assert.LessOrEqual(t, tracker.maxReturned, 1)
	require.GreaterOrEqual(t, len(want), 3)
	require.Len(t, capture.Matches(), 2)
	assert.Equal(t, want[:2], matchPaths(capture.Matches()))
	assert.LessOrEqual(t, capture.RetainedBytes(), 1<<20)

	allCapture := newGrepMatchCapture(10, 1<<20)
	allTruncated, err := grepPureGo(context.Background(), mustCompile(t, "needle"), root, "*.txt", PathWhitelist{}, allCapture)
	require.NoError(t, err)
	assert.False(t, allTruncated)
	assert.Equal(t, want, matchPaths(allCapture.Matches()), "glob applies to files without pruning directories")
}

func TestGrepPureGoRootFileAndMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root.txt")
	require.NoError(t, os.WriteFile(root, []byte("needle\n"), 0o600))
	capture := newGrepMatchCapture(10, 1024)
	truncated, err := grepPureGo(context.Background(), mustCompile(t, "needle"), root, "*.txt", PathWhitelist{}, capture)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Equal(t, []match{{File: root, Line: 1, Content: "needle"}}, capture.Matches())

	missing := filepath.Join(t.TempDir(), "missing")
	_, err = grepPureGo(context.Background(), mustCompile(t, "needle"), missing, "", PathWhitelist{}, newGrepMatchCapture(1, 100))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestGrepPureGoRootOpenErrorPreservesCauseAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	cause := errors.New("injected root open failure")
	var opened []string
	capture := newGrepMatchCapture(10, 1024)
	truncated, err := grepPureGoWithOpen(context.Background(), mustCompile(t, "needle"), root, "", PathWhitelist{}, capture, func(path string) (grepDirectoryReader, error) {
		opened = append(opened, path)
		return nil, cause
	})
	require.ErrorIs(t, err, cause)
	assert.Contains(t, err.Error(), "open root directory")
	assert.Contains(t, err.Error(), root)
	assert.Equal(t, []string{root}, opened, "the root error must not be swallowed as a best-effort descendant failure")
	assert.Empty(t, capture.Matches(), "a requested-root open failure cannot return partial matches")
	assert.False(t, capture.Truncated())
	assert.False(t, truncated)

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-root")
	result, executeErr := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true)).Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "needle", "path": "missing-root"},
	})
	require.ErrorIs(t, executeErr, os.ErrNotExist)
	require.NotNil(t, result)
	assert.Equal(t, "", result.Output)
	assert.Nil(t, result.Metadata, "failed Execute results must not claim successful search metadata")
	assert.Contains(t, executeErr.Error(), missing)
}

func TestGrepPureGoOutputBudgetUsesWholePrefixAndExactBoundary(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "matches.txt")
	require.NoError(t, os.WriteFile(file, []byte("needle first\nneedle second\n"), 0o600))
	firstRecord := file + ":1:needle first\n"
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true), WithGrepMaxOutput(len(firstRecord)))
	result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle", "path": file}})
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSuffix(firstRecord, "\n")+"\n[results truncated]", result.Output)
	assert.Equal(t, 1, result.Metadata["matches"])
	assert.Equal(t, true, result.Metadata["truncated"])

	onlyOne := filepath.Join(dir, "one.txt")
	require.NoError(t, os.WriteFile(onlyOne, []byte("needle only\n"), 0o600))
	exact := onlyOne + ":1:needle only\n"
	exactTool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true), WithGrepMaxOutput(len(exact)))
	exactResult, err := exactTool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle", "path": onlyOne}})
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSuffix(exact, "\n"), exactResult.Output)
	assert.Equal(t, false, exactResult.Metadata["truncated"], "an exact budget is complete until a record is omitted")
}

func TestGrepPureGoDescendantFailureAndEOFCancellation(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(child, "nested.txt"), []byte("needle\n"), 0o600))
	open := func(path string) (grepDirectoryReader, error) {
		if path == child {
			return nil, os.ErrPermission
		}
		return os.Open(path)
	}
	truncated, err := grepPureGoWithOpen(context.Background(), mustCompile(t, "needle"), root, "", PathWhitelist{}, newGrepMatchCapture(10, 1024), open)
	require.NoError(t, err)
	assert.True(t, truncated, "a skipped descendant must be reported as incomplete")

	empty := t.TempDir()
	leaf := empty
	for i := 0; i < 6; i++ {
		leaf = filepath.Join(leaf, fmt.Sprintf("depth-%d", i))
		require.NoError(t, os.Mkdir(leaf, 0o700))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker := &cancelingGrepDirectoryTracker{}
	beforeFDs := countTestFDs(t)
	openCancel := func(path string) (grepDirectoryReader, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		tracker.opened++
		return &cancelingGrepDirectory{File: file, cancel: cancel, tracker: tracker}, nil
	}
	_, err = grepPureGoWithOpen(ctx, mustCompile(t, "needle"), empty, "", PathWhitelist{}, newGrepMatchCapture(1, 100), openCancel)
	require.ErrorIs(t, err, context.Canceled, "cancellation racing with empty EOF is an error")
	require.Equal(t, 7, tracker.opened, "the walk descends through the full directory chain")
	assert.Equal(t, tracker.opened, tracker.closed, "every active directory handle closes while unwinding")
	assert.LessOrEqual(t, countTestFDs(t), beforeFDs+1, "the canceled deep walk returns directory descriptors to baseline")
}

func TestGrepPureGoRootReadAndCloseErrorsPreserveCause(t *testing.T) {
	for _, failure := range []string{"read", "close"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			cause := errors.New("injected root " + failure + " failure")
			var tracked *failingGrepDirectory
			open := func(path string) (grepDirectoryReader, error) {
				file, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				tracked = &failingGrepDirectory{File: file, failure: failure, cause: cause}
				return tracked, nil
			}
			_, err := grepPureGoWithOpen(context.Background(), mustCompile(t, "needle"), root, "", PathWhitelist{}, newGrepMatchCapture(1, 100), open)
			require.ErrorIs(t, err, cause)
			require.NotNil(t, tracked)
			assert.True(t, tracked.closed)
		})
	}
}

func TestGrepRealRipgrepPreservesColonAndLFPathsAndScanLines(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep is not installed")
	}
	base := t.TempDir()
	for _, name := range []string{"odd:colon.txt", "odd\nname.txt"} {
		dir := filepath.Join(base, strconv.Quote(name))
		require.NoError(t, os.Mkdir(dir, 0o700))
		file := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(file, []byte("needle\n"), 0o600))
		matches, err := grepWithRipgrep(context.Background(), mustCompile(t, "needle"), file, "")
		require.NoError(t, err)
		require.Equal(t, []match{{File: file, Line: 1, Content: "needle"}}, matches)
		if strings.Contains(name, "\n") {
			result, err := NewGrepTool(WithGrepWorkdir(dir)).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle", "path": dir}})
			require.NoError(t, err)
			assert.Equal(t, strconv.Quote(file)+":1:needle", result.Output)
			assert.Equal(t, 1, result.Metadata["matches"])
		}
	}

	file := filepath.Join(base, "line-endings.txt")
	require.NoError(t, os.WriteFile(file, []byte("needle first\r\nneedle final\r"), 0o600))
	realMatches, err := grepWithRipgrep(context.Background(), mustCompile(t, "needle"), file, "")
	require.NoError(t, err)
	goCapture := newGrepMatchCapture(10, 1024)
	_, err = grepPureGo(context.Background(), mustCompile(t, "needle"), file, "", PathWhitelist{}, goCapture)
	require.NoError(t, err)
	assert.Equal(t, []string{"needle first", "needle final"}, matchContents(realMatches))
	assert.Equal(t, matchContents(goCapture.Matches()), matchContents(realMatches), "this assertion covers CRLF/final-CR line normalization only")
}

func TestGrepRipgrepSaturationDrainsAndKeepsWholePrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-local shell helper requires POSIX")
	}
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.txt")
	secondPath := filepath.Join(root, "second.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("needle fallback one\n"), 0o600))
	require.NoError(t, os.WriteFile(secondPath, []byte("needle fallback two\n"), 0o600))
	firstEvent := grepJSONEvent(firstPath, "needle first", 1)
	secondEvent := grepJSONEvent(secondPath, "needle second", 2)
	marker := filepath.Join(t.TempDir(), "drained")
	body := fmt.Sprintf("printf '%%s' %s\nsleep 0.02\nprintf '%%s\\n' %s\nprintf '%%s\\n' %s\nprintf '%%s' 'not-json-after-saturation'\nprintf done > \"$GREP_DRAIN_MARKER\"\n",
		shellQuoteGrep(firstEvent[:len(firstEvent)/2]),
		shellQuoteGrep(firstEvent[len(firstEvent)/2:]),
		shellQuoteGrep(secondEvent))
	helperDir := writeGrepShim(t, body)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREP_DRAIN_MARKER", marker)
	tool := NewGrepTool(WithGrepWorkdir(root), WithGrepMaxMatches(1))
	ms, truncated, lifecycle, started, err := tool.search(context.Background(), mustCompile(t, "needle"), root, "")
	require.NoError(t, err)
	require.True(t, started)
	require.True(t, lifecycle.readersJoined)
	require.True(t, lifecycle.containmentProven)
	require.True(t, truncated)
	require.Equal(t, []match{{File: firstPath, Line: 1, Content: "needle first"}}, ms)
	markerBytes, err := os.ReadFile(marker)
	require.NoError(t, err, "the helper completion marker must be visible before Grep returns")
	assert.Equal(t, "done", string(markerBytes))
}

func TestGrepRipgrepMalformedProtocolDoesNotFallBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-local shell helper requires POSIX")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "fallback.txt"), []byte("needle fallback\n"), 0o600))
	marker := filepath.Join(t.TempDir(), "finished")
	helperDir := writeGrepShim(t, "printf '%s\\n' 'not-json'\nprintf done > \"$GREP_PROTOCOL_MARKER\"\n")
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREP_PROTOCOL_MARKER", marker)
	result, err := NewGrepTool(WithGrepWorkdir(root)).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle"}})
	require.ErrorIs(t, err, errGrepOutputProtocol)
	require.NotNil(t, result)
	assert.Empty(t, result.Output, "malformed stdout must not fall back to Go")
	assert.FileExists(t, marker, "the malformed-stream helper must still drain and exit")
}

func TestGrepRipgrepOversizedFrameDrainsAndDoesNotFallBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-local shell helper requires POSIX")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "fallback.txt"), []byte("needle fallback\n"), 0o600))
	marker := filepath.Join(t.TempDir(), "drained")
	body := "head -c 8388609 /dev/zero\nprintf done > \"$GREP_OVERSIZE_MARKER\"\n"
	helperDir := writeGrepShim(t, body)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREP_OVERSIZE_MARKER", marker)
	result, err := NewGrepTool(WithGrepWorkdir(root)).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle"}})
	require.ErrorIs(t, err, errGrepCaptureLimit)
	require.NotNil(t, result)
	assert.Empty(t, result.Output, "oversized protocol input must not return fallback output")
	assert.FileExists(t, marker, "stdout keeps draining until the helper writes its completion marker")
}

func TestGrepRipgrepArgvAndEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-local shell helper requires POSIX")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	configPath := filepath.Join(dir, "config")
	sentinelPath := filepath.Join(dir, "sentinel")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GREP_ARGS_PATH\"\nif env | grep -q '^RIPGREP_CONFIG_PATH='; then printf present > \"$GREP_CONFIG_PATH\"; else printf '<unset>' > \"$GREP_CONFIG_PATH\"; fi\nprintf '%s' \"$GREP_SENTINEL\" > \"$GREP_SENTINEL_PATH\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rg"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREP_ARGS_PATH", argsPath)
	t.Setenv("GREP_CONFIG_PATH", configPath)
	t.Setenv("GREP_SENTINEL_PATH", sentinelPath)
	t.Setenv("GREP_SENTINEL", "unrelated-inherited")
	t.Setenv("RIPGREP_CONFIG_PATH", "hostile-config")
	_, err := grepWithRipgrep(context.Background(), mustCompile(t, "--pre=literal"), "/validated/root", "*.go")
	require.NoError(t, err)
	args, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"--no-config", "--json", "-n", "--glob=*.go", "-e", "--pre=literal", "--", "/validated/root"}, strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"))
	assert.Equal(t, "<unset>", readTestFile(t, configPath))
	assert.Equal(t, "unrelated-inherited", readTestFile(t, sentinelPath))
}

func TestGrepManagedSearchBufferAdapterStillJoins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed local process fixtures require POSIX")
	}
	printf, err := exec.LookPath("printf")
	if err != nil {
		t.Skip("printf is not installed")
	}
	output, lifecycle, err := runManagedSearchCommand(context.Background(), exec.Command(printf, "legacy helper output"))
	require.NoError(t, err)
	assert.Equal(t, "legacy helper output", string(output))
	assert.True(t, lifecycle.readersJoined)
	assert.True(t, lifecycle.containmentProven)
}

func TestGrepManagedHelperReadersJoinWithoutFDLeak(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed local process fixtures require POSIX")
	}
	helperDir := writeGrepShim(t, "exit 0\n")
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beforeFDs := countTestFDs(t)
	for i := 0; i < 5; i++ {
		capture := newGrepMatchCapture(1, 100)
		lifecycle, started, err := grepWithRipgrepJSONManaged(context.Background(), mustCompile(t, "needle"), "/root", "", capture)
		require.NoError(t, err)
		require.True(t, started)
		require.True(t, lifecycle.readersJoined)
		require.True(t, lifecycle.containmentProven)
	}
	assert.LessOrEqual(t, countTestFDs(t), beforeFDs+1, "repeated managed helpers return descriptors to baseline")
}

type grepDirectoryTracker struct {
	active      int
	maxActive   int
	maxReturned int
	badReadDirN int
}

type trackedGrepDirectory struct {
	*os.File
	tracker *grepDirectoryTracker
}

func (d *trackedGrepDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	if n != 1 {
		d.tracker.badReadDirN++
	}
	entries, err := d.File.ReadDir(n)
	if len(entries) > d.tracker.maxReturned {
		d.tracker.maxReturned = len(entries)
	}
	return entries, err
}

func (d *trackedGrepDirectory) Close() error {
	d.tracker.active--
	return d.File.Close()
}

type cancelingGrepDirectory struct {
	*os.File
	cancel  context.CancelFunc
	tracker *cancelingGrepDirectoryTracker
}

type failingGrepDirectory struct {
	*os.File
	failure string
	cause   error
	closed  bool
}

func (d *failingGrepDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.failure == "read" {
		return nil, d.cause
	}
	return d.File.ReadDir(n)
}

func (d *failingGrepDirectory) Close() error {
	d.closed = true
	_ = d.File.Close()
	if d.failure == "close" {
		return d.cause
	}
	return nil
}

func (d *cancelingGrepDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := d.File.ReadDir(n)
	if len(entries) == 0 && errors.Is(err, io.EOF) {
		d.cancel()
	}
	return entries, err
}

func (d *cancelingGrepDirectory) Close() error {
	d.tracker.closed++
	return d.File.Close()
}

type cancelingGrepDirectoryTracker struct {
	opened int
	closed int
}

func countTestFDs(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("descriptor count requires /dev/fd")
	}
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("/dev/fd is unavailable: %v", err)
	}
	names, readErr := dir.Readdirnames(-1)
	closeErr := dir.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	return len(names)
}

func observedGrepFiles(t *testing.T, dir, glob string) []string {
	t.Helper()
	var files []string
	var walk func(string)
	walk = func(path string) {
		file, err := os.Open(path)
		require.NoError(t, err)
		defer file.Close()
		for {
			entries, err := file.ReadDir(1)
			for _, entry := range entries {
				full := filepath.Join(path, entry.Name())
				info, statErr := entry.Info()
				require.NoError(t, statErr)
				if info.IsDir() {
					walk(full)
				} else if info.Mode().IsRegular() && (matchGlob(glob, entry.Name()) || matchGlob(glob, filepath.ToSlash(strings.TrimPrefix(full, dir+string(os.PathSeparator))))) {
					files = append(files, full)
				}
			}
			if errors.Is(err, io.EOF) {
				return
			}
			require.NoError(t, err)
			if len(entries) == 0 {
				return
			}
		}
	}
	walk(dir)
	return files
}

func matchPaths(matches []match) []string {
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		paths = append(paths, m.File)
	}
	return paths
}

func matchContents(matches []match) []string {
	contents := make([]string, 0, len(matches))
	for _, m := range matches {
		contents = append(contents, m.Content)
	}
	return contents
}

func grepJSONEvent(file, line string, lineNumber int) string {
	data, err := json.Marshal(map[string]any{
		"type": "match",
		"data": map[string]any{
			"path":        map[string]string{"text": file},
			"lines":       map[string]string{"text": line + "\n"},
			"line_number": lineNumber,
		},
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func writeGrepShim(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rg"), []byte("#!/bin/sh\n"+body), 0o700))
	return dir
}

func shellQuoteGrep(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(value)
}

var _ grepDirectoryReader = (*trackedGrepDirectory)(nil)
var _ grepDirectoryReader = (*cancelingGrepDirectory)(nil)
var _ grepDirectoryReader = (*failingGrepDirectory)(nil)
