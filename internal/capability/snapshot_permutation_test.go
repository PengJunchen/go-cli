package capability

import (
	"reflect"
	"testing"
)

func TestSnapshotIdentityIgnoresInputTraversalOrder(t *testing.T) {
	input := Input{Prompt: "same captured prompt", PolicyEpoch: 4, TrustEpoch: 9,
		Sources: []Source{{ID: "config", Kind: "config", Content: []byte("config bytes")}, {ID: "skills", Kind: "catalog", Content: []byte("catalog bytes")}},
		Nodes: []Node{
			{ID: "store", Owner: "a", Provides: []string{"store.read", "store.write"}},
			{ID: "catalog", Owner: "b", Provides: []string{"catalog.list", "catalog.load"}},
			{ID: "agent", Owner: "c", Requires: []string{"store.read", "catalog.load", "store.write", "catalog.list"}, Provides: []string{"agent.run", "agent.inspect"}, Permissions: []string{"read", "write"}},
		}, GrantedPermissions: []string{"read", "write"}}
	want, err := NewSnapshot(input)
	if err != nil {
		t.Fatal(err)
	}
	// Exhaust all source and node permutations; reverse every dependency and
	// declaration slice independently of the outer traversal.
	for sourceOrder := range 2 {
		for first := range 3 {
			for second := range 3 {
				if first == second {
					continue
				}
				third := 3 - first - second
				for reverse := range 2 {
					candidate := input
					candidate.Sources = []Source{input.Sources[sourceOrder], input.Sources[1-sourceOrder]}
					candidate.Nodes = cloneNodes([]Node{input.Nodes[first], input.Nodes[second], input.Nodes[third]})
					if reverse == 1 {
						for index := range candidate.Nodes {
							node := &candidate.Nodes[index]
							for _, values := range [][]string{node.Requires, node.Provides, node.Permissions} {
								for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
									values[left], values[right] = values[right], values[left]
								}
							}
						}
						candidate.GrantedPermissions = []string{"write", "read"}
					}
					got, buildErr := NewSnapshot(candidate)
					if buildErr != nil {
						t.Fatal(buildErr)
					}
					if got.ID() != want.ID() || got.CapabilityDigest() != want.CapabilityDigest() || got.PromptDigest() != want.PromptDigest() || !reflect.DeepEqual(got.Nodes(), want.Nodes()) || !reflect.DeepEqual(got.Sources(), want.Sources()) {
						t.Fatalf("source/node/declaration permutation changed identity: sources=%d nodes=%d,%d,%d reverse=%d", sourceOrder, first, second, third, reverse)
					}
				}
			}
		}
	}
}
