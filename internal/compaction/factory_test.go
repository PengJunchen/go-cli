package compaction

import "testing"

func TestDefaultCompactorFactory_Create(t *testing.T) {
	f := NewDefaultCompactorFactory()

	tests := []struct {
		strategy string
		wantType string
		wantErr  bool
	}{
		{"", "*compaction.UnifiedCompactor", false},
		{"unified", "*compaction.UnifiedCompactor", false},
		{"micro", "*compaction.MicroCompactor", false},
		{"summary", "*compaction.SummaryCompactor", false},
		{"truncating", "*compaction.TruncatingCompactor", false},
		{"unknown", "", true},
		{"invalid", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.strategy, func(t *testing.T) {
			c, err := f.Create(tt.strategy)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for strategy %q, got nil", tt.strategy)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for strategy %q: %v", tt.strategy, err)
			}
			if c == nil {
				t.Fatalf("expected non-nil compactor for strategy %q", tt.strategy)
			}
		})
	}
}

func TestDefaultCompactorFactory_CompileTimeCheck(t *testing.T) {
	var _ CompactorFactory = (*DefaultCompactorFactory)(nil)
}
