package llm

import (
	"encoding/json"
	"testing"
)

func TestRuntimeMetadataIsTaggedAndNeverSerializedToProviderJSON(t *testing.T) {
	ref := SourceRef{
		Kind: SourcePriorPublished, HistoryRevision: "revision-1", BranchID: "branch-1",
		SourceEntryID: "entry-7", PriorOrdinal: 0,
	}
	if err := ref.Validate(); err != nil {
		t.Fatalf("valid prior source rejected: %v", err)
	}
	if err := (SourceRef{Kind: SourcePriorPublished, HistoryRevision: "revision-1"}).Validate(); err == nil {
		t.Fatal("incomplete prior source accepted")
	}

	encoded, err := json.Marshal(Message{
		Role: RoleUser, Content: "hello",
		Runtime: RuntimeMetadata{Source: ref},
	})
	if err != nil {
		t.Fatalf("marshal provider message: %v", err)
	}
	if string(encoded) != `{"role":"user","content":"hello"}` {
		t.Fatalf("runtime-only source metadata leaked into provider JSON: %s", encoded)
	}
}
