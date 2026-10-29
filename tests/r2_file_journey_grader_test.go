package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR2FileGraderContract(t *testing.T) {
	for _, scenario := range []string{"accepted", "wrong-business-bytes", "changed-input", "missing-result", "unknown-schema", "wrong-tool", "pending", "forged-proof", "extra-business-file"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			expected := r2FileExpectation{Business: "data space", Token: "synthetic-token", Target: "nested/target.txt", Initial: map[string]string{
				"nested/target.txt": "record\nR2_TOKEN=synthetic-token\nstatus:pending\n",
				"decoy.txt":         "unrelated\n",
				"ignored.log":       "R2_TOKEN=synthetic-token\n",
			}}
			for name, content := range expected.Initial {
				r2ContractWrite(t, filepath.Join(root, expected.Business, name), content)
			}
			result := strings.Replace(expected.Initial[expected.Target], "status:pending", "status:done", 1)
			r2ContractWrite(t, filepath.Join(root, expected.Business, "result.txt"), result)
			// Native runtime files live outside the declared business directory.
			r2ContractWrite(t, filepath.Join(root, ".go-cli/logs/audit.log"), "retained audit\n")
			evidence := r2ContractEvidence(t, root, expected)
			switch scenario {
			case "wrong-business-bytes":
				r2ContractWrite(t, filepath.Join(root, expected.Business, "result.txt"), "wrong\n")
			case "changed-input":
				r2ContractWrite(t, filepath.Join(root, expected.Business, "decoy.txt"), "changed\n")
			case "missing-result":
				var body map[string]any
				if err := json.Unmarshal(evidence.Calls[3].NextRequest, &body); err != nil {
					t.Fatal(err)
				}
				var kept []any
				for _, item := range body["messages"].([]any) {
					message := item.(map[string]any)
					if message["role"] != "tool" || message["tool_call_id"] != "r2-read-source" {
						kept = append(kept, item)
					}
				}
				body["messages"] = kept
				evidence.Calls[3].NextRequest, _ = json.Marshal(body)
				evidence.Calls[4].Request = evidence.Calls[3].NextRequest
				evidence.Calls[4].RequestSHA256 = r2FileHash(evidence.Calls[4].Request)
			case "unknown-schema":
				evidence.Calls[0].Request = []byte(`{"tools":[],"messages":[]}`)
				evidence.Calls[0].RequestSHA256 = r2FileHash(evidence.Calls[0].Request)
			case "wrong-tool":
				call := &evidence.Calls[0]
				var body map[string]any
				if err := json.Unmarshal(call.Request, &body); err != nil {
					t.Fatal(err)
				}
				body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] = "read"
				call.Tool = "read"
				call.Request, _ = json.Marshal(body)
				call.RequestSHA256 = r2FileHash(call.Request)
				action := evidence.Actions[call.ID]
				action.Tool = "read"
				evidence.Actions[call.ID] = action
			case "pending":
				evidence.Pending = 1
			case "forged-proof":
				action := evidence.Actions["r2-ls"]
				action.Proof = "pending"
				evidence.Actions["r2-ls"] = action
			case "extra-business-file":
				r2ContractWrite(t, filepath.Join(root, expected.Business, "unexpected.txt"), "extra\n")
			}
			err := gradeR2FileJourney(root, expected, evidence)
			if scenario == "accepted" && err != nil {
				t.Fatalf("valid independent fixture rejected: %v", err)
			}
			if scenario != "accepted" && err == nil {
				t.Fatalf("independent grader accepted %s despite valid exit/final evidence", scenario)
			}
			if scenario == "missing-result" && !strings.Contains(err.Error(), "r2-read-source has 0 results") {
				t.Fatalf("missing-result control failed for an unrelated reason: %v", err)
			}
			if scenario == "wrong-tool" && !strings.Contains(err.Error(), "expected tool ls") {
				t.Fatalf("wrong-tool control failed for an unrelated reason: %v", err)
			}
		})
	}
}

