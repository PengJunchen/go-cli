package core

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxActiveProcessSessionRefs = 16

// ValidateActiveProcessSessionRefs checks the bounded, deterministic roster
// copied into a durable compaction projection. These identifiers are model
// context only; they do not grant process-session authority.
func ValidateActiveProcessSessionRefs(refs []ActiveProcessSessionRef, runID, workspaceID string) error {
	if len(refs) > maxActiveProcessSessionRefs {
		return fmt.Errorf("core: active process-session roster has %d entries, maximum is %d", len(refs), maxActiveProcessSessionRefs)
	}
	if len(refs) == 0 {
		return nil
	}
	if runID == "" || workspaceID == "" {
		return errors.New("core: active process-session roster requires run and workspace identity")
	}
	seenActions := make(map[string]struct{}, len(refs))
	previousSessionID := ""
	for index, ref := range refs {
		for _, field := range []struct{ name, value string }{
			{"owner ID", ref.OwnerID}, {"workspace ID", ref.WorkspaceID}, {"run ID", ref.RunID},
			{"session ID", ref.SessionID}, {"owner generation", ref.OwnerGeneration}, {"start action ID", ref.StartActionID},
		} {
			name, value := field.name, field.value
			if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
				return fmt.Errorf("core: active process-session reference %d has invalid %s", index, name)
			}
			for _, char := range value {
				if unicode.IsControl(char) {
					return fmt.Errorf("core: active process-session reference %d has control characters in %s", index, name)
				}
			}
		}
		if ref.RunID != runID || ref.WorkspaceID != workspaceID {
			return fmt.Errorf("core: active process-session reference %d belongs to another run or workspace", index)
		}
		if previousSessionID != "" && ref.SessionID <= previousSessionID {
			return errors.New("core: active process-session references must be unique and sorted by session ID")
		}
		if _, duplicate := seenActions[ref.StartActionID]; duplicate {
			return errors.New("core: active process-session references repeat a start action ID")
		}
		seenActions[ref.StartActionID] = struct{}{}
		previousSessionID = ref.SessionID
	}
	return nil
}

// ActiveProcessSessionRoster renders only the opaque session IDs needed to
// address live sessions. Other lease facts remain durable metadata and are not
// copied into model context.
func ActiveProcessSessionRoster(refs []ActiveProcessSessionRef) (string, error) {
	if len(refs) == 0 {
		return "", nil
	}
	if err := ValidateActiveProcessSessionRefs(refs, refs[0].RunID, refs[0].WorkspaceID); err != nil {
		return "", err
	}
	var roster strings.Builder
	roster.WriteString("Active process sessions (use these exact IDs with process_session read, status, or stop):")
	for _, ref := range refs {
		_, _ = fmt.Fprintf(&roster, "\n- %q", ref.SessionID)
	}
	return roster.String(), nil
}

// ActiveProcessSessionSummaryContext adds the bounded session ID roster to a
// durable summary for model use while leaving the persisted summary text itself
// unchanged.
func ActiveProcessSessionSummaryContext(summary string, refs []ActiveProcessSessionRef) (string, error) {
	roster, err := ActiveProcessSessionRoster(refs)
	if err != nil || roster == "" {
		return summary, err
	}
	return summary + "\n\n" + roster, nil
}
