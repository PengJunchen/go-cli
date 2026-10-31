package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestR1bAcceptanceGraderControls(t *testing.T) {
	for _, control := range []string{"valid", "new session", "missing checkpoint", "duplicate effect", "duplicate compaction boundary", "pending before resume"} {
		t.Run(control, func(t *testing.T) {
			root := t.TempDir()
			expected := r1bExpectation{TaskID: "synthetic-task", Canary: "synthetic-state-value"}
			state := r1bTaskState{TaskID: expected.TaskID, Canary: expected.Canary, Version: 2, Stage: 2, EffectDone: true}
			write := func(name string, data any) {
				t.Helper()
				content, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("task-state.json", state)
			write("final.json", state)
			effect := `{"task_id":"synthetic-task","event":"once"}` + "\n"
			if control == "duplicate effect" {
				effect += effect
			}
			if err := os.WriteFile(filepath.Join(root, "effects.jsonl"), []byte(effect), 0o600); err != nil {
				t.Fatal(err)
			}
			evidence := r1bEvidence{
				Processes:    []r1bProcess{{PID: 101, SessionID: "same", Turns: 60}, {PID: 202, SessionID: "same", Turns: 60, Resumed: true}},
				Compactions:  []r1bCompaction{{ID: "compact-1", Boundary: "entry-20", Process: "A", Turn: 20}, {ID: "compact-2", Boundary: "entry-80", Process: "B", Turn: 80}},
				ToolReceipts: map[string]string{}, ToolProofs: map[string]string{}, ToolResolved: map[string]bool{},
			}
			for _, id := range r1bToolIDs {
				evidence.ToolReceipts[id] = "success"
				evidence.ToolProofs[id] = "inline_pipeline_joined"
				evidence.ToolResolved[id] = true
			}
			if control == "new session" {
				evidence.Processes[1].SessionID = "fresh-session"
				evidence.Processes[1].Resumed = false
			}
			if control == "missing checkpoint" {
				if err := os.Remove(filepath.Join(root, "task-state.json")); err != nil {
					t.Fatal(err)
				}
			}
			if control == "duplicate compaction boundary" {
				evidence.Compactions[1].Boundary = evidence.Compactions[0].Boundary
			}
			if control == "pending before resume" {
				evidence.PendingBeforeResume = 1
			}
			err := gradeR1b(root, expected, evidence)
			if control == "valid" && err != nil {
				t.Fatal(err)
			}
			if control != "valid" && err == nil {
				t.Fatalf("grader accepted negative control %q", control)
			}
		})
	}
}
