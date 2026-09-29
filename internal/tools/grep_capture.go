package tools

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const grepMaxJSONFrameBytes = 8 << 20

var (
	errGrepCaptureLimit   = errors.New("tools: grep capture limit exceeded")
	errGrepOutputProtocol = errors.New("tools: invalid ripgrep output")
)

// grepMatchCapture retains only complete output records within the tool's
// effective count and serialized-byte budgets.
type grepMatchCapture struct {
	maxMatches int
	maxBytes   int
	matches    []match
	bytesUsed  int
	truncated  bool
}

func newGrepMatchCapture(maxMatches, maxBytes int) *grepMatchCapture {
	if maxMatches < 0 {
		maxMatches = 0
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	initial := maxMatches
	if initial > 64 {
		initial = 64
	}
	return &grepMatchCapture{
		maxMatches: maxMatches,
		maxBytes:   maxBytes,
		matches:    make([]match, 0, initial),
	}
}

func (c *grepMatchCapture) Add(m match) bool {
	if c.truncated {
		return false
	}
	if len(c.matches) >= c.maxMatches {
		c.truncated = true
		return false
	}
	displayPath := grepDisplayPath(m.File)
	lineNumber := strconv.Itoa(m.Line)
	remaining := c.maxBytes - c.bytesUsed
	for _, size := range []int{len(displayPath), 1, len(lineNumber), 1, len(m.Content), 1} {
		if size > remaining {
			c.truncated = true
			return false
		}
		remaining -= size
	}
	c.matches = append(c.matches, m)
	c.bytesUsed = c.maxBytes - remaining
	return true
}

func (c *grepMatchCapture) Matches() []match { return append([]match(nil), c.matches...) }
func (c *grepMatchCapture) Truncated() bool  { return c.truncated }
func (c *grepMatchCapture) RetainedBytes() int {
	return c.bytesUsed
}

func grepDisplayPath(file string) string {
	if utf8.ValidString(file) && !strings.ContainsFunc(file, unicode.IsControl) {
		return file
	}
	return strconv.Quote(file)
}

// grepWithRipgrepJSONManaged keeps JSON framing inside the existing managed
// stdout reader so capture never takes ownership of a process or goroutine.
func grepWithRipgrepJSONManaged(ctx context.Context, re *regexp.Regexp, searchPath, glob string, capture *grepMatchCapture) (managedLocalResult, bool, error) {
	args := []string{"--no-config", "--json", "-n"}
	if glob != "" {
		args = append(args, "--glob="+glob)
	}
	args = append(args, "-e", re.String(), "--", searchPath)

	cmd := exec.Command("rg", args...)
	cmd.Env = make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "RIPGREP_CONFIG_PATH") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	writer := newGrepRipgrepJSONWriter(capture)
	lifecycle, err := runManagedSearchCommandWithStdoutWriter(ctx, cmd, writer)
	if err != nil {
		return lifecycle, false, fmt.Errorf("grep: rg: %w", err)
	}
	if lifecycleErr := managedSearchLifecycleError(ctx, "grep: rg", lifecycle); lifecycleErr != nil {
		return lifecycle, true, errors.Join(lifecycleErr, writer.CaptureError())
	}
	if err := writer.Finish(); err != nil {
		return lifecycle, true, fmt.Errorf("grep: rg output: %w", err)
	}
	if lifecycle.runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(lifecycle.runErr, &exitErr) && exitErr.ExitCode() == 1 {
			return lifecycle, true, nil
		}
		return lifecycle, true, fmt.Errorf("grep: rg: %w", lifecycle.runErr)
	}
	return lifecycle, true, nil
}

type grepRipgrepJSONWriter struct {
	mu       sync.Mutex
	capture  *grepMatchCapture
	frame    []byte
	err      error
	finished bool
}

func newGrepRipgrepJSONWriter(capture *grepMatchCapture) *grepRipgrepJSONWriter {
	return &grepRipgrepJSONWriter{
		capture: capture,
		frame:   make([]byte, 0, 64*1024),
	}
}

// Write deliberately never returns a capture error: the managed owner must
// keep draining stdout and joining its readers after a bad or oversized event.
func (w *grepRipgrepJSONWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished || w.err != nil || w.capture.Truncated() {
		w.frame = nil
		return len(p), nil
	}
	for _, value := range p {
		if w.err != nil || w.capture.Truncated() {
			w.frame = nil
			break
		}
		if value == '\n' {
			w.consumeFrame()
			if w.err != nil || w.capture.Truncated() {
				w.frame = nil
				break
			}
			w.frame = w.frame[:0]
			continue
		}
		if len(w.frame) == grepMaxJSONFrameBytes {
			w.err = fmt.Errorf("%w: ripgrep event exceeds %d bytes", errGrepCaptureLimit, grepMaxJSONFrameBytes)
			w.frame = nil
			break
		}
		w.frame = append(w.frame, value)
	}
	return len(p), nil
}

