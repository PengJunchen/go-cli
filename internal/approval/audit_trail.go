package approval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// AuditEntry records a single approval classification decision for the
// append-only audit trail. Each field is exported so the entry serialises to
// JSON with stable key names.
type AuditEntry struct {
	// Timestamp is when the classification decision was made (UTC).
	Timestamp time.Time `json:"timestamp"`
	// Tool is the name of the tool that was classified.
	Tool string `json:"tool"`
	// ArgsSummary is a brief, length-capped summary of the tool call arguments.
	ArgsSummary string `json:"args_summary"`
	// Decision is the classification outcome: "allow", "deny", or "ask".
	Decision string `json:"decision"`
	// Classifier is the name of the classifier that produced the decision.
	Classifier string `json:"classifier"`
	// PermissionMode is the active permission mode when the decision was made.
	PermissionMode string `json:"permission_mode"`
	// SessionID is the identifier of the session the decision belongs to.
	SessionID string `json:"session_id"`
}

// AuditTrail is an append-only audit log for approval decisions. Each Record
// call appends a JSON line to {dir}/{date}.jsonl. The file is opened with
// O_APPEND|O_CREATE|O_WRONLY so concurrent or sequential calls never overwrite
// prior entries.
type AuditTrail struct {
	dir string
	mu  sync.Mutex
}

// NewAuditTrail creates an AuditTrail that writes daily JSONL files under dir.
// The directory is created lazily on the first Record call.
func NewAuditTrail(dir string) *AuditTrail {
	return &AuditTrail{dir: dir}
}

// Record writes an audit entry to the daily JSONL file. The file is opened with
// O_APPEND|O_CREATE|O_WRONLY, ensuring append-only semantics. A nil receiver is
// a no-op so callers can safely disable auditing by passing nil.
func (a *AuditTrail) Record(entry AuditEntry) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return fmt.Errorf("audit: mkdir: %w", err)
	}

	filename := filepath.Join(a.dir, entry.Timestamp.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}

// summarizeArgs produces a brief, deterministic summary of tool call arguments.
// It lists the argument keys (sorted) joined by commas, capped at 200 bytes.
// This avoids logging full argument values, which may contain sensitive data.
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	summary := strings.Join(keys, ",")
	if len(summary) > 200 {
		// Truncate at the last valid UTF-8 boundary <= 200 bytes.
		trunc := summary[:200]
		for len(trunc) > 0 && !utf8.ValidString(trunc) {
			trunc = trunc[:len(trunc)-1]
		}
		summary = trunc + "..."
	}
	return summary
}
