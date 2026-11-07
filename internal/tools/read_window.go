package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
)

const maxReadWindowParameter = 2147483647

type readWindow struct {
	enabled  bool
	offset   int
	limit    int
	hasLimit bool
}

func parseReadWindow(args map[string]any) (readWindow, error) {
	window := readWindow{offset: 1}
	rawOffset, hasOffset := args["offset"]
	rawLimit, hasLimit := args["limit"]
	window.enabled = hasOffset || hasLimit
	if hasOffset {
		value, err := readWindowInteger("offset", rawOffset)
		if err != nil {
			return readWindow{}, err
		}
		window.offset = value
	}
	if hasLimit {
		value, err := readWindowInteger("limit", rawLimit)
		if err != nil {
			return readWindow{}, err
		}
		window.limit, window.hasLimit = value, true
	}
	return window, nil
}

func readWindowInteger(name string, value any) (int, error) {
	number, ok := schemaRational(value)
	if !ok || !number.IsInt() || !number.Num().IsInt64() {
		return 0, fmt.Errorf("read: %s must be an integer from 1 to %d", name, maxReadWindowParameter)
	}
	n := number.Num().Int64()
	if n < 1 || n > maxReadWindowParameter {
		return 0, fmt.Errorf("read: %s must be an integer from 1 to %d", name, maxReadWindowParameter)
	}
	return int(n), nil
}

func projectReadWindow(ctx context.Context, path string, data []byte, window readWindow) (*ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read: window: %w", err)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("read: line windows require valid UTF-8")
	}
	startIndex := window.offset - 1
	startByte, endByte, total, returned := -1, -1, 0, 0
	for pos := 0; pos < len(data); {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("read: window: %w", err)
		}
		end := len(data)
		if i := bytes.IndexByte(data[pos:], '\n'); i >= 0 {
			end = pos + i + 1
		}
		if total >= startIndex && (!window.hasLimit || total-startIndex < window.limit) {
			if startByte < 0 {
				startByte = pos
			}
			endByte = end
			returned++
		}
		total++
		pos = end
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read: window: %w", err)
	}
	before := min(startIndex, total)
	omittedBefore := before
	omittedAfter := total - before - returned
	hasMore := omittedAfter > 0
	startLine, endLine := 0, 0
	excerpt := ""
	if returned > 0 {
		startLine, endLine = before+1, before+returned
		excerpt = string(data[startByte:endByte])
	}
	metadata := map[string]any{
		"path": path, "bytes": len(excerpt), "source_bytes": len(data), "windowed": true,
		"offset": window.offset, "start_line": startLine, "end_line": endLine,
		"total_lines": total, "returned_lines": returned, "omitted_before": omittedBefore,
		"omitted_after": omittedAfter, "has_more": hasMore, "truncated": omittedBefore > 0 || omittedAfter > 0,
	}
	if window.hasLimit {
		metadata["limit"] = window.limit
	}
	var notice string
	if returned == 0 {
		notice = fmt.Sprintf("[read window; not file content; empty excerpt, EOF; total_lines=%d; omitted_before=%d; omitted_after=%d]\n", total, omittedBefore, omittedAfter)
	} else {
		notice = fmt.Sprintf("[read window; not file content; lines=%d-%d/%d; omitted_before=%d; omitted_after=%d; eof=%t", startLine, endLine, total, omittedBefore, omittedAfter, !hasMore)
		if hasMore {
			nextOffset := before + returned + 1
			metadata["next_offset"] = nextOffset
			notice += fmt.Sprintf("; next_offset=%d", nextOffset)
		}
		notice += "]\n"
	}
	return &ToolResult{Output: notice + excerpt, Metadata: metadata}, nil
}