func (w *grepRipgrepJSONWriter) Finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return w.err
	}
	w.finished = true
	if w.err == nil && !w.capture.Truncated() && len(w.frame) != 0 {
		w.consumeFrame()
	}
	w.frame = nil
	return w.err
}

func (w *grepRipgrepJSONWriter) CaptureError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *grepRipgrepJSONWriter) consumeFrame() {
	m, isMatch, err := parseRipgrepJSONFrame(w.frame)
	if err != nil {
		w.err = err
		return
	}
	if isMatch {
		w.capture.Add(m)
	}
}

func parseRipgrepJSONFrame(frame []byte) (match, bool, error) {
	var event struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(frame, &event); err != nil {
		return match{}, false, fmt.Errorf("%w: malformed JSON event: %v", errGrepOutputProtocol, err)
	}
	if event.Type == "" {
		return match{}, false, fmt.Errorf("%w: event type is missing", errGrepOutputProtocol)
	}
	if event.Type != "match" {
		switch event.Type {
		case "begin", "end", "summary":
			var data map[string]json.RawMessage
			if len(event.Data) == 0 || json.Unmarshal(event.Data, &data) != nil || data == nil {
				return match{}, false, fmt.Errorf("%w: %s event data is malformed", errGrepOutputProtocol, event.Type)
			}
			return match{}, false, nil
		default:
			return match{}, false, fmt.Errorf("%w: unsupported event type %q", errGrepOutputProtocol, event.Type)
		}
	}
	var data struct {
		Path       json.RawMessage `json:"path"`
		Lines      json.RawMessage `json:"lines"`
		LineNumber uint64          `json:"line_number"`
	}
	if len(event.Data) == 0 || json.Unmarshal(event.Data, &data) != nil {
		return match{}, false, fmt.Errorf("%w: match event data is malformed", errGrepOutputProtocol)
	}
	if data.LineNumber == 0 || data.LineNumber > uint64(^uint(0)>>1) {
		return match{}, false, fmt.Errorf("%w: match line number is invalid", errGrepOutputProtocol)
	}
	file, err := decodeRipgrepTextOrBytes(data.Path)
	if err != nil {
		return match{}, false, fmt.Errorf("%w: match path: %v", errGrepOutputProtocol, err)
	}
	if len(file) == 0 {
		return match{}, false, fmt.Errorf("%w: match path is empty", errGrepOutputProtocol)
	}
	content, err := decodeRipgrepTextOrBytes(data.Lines)
	if err != nil {
		return match{}, false, fmt.Errorf("%w: match lines: %v", errGrepOutputProtocol, err)
	}
	if len(content) > 0 && content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}
	if len(content) > 0 && content[len(content)-1] == '\r' {
		content = content[:len(content)-1]
	}
	return match{File: string(file), Line: int(data.LineNumber), Content: string(content)}, true, nil
}

