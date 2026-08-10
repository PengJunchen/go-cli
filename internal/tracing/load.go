package tracing

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
)

// SpanNode is a node in the reconstructed execution tree that LoadTrace builds.
type SpanNode struct {
	Span     SpanData
	Children []*SpanNode
}

// LoadTrace loads all spans from a JSONL file and reconstructs the execution
// tree. Lines that cannot be parsed are skipped.
func LoadTrace(filePath string) (*SpanNode, error) {
	slog.Debug("tracing: loading trace file", "path", filePath)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}

	var spans []SpanData
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB buffer per line
	for scanner.Scan() {
		var span SpanData
		if err := json.Unmarshal(scanner.Bytes(), &span); err != nil {
			continue // skip unparsable lines
		}
		spans = append(spans, span)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read trace file: %w", err)
	}

	slog.Debug("tracing: parsed spans from file", "count", len(spans), "path", filePath)
	return buildSpanTree(spans)
}

// buildSpanTree sorts spans by start_time and links parents to children via
// parent_span_id. It returns the root node, or nil when spans is empty.
func buildSpanTree(spans []SpanData) (*SpanNode, error) {
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].StartTime < spans[j].StartTime
	})

	nodeMap := make(map[string]*SpanNode, len(spans))
	for _, span := range spans {
		nodeMap[span.SpanID] = &SpanNode{Span: span}
	}

	var roots []*SpanNode
	for _, node := range nodeMap {
		if node.Span.ParentSpanID == "" {
			roots = append(roots, node)
		} else if parent, ok := nodeMap[node.Span.ParentSpanID]; ok {
			parent.Children = append(parent.Children, node)
		}
	}

	var root *SpanNode
	if len(roots) == 1 {
		root = roots[0]
	} else if len(roots) > 1 {
		// Multiple root spans: link them under a synthetic root so no data
		// is lost. The synthetic root has an empty SpanID.
		root = &SpanNode{Span: SpanData{SpanID: "<root>"}}
		for _, r := range roots {
			root.Children = append(root.Children, r)
		}
	}

	if root == nil {
		// No explicit root; fall back to the earliest span (spans is sorted
		// by StartTime at the top of this function).
		if len(spans) > 0 {
			root = nodeMap[spans[0].SpanID]
		}
	}

	return root, nil
}

// PrintTree prints the execution tree rooted at node in a readable hierarchy.
func PrintTree(node *SpanNode, indent string) {
	if node == nil {
		return
	}

	status := string(SpanStatusOK)
	if node.Span.Status == SpanStatusError {
		status = string(SpanStatusError)
	}
	_, _ = fmt.Printf("%s%s %s [%s] %s -> %s\n",
		indent,
		node.Span.SpanKind,
		status,
		node.Span.Name,
		node.Span.StartTime,
		node.Span.EndTime,
	)
	if node.Span.Status == SpanStatusError && node.Span.StatusMessage != "" {
		_, _ = fmt.Printf("%s  ERROR: %s\n", indent, node.Span.StatusMessage)
	}
	for _, child := range node.Children {
		PrintTree(child, indent+"  ")
	}
}
