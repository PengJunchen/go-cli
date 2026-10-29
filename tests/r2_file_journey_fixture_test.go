package tests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type r2FileExpectation struct {
	Business string
	Token    string
	Target   string
	Initial  map[string]string
}

type r2FileCall struct {
	ID, Tool, RequestSHA256, SchemaSHA256 string
	Args, Request, NextRequest            json.RawMessage
}

type r2FileAction struct {
	ID, Tool, Outcome, Dispatch, Resource, Proof, Resolution, ResolutionRef string
	IntentCount, ReceiptCount, ResolutionCount                              int
}

type r2FileEvidence struct {
	Calls          []r2FileCall
	Actions        map[string]r2FileAction
	Pending        int
	ExitCode       int
	FinalDelivered bool
}

var r2FileExpectedTools = map[string]string{
	"r2-ls": "ls", "r2-find": "find", "r2-grep": "grep", "r2-read-source": "read", "r2-write": "write",
	"r2-edit-bad": "edit", "r2-read-after-bad": "read", "r2-edit-good": "edit", "r2-read-final": "read",
}

func r2FileHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// This grader reads business bytes itself. Peer observations and final prose
// cannot substitute for either disk acceptance or captured tool-result data.
func gradeR2FileJourney(root string, expected r2FileExpectation, evidence r2FileEvidence) error {
	if evidence.ExitCode != 0 || !evidence.FinalDelivered || evidence.Pending != 0 {
		return fmt.Errorf("CLI final/exit/pending acceptance failed")
	}
	wantedFiles := map[string]string{"result.txt": strings.Replace(expected.Initial[expected.Target], "status:pending", "status:done", 1)}
	for name, content := range expected.Initial {
		wantedFiles[name] = content
	}
	wantedEntries := map[string]bool{}
	for name := range wantedFiles {
		wantedEntries[name] = false
		for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
			wantedEntries[dir] = true
		}
	}
	seen := map[string]bool{}
	businessRoot := filepath.Join(root, expected.Business)
	err := filepath.WalkDir(businessRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == businessRoot {
			return nil
		}
		name, relErr := filepath.Rel(businessRoot, path)
		if relErr != nil {
			return relErr
		}
		isDir, exists := wantedEntries[name]
		if !exists || isDir != entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected business entry %q", name)
		}
		seen[name] = true
		if !entry.IsDir() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if r2FileHash(data) != r2FileHash([]byte(wantedFiles[name])) {
				return fmt.Errorf("business bytes mismatch: %s", name)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("independent disk acceptance: %w", err)
	}
	if len(seen) != len(wantedEntries) {
		return fmt.Errorf("missing business entries")
	}
	ids := []string{"r2-ls", "r2-find", "r2-grep", "r2-read-source", "r2-write", "r2-edit-bad", "r2-read-after-bad", "r2-edit-good", "r2-read-final"}
	results, calls, actions := map[string]string{}, map[string]r2FileCall{}, map[string]bool{}
	next := 0
	for index, call := range evidence.Calls {
		if _, duplicate := calls[call.ID]; duplicate {
			return fmt.Errorf("duplicate wire call %s", call.ID)
		}
		if index+1 < len(evidence.Calls) && r2FileHash(call.NextRequest) != r2FileHash(evidence.Calls[index+1].Request) {
			return fmt.Errorf("next request is not the next captured call boundary")
		}
		if strings.HasPrefix(call.ID, "r2-discover-") {
			if call.Tool != "tool_search" {
				return fmt.Errorf("discovery used a non-search tool")
			}
		} else {
			if next >= len(ids) || call.ID != ids[next] {
				return fmt.Errorf("unexpected call order %s", call.ID)
			}
			if call.Tool != r2FileExpectedTools[call.ID] {
				return fmt.Errorf("call %s expected tool %s, got %s", call.ID, r2FileExpectedTools[call.ID], call.Tool)
			}
			next++
		}
		schemaHash, guardErr := r2GuardAdvertisedCall(call.Request, call.Tool, call.Args)
		if guardErr != nil {
			return fmt.Errorf("wire advertisement %s: %w", call.ID, guardErr)
		}
		if schemaHash != call.SchemaSHA256 || r2FileHash(call.Request) != call.RequestSHA256 {
			return fmt.Errorf("wire/schema digest mismatch for %s", call.ID)
		}
		output, resultErr := r2ToolResult(call.NextRequest, call.ID)
		if resultErr != nil {
			return resultErr
		}
		results[call.ID], calls[call.ID] = output, call
		action, exists := evidence.Actions[call.ID]
		if !exists || action.ID == "" || action.Tool != call.Tool || actions[action.ID] || action.IntentCount != 1 || action.ReceiptCount != 1 || action.ResolutionCount != 1 || action.ResolutionRef == "" {
			return fmt.Errorf("missing/duplicate durable action identity: %s", call.ID)
		}
		actions[action.ID] = true
		if call.ID == "r2-edit-bad" {
			if action.Outcome != "error" || !strings.Contains(output, "not found") {
				return fmt.Errorf("failed edit did not return its actual mismatch")
			}
			if action.Dispatch == "not_dispatched" {
				if action.Proof != "" || action.Resolution != "never_dispatched" {
					return fmt.Errorf("undispatched edit minted completion evidence")
				}
				continue
			}
		} else if action.Outcome != "success" {
			return fmt.Errorf("successful action %s has outcome %s", call.ID, action.Outcome)
		}
		if action.Dispatch != "dispatched" || action.Resource != "quiescent" || !r2PositiveFileProof(action.Proof) || action.Resolution != "local_resources_quiescent" {
			return fmt.Errorf("action %s lacks positive resource closure", call.ID)
		}
	}
	if next != len(ids) || len(actions) != len(evidence.Actions) {
		return fmt.Errorf("incomplete journey or unaccounted action")
	}
	if !r2SameStrings(r2Lines(results["r2-ls"]), []string{"decoy.txt", "ignored.log", "nested/"}) {
		return fmt.Errorf("ls entry set mismatch")
	}
	var found []string
	for _, path := range r2Lines(results["r2-find"]) {
		normalized, pathErr := r2BusinessPath(root, expected.Business, path)
		if pathErr != nil {
			return pathErr
		}
		found = append(found, normalized)
	}
	if !r2SameStrings(found, []string{"decoy.txt", expected.Target}) {
		return fmt.Errorf("find matched incorrect files")
	}
	path, line, content, err := r2GrepMatch(results["r2-grep"])
	if err != nil {
		return err
	}
	normalized, err := r2BusinessPath(root, expected.Business, path)
	if err != nil || normalized != expected.Target || line != 2 || content != "R2_TOKEN="+expected.Token {
		return fmt.Errorf("grep target/line/content mismatch")
	}
	source := expected.Initial[expected.Target]
	if results["r2-read-source"] != source || results["r2-read-after-bad"] != source || results["r2-read-final"] != wantedFiles["result.txt"] {
		return fmt.Errorf("actual read result bytes mismatch")
	}
	for id, args := range map[string]map[string]any{
		"r2-ls":             {"path": expected.Business},
		"r2-find":           {"path": expected.Business, "pattern": "*.txt", "type": "f"},
		"r2-grep":           {"path": expected.Business, "glob": "*.txt", "pattern": expected.Token},
		"r2-read-source":    {"path": filepath.Join(expected.Business, normalized)},
		"r2-write":          {"path": filepath.Join(expected.Business, "result.txt"), "content": results["r2-read-source"]},
		"r2-edit-bad":       {"file_path": filepath.Join(expected.Business, "result.txt"), "old_string": "missing-" + expected.Token, "new_string": "wrong"},
		"r2-read-after-bad": {"path": filepath.Join(expected.Business, "result.txt")},
		"r2-edit-good":      {"file_path": filepath.Join(expected.Business, "result.txt"), "old_string": "status:pending", "new_string": "status:done"},
		"r2-read-final":     {"path": filepath.Join(expected.Business, "result.txt")},
	} {
		var actual map[string]any
		if err := json.Unmarshal(calls[id].Args, &actual); err != nil || !reflect.DeepEqual(actual, args) {
			return fmt.Errorf("actual result-to-next-call dataflow mismatch: %s", id)
		}
	}
	return nil
}