func decodeRipgrepTextOrBytes(value json.RawMessage) ([]byte, error) {
	var encoded struct {
		Text  *string `json:"text"`
		Bytes *string `json:"bytes"`
	}
	if len(value) == 0 || json.Unmarshal(value, &encoded) != nil {
		return nil, errors.New("text/bytes value is malformed")
	}
	if encoded.Text != nil && encoded.Bytes != nil {
		return nil, errors.New("text and bytes are both present")
	}
	if encoded.Text != nil {
		return []byte(*encoded.Text), nil
	}
	if encoded.Bytes == nil {
		return nil, errors.New("text or bytes is missing")
	}
	decoded, err := base64.StdEncoding.DecodeString(*encoded.Bytes)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

type grepDirectoryReader interface {
	directoryBatchReader
	Close() error
}

type grepDirectoryOpenFunc func(string) (grepDirectoryReader, error)

type grepFileAccessError struct{ err error }

func (e *grepFileAccessError) Error() string { return fmt.Sprintf("grep: file access: %v", e.err) }
func (e *grepFileAccessError) Unwrap() error { return e.err }

// grepPureGo scans a root file directly and traverses directory roots with a
// one-entry-at-a-time DFS, retaining no pending sibling list.
func grepPureGo(ctx context.Context, re *regexp.Regexp, searchPath, glob string, whitelist PathWhitelist, capture *grepMatchCapture) (bool, error) {
	return grepPureGoWithOpen(ctx, re, searchPath, glob, whitelist, capture, func(path string) (grepDirectoryReader, error) {
		return os.Open(path)
	})
}

func grepPureGoWithOpen(ctx context.Context, re *regexp.Regexp, searchPath, glob string, whitelist PathWhitelist, capture *grepMatchCapture, openDir grepDirectoryOpenFunc) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rootInfo, err := os.Lstat(searchPath)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if err != nil {
		return false, fmt.Errorf("grep: root %q: %w", searchPath, err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	if rootInfo.Mode().IsRegular() {
		if grepFileMatchesGlob(searchPath, searchPath, glob, true) {
			if err := scanGrepFile(ctx, searchPath, re, capture, whitelist); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return false, ctxErr
				}
				return false, fmt.Errorf("grep: root file %q: %w", searchPath, err)
			}
		}
		return capture.Truncated(), ctx.Err()
	}
	if !rootInfo.IsDir() {
		return false, ctx.Err()
	}

	truncated := false
	var visitDir func(string, bool) error
	visitDir = func(dirPath string, isRoot bool) (walkErr error) {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, openErr := openDir(dirPath)
		if openErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if isRoot {
				return fmt.Errorf("grep: open root directory %q: %w", dirPath, openErr)
			}
			truncated = true
			return nil
		}
		for {
			if err := ctx.Err(); err != nil {
				walkErr = err
				break
			}
			entries, readErr := dir.ReadDir(1)
			if err := ctx.Err(); err != nil {
				walkErr = err
				break
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					walkErr = err
					break
				}
				entryPath := filepath.Join(dirPath, entry.Name())
				info, infoErr := entry.Info()
				if infoErr != nil {
					if err := ctx.Err(); err != nil {
						walkErr = err
						break
					}
					truncated = true
					continue
				}
				if info.Mode()&os.ModeSymlink != 0 {
					continue
				}
				if info.IsDir() {
					if err := visitDir(entryPath, false); err != nil {
						walkErr = err
						break
					}
					continue
				}
				if !info.Mode().IsRegular() || !grepFileMatchesGlob(searchPath, entryPath, glob, false) {
					continue
				}
				if err := ctx.Err(); err != nil {
					walkErr = err
					break
				}
				scanErr := scanGrepFile(ctx, entryPath, re, capture, whitelist)
				if scanErr != nil {
					var accessErr *grepFileAccessError
					if errors.As(scanErr, &accessErr) {
						if ctxErr := ctx.Err(); ctxErr != nil {
							walkErr = ctxErr
							break
						}
						truncated = true
						continue
					}
					walkErr = scanErr
					break
				}
			}
			if walkErr != nil {
				break
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				if isRoot {
					walkErr = fmt.Errorf("grep: read root directory %q: %w", dirPath, readErr)
				} else {
					truncated = true
				}
				break
			}
			if len(entries) == 0 {
				if isRoot {
					walkErr = fmt.Errorf("grep: read root directory %q: %w", dirPath, io.ErrNoProgress)
				} else {
					truncated = true
				}
				break
			}
		}
		closeErr := dir.Close()
		if closeErr != nil {
			wrapped := fmt.Errorf("grep: close directory %q: %w", dirPath, closeErr)
			if isRoot {
				walkErr = errors.Join(walkErr, wrapped)
			} else {
				truncated = true
				if walkErr != nil {
					walkErr = errors.Join(walkErr, wrapped)
				}
			}
		}
		return walkErr
	}
	if err := visitDir(searchPath, true); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return truncated || capture.Truncated(), nil
}

func grepFileMatchesGlob(root, filePath, glob string, rootFile bool) bool {
	if glob == "" {
		return true
	}
	relative := filepath.Base(filePath)
	if !rootFile {
		if rel, err := filepath.Rel(root, filePath); err == nil {
			relative = filepath.ToSlash(rel)
		} else {
			relative = filepath.ToSlash(filePath)
		}
	}
	return matchGlob(glob, filepath.Base(filePath)) || matchGlob(glob, relative)
}

func scanGrepFile(ctx context.Context, filePath string, re *regexp.Regexp, capture *grepMatchCapture, whitelist PathWhitelist) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return &grepFileAccessError{err: err}
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if !whitelist.unrestricted() && !whitelist.IsAllowed(resolveSymlinks(filePath)) {
		return nil
	}
	file, err := os.Open(filePath)
	if err != nil {
		return &grepFileAccessError{err: err}
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNumber := 0
	var scanErr error
	for {
		if err := ctx.Err(); err != nil {
			scanErr = err
			break
		}
		if !scanner.Scan() {
			if err := ctx.Err(); err != nil {
				scanErr = err
			} else if err := scanner.Err(); err != nil {
				if errors.Is(err, bufio.ErrTooLong) {
					scanErr = fmt.Errorf("grep: scan %q: %w", filePath, errors.Join(errGrepCaptureLimit, err))
				} else {
					scanErr = fmt.Errorf("grep: scan %q: %w", filePath, err)
				}
			}
			break
		}
		if err := ctx.Err(); err != nil {
			scanErr = err
			break
		}
		lineNumber++
		content := scanner.Text()
		if re.MatchString(content) {
			capture.Add(match{File: filePath, Line: lineNumber, Content: content})
		}
	}
	if closeErr := file.Close(); closeErr != nil {
		if scanErr != nil {
			return errors.Join(scanErr, fmt.Errorf("grep: close file %q: %w", filePath, closeErr))
		}
		return &grepFileAccessError{err: closeErr}
	}
	return scanErr
}