func r2ContractWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func r2ContractEvidence(t *testing.T, root string, expected r2FileExpectation) r2FileEvidence {
	t.Helper()
	source := expected.Initial[expected.Target]
	definitions := []struct {
		id, tool string
		args     map[string]any
		result   string
	}{
		{"r2-ls", "ls", map[string]any{"path": expected.Business}, "decoy.txt\nignored.log\nnested/"},
		{"r2-find", "find", map[string]any{"path": expected.Business, "pattern": "*.txt", "type": "f"}, "data space/decoy.txt\ndata space/nested/target.txt"},
		{"r2-grep", "grep", map[string]any{"path": expected.Business, "glob": "*.txt", "pattern": expected.Token}, "data space/nested/target.txt:2:R2_TOKEN=" + expected.Token},
		{"r2-read-source", "read", map[string]any{"path": "data space/nested/target.txt"}, source},
		{"r2-write", "write", map[string]any{"path": "data space/result.txt", "content": source}, "written"},
		{"r2-edit-bad", "edit", map[string]any{"file_path": "data space/result.txt", "old_string": "missing-" + expected.Token, "new_string": "wrong"}, "Error: edit: old_string not found"},
		{"r2-read-after-bad", "read", map[string]any{"path": "data space/result.txt"}, source},
		{"r2-edit-good", "edit", map[string]any{"file_path": "data space/result.txt", "old_string": "status:pending", "new_string": "status:done"}, "edited"},
		{"r2-read-final", "read", map[string]any{"path": "data space/result.txt"}, strings.Replace(source, "status:pending", "status:done", 1)},
	}
	evidence := r2FileEvidence{Actions: map[string]r2FileAction{}, FinalDelivered: true}
	var messages []any
	for _, definition := range definitions {
		properties := map[string]any{}
		for key := range definition.args {
			properties[key] = map[string]any{"type": "string"}
		}
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		request, _ := json.Marshal(map[string]any{"messages": messages, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": definition.tool, "parameters": schema}}}})
		args, _ := json.Marshal(definition.args)
		schemaBytes, _ := json.Marshal(schema)
		evidence.Calls = append(evidence.Calls, r2FileCall{ID: definition.id, Tool: definition.tool, Args: args, Request: request, RequestSHA256: r2FileHash(request), SchemaSHA256: r2FileHash(schemaBytes)})
		messages = append(messages, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": definition.id, "type": "function", "function": map[string]any{"name": definition.tool, "arguments": string(args)}}}},
			map[string]any{"role": "tool", "tool_call_id": definition.id, "content": definition.result})
		action := r2FileAction{ID: "action-" + definition.id, Tool: definition.tool, IntentCount: 1, ReceiptCount: 1, Outcome: "success", Dispatch: "dispatched", Resource: "quiescent", Proof: "inline_pipeline_joined", ResolutionCount: 1, Resolution: "local_resources_quiescent", ResolutionRef: "execution-" + definition.id}
		if definition.id == "r2-edit-bad" {
			action.Outcome, action.Dispatch, action.Proof, action.Resolution = "error", "not_dispatched", "", "never_dispatched"
		}
		evidence.Actions[definition.id] = action
	}
	final, _ := json.Marshal(map[string]any{"messages": messages})
	for index := range evidence.Calls {
		evidence.Calls[index].NextRequest = final
		if index+1 < len(evidence.Calls) {
			evidence.Calls[index].NextRequest = evidence.Calls[index+1].Request
		}
	}
	return evidence
}

func TestR2CurrentAdvertisementContract(t *testing.T) {
	request := []byte(`{"tools":[{"type":"function","function":{"name":"edit","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["file_path","old_string","new_string"],"additionalProperties":false}}}]}`)
	for _, scenario := range []struct {
		name, tool, args string
		valid            bool
	}{
		{"valid", "edit", `{"file_path":"x","old_string":"a","new_string":"b"}`, true},
		{"not-currently-advertised", "read", `{"path":"x"}`, false},
		{"wrong-field", "edit", `{"path":"x","old_string":"a","new_string":"b"}`, false},
		{"missing-required", "edit", `{"file_path":"x","old_string":"a"}`, false},
		{"wrong-type", "edit", `{"file_path":"x","old_string":3,"new_string":"b"}`, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			digest, err := r2GuardAdvertisedCall(request, scenario.tool, []byte(scenario.args))
			if scenario.valid && (err != nil || len(digest) != 64) {
				t.Fatalf("valid current schema rejected: %v", err)
			}
			if !scenario.valid && err == nil {
				t.Fatal("invalid/unadvertised call passed the live guard")
			}
		})
	}
}

func TestR2SchemaSubsetRejectsUnimplementedConstraints(t *testing.T) {
	for _, scenario := range []string{"native-subset", "allOf", "oneOf", "minProperties", "required-string", "required-null", "required-number-member", "enum-string", "enum-number-member", "unused-property-constraint", "additionalProperties-object"} {
		t.Run(scenario, func(t *testing.T) {
			parameters := map[string]any{
				"type": "object", "required": []any{"path"}, "additionalProperties": false,
				"properties": map[string]any{"path": map[string]any{"type": "string"}, "optional": map[string]any{"type": "integer"}},
			}
			path := parameters["properties"].(map[string]any)["path"].(map[string]any)
			switch scenario {
			case "allOf", "oneOf":
				parameters[scenario] = []any{map[string]any{"type": "object"}}
			case "minProperties":
				parameters[scenario] = 2
			case "required-string":
				parameters["required"] = "path"
			case "required-null":
				parameters["required"] = nil
			case "required-number-member":
				parameters["required"] = []any{3}
			case "enum-string":
				path["enum"] = "x"
			case "enum-number-member":
				path["enum"] = []any{"x", 3}
			case "unused-property-constraint":
				parameters["properties"].(map[string]any)["optional"].(map[string]any)["minimum"] = 1
			case "additionalProperties-object":
				parameters["additionalProperties"] = map[string]any{"type": "string"}
			}
			request, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": parameters}}}})
			_, err := r2GuardAdvertisedCall(request, "read", []byte(`{"path":"x"}`))
			if scenario == "native-subset" && err != nil {
				t.Fatalf("native simple subset rejected: %v", err)
			}
			if scenario != "native-subset" && err == nil {
				t.Fatalf("unimplemented/malformed schema %s was accepted", scenario)
			}
		})
	}
}