func r2PositiveFileProof(proof string) bool {
	return proof == "inline_pipeline_joined" || proof == "owned_process_group_joined" || proof == "queued_job_joined"
}

// Only this fixture's flat scalar arguments are supported. Unknown keywords,
// missing advertisement, missing required fields and unknown argument keys fail
// before the peer emits a call; this is not a general JSON Schema engine.
func r2GuardAdvertisedCall(request []byte, name string, arguments []byte) (string, error) {
	var body struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		return "", fmt.Errorf("decode request: %w", err)
	}
	var schema map[string]any
	for _, tool := range body.Tools {
		if tool.Type == "function" && tool.Function.Name == name {
			if schema != nil {
				return "", fmt.Errorf("duplicate advertisement for %s", name)
			}
			schema = tool.Function.Parameters
		}
	}
	if schema == nil {
		return "", fmt.Errorf("%s not advertised with object parameters in current request", name)
	}
	properties, required, err := r2SimpleFileSchema(schema)
	if err != nil {
		return "", fmt.Errorf("unsupported advertised schema for %s: %w", name, err)
	}
	var args map[string]any
	if err := json.Unmarshal(arguments, &args); err != nil || args == nil {
		return "", fmt.Errorf("invalid fixture arguments for %s", name)
	}
	for key, value := range args {
		property, exists := properties[key].(map[string]any)
		if !exists || property["type"] != "string" {
			return "", fmt.Errorf("unknown or unsupported argument %s.%s", name, key)
		}
		text, isString := value.(string)
		if !isString {
			return "", fmt.Errorf("non-string fixture argument %s.%s", name, key)
		}
		if choices, hasEnum := property["enum"].([]any); hasEnum {
			matched := false
			for _, choice := range choices {
				matched = matched || choice == text
			}
			if !matched {
				return "", fmt.Errorf("invalid enum for %s.%s", name, key)
			}
		}
	}
	for _, key := range required {
		if _, exists := args[key]; !exists {
			return "", fmt.Errorf("missing required argument %s.%s", name, key)
		}
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}
	return r2FileHash(data), nil
}

