package tests

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type r1bExpectation struct {
	TaskID string
	Canary string
}

type r1bTaskState struct {
	TaskID     string `json:"task_id"`
	Canary     string `json:"canary"`
	Version    int    `json:"version"`
	Stage      int    `json:"stage"`
	EffectDone bool   `json:"effect_done"`
}

type r1bProcess struct {
	PID         int     `json:"pid"`
	SessionID   string  `json:"session_id"`
	Turns       int     `json:"user_turns"`
	Resumed     bool    `json:"resumed"`
	ExitCode    int     `json:"exit_code"`
	WallSeconds float64 `json:"wall_seconds"`
	StartedUTC  string  `json:"started_utc"`
	ExitedUTC   string  `json:"exited_utc"`
}

type r1bCompaction struct {
	ID       string `json:"entry_id"`
	Boundary string `json:"source_boundary"`
	Process  string `json:"process"`
	Turn     int    `json:"user_turn"`
}

type r1bEvidence struct {
	Processes           []r1bProcess      `json:"processes"`
	Compactions         []r1bCompaction   `json:"compactions"`
	ToolReceipts        map[string]string `json:"tool_receipts"`
	ToolProofs          map[string]string `json:"tool_proofs"`
	ToolResolved        map[string]bool   `json:"tool_resolved"`
	Pending             int               `json:"pending_actions"`
	PendingBeforeResume int               `json:"pending_before_resume"`
}

var r1bToolIDs = []string{"r1b-write-state", "r1b-effect-once", "r1b-read-state", "r1b-read-final-state", "r1b-finalize"}

func gradeR1b(root string, expected r1bExpectation, evidence r1bEvidence) error {
	if len(evidence.Processes) != 2 {
		return fmt.Errorf("require two actual process records")
	}
	a, b := evidence.Processes[0], evidence.Processes[1]
	if a.PID <= 0 || b.PID <= 0 || a.PID == b.PID || a.ExitCode != 0 || b.ExitCode != 0 || a.Turns != 60 || b.Turns != 60 {
		return fmt.Errorf("process identity, exit or 60+60 submissions mismatch")
	}
	if a.SessionID == "" || b.SessionID != a.SessionID || a.Resumed || !b.Resumed {
		return fmt.Errorf("second process did not resume the exact physical session")
	}
	for _, name := range []string{"task-state.json", "final.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		var state r1bTaskState
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("decode %s: %w", name, err)
		}
		if state.TaskID != expected.TaskID || state.Canary != expected.Canary || state.Version != 2 || state.Stage != 2 || !state.EffectDone {
			return fmt.Errorf("%s failed independent state/phase acceptance", name)
		}
	}
	file, err := os.Open(filepath.Join(root, "effects.jsonl"))
	if err != nil {
		return fmt.Errorf("open effect ledger: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	effects := 0
	for scanner.Scan() {
		var row struct {
			TaskID string `json:"task_id"`
			Event  string `json:"event"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return fmt.Errorf("invalid effect ledger: %w", err)
		}
		if row.TaskID != expected.TaskID || row.Event != "once" {
			return fmt.Errorf("effect identity mismatch")
		}
		effects++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read effect ledger: %w", err)
	}
	if effects != 1 {
		return fmt.Errorf("effect count %d, require exactly one", effects)
	}
	ids, boundaries := map[string]bool{}, map[string]bool{}
	for _, event := range evidence.Compactions {
		if event.ID == "" || event.Boundary == "" || ids[event.ID] || boundaries[event.Boundary] ||
			(event.Process != "A" && event.Process != "B") || event.Turn < 1 || event.Turn > 120 {
			return fmt.Errorf("invalid or duplicate compaction source evidence")
		}
		ids[event.ID], boundaries[event.Boundary] = true, true
	}
	if len(ids) < 2 {
		return fmt.Errorf("require at least two distinct durable compaction boundaries")
	}
	if evidence.PendingBeforeResume != 0 || evidence.Pending != 0 {
		return fmt.Errorf("pending actions before/after resume: %d/%d", evidence.PendingBeforeResume, evidence.Pending)
	}
	for _, id := range r1bToolIDs {
		if evidence.ToolReceipts[id] != "success" || evidence.ToolProofs[id] == "" || !evidence.ToolResolved[id] {
			return fmt.Errorf("tool %s lacks successful receipt, completion proof or resolution", id)
		}
	}
	return nil
}