// Validate only the native flat scalar schema subset used by this journey.
// Optional native booleans/integers may be advertised; emitted fixture args
// remain strings. Combinators and unimplemented constraints fail closed.
func r2SimpleFileSchema(schema map[string]any) (map[string]any, []string, error) {
	for keyword, value := range schema {
		switch keyword {
		case "type":
			if value != "object" {
				return nil, nil, fmt.Errorf("non-object parameters")
			}
		case "properties", "required":
			// Checked structurally below, including explicitly malformed values.
		case "additionalProperties":
			if _, ok := value.(bool); !ok {
				return nil, nil, fmt.Errorf("additionalProperties is not boolean")
			}
		case "description":
			if _, ok := value.(string); !ok {
				return nil, nil, fmt.Errorf("description is not string")
			}
		default:
			return nil, nil, fmt.Errorf("unsupported keyword %s", keyword)
		}
	}
	properties, ok := schema["properties"].(map[string]any)
	if schema["type"] != "object" || !ok || len(properties) == 0 {
		return nil, nil, fmt.Errorf("missing object/properties")
	}
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("property %s is not object", name)
		}
		kind, ok := property["type"].(string)
		if !ok || (kind != "string" && kind != "boolean" && kind != "integer") {
			return nil, nil, fmt.Errorf("unsupported property type for %s", name)
		}
		for keyword, value := range property {
			switch keyword {
			case "type":
			case "description":
				if _, ok := value.(string); !ok {
					return nil, nil, fmt.Errorf("property description is not string")
				}
			case "enum":
				choices, ok := value.([]any)
				if !ok || len(choices) == 0 || kind != "string" {
					return nil, nil, fmt.Errorf("malformed string enum for %s", name)
				}
				for _, choice := range choices {
					if _, ok := choice.(string); !ok {
						return nil, nil, fmt.Errorf("non-string enum member for %s", name)
					}
				}
			default:
				return nil, nil, fmt.Errorf("unsupported property keyword %s.%s", name, keyword)
			}
		}
	}
	var required []string
	if value, present := schema["required"]; present {
		keys, ok := value.([]any)
		if !ok {
			return nil, nil, fmt.Errorf("required is not array")
		}
		seen := map[string]bool{}
		for _, raw := range keys {
			key, ok := raw.(string)
			if _, exists := properties[key]; !ok || !exists || seen[key] {
				return nil, nil, fmt.Errorf("invalid required member")
			}
			seen[key] = true
			required = append(required, key)
		}
	}
	return properties, required, nil
}

func r2ToolResult(request []byte, id string) (string, error) {
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		return "", fmt.Errorf("decode next actual request: %w", err)
	}
	count, result := 0, ""
	for _, message := range body.Messages {
		if message.Role == "tool" && message.ToolCallID == id {
			count++
			result = message.Content
		}
	}
	if count != 1 {
		return "", fmt.Errorf("tool %s has %d results in next actual request", id, count)
	}
	return result, nil
}

func r2BusinessPath(root, business, observed string) (string, error) {
	path := filepath.Clean(strings.TrimSpace(observed))
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
	}
	path = strings.TrimPrefix(path, business+string(filepath.Separator))
	if path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) || filepath.IsAbs(path) {
		return "", fmt.Errorf("observed path escapes business fixture: %q", observed)
	}
	return path, nil
}

func r2GrepMatch(output string) (string, int, string, error) {
	lines := r2Lines(output)
	if len(lines) != 1 {
		return "", 0, "", fmt.Errorf("require one real grep match")
	}
	parts := strings.SplitN(lines[0], ":", 3)
	if len(parts) != 3 {
		return "", 0, "", fmt.Errorf("invalid grep match format")
	}
	line, err := strconv.Atoi(parts[1])
	return parts[0], line, parts[2], err
}

func r2Lines(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(text), "\n")
}

func r2SameStrings(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}
