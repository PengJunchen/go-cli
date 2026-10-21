// Package capability owns immutable capability identities and bounded runtime generations.
package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Source identifies captured source bytes. Digests prove content, not provenance.
type Source struct {
	ID, Kind, Digest string
	Content          []byte `json:"-"`
}

// Node declares a capability's dependencies and deterministic ordering.
type Node struct {
	ID, Owner                       string
	Requires, Provides, Permissions []string
	Phase, Priority                 int
}

// Input contains captured, verified inputs; secret values must use opaque handles.
type Input struct {
	Sources                 []Source
	Nodes                   []Node
	Prompt                  string
	PolicyEpoch, TrustEpoch uint64
	GrantedPermissions      []string
}

type snapshotData struct {
	id, promptDigest, capabilityDigest string
	sources                            []Source
	nodes                              []Node
	policyEpoch, trustEpoch            uint64
}

// Snapshot exposes detached identity metadata and never exposes runtime resources.
type Snapshot struct{ data *snapshotData }

// ID returns the content identity of the complete captured input.
func (s Snapshot) ID() string {
	if s.data == nil {
		return ""
	}
	return s.data.id
}

// PromptDigest returns the digest of the actual prompt bytes.
func (s Snapshot) PromptDigest() string {
	if s.data == nil {
		return ""
	}
	return s.data.promptDigest
}

// CapabilityDigest returns the digest of the dependency-ordered capability plan.
func (s Snapshot) CapabilityDigest() string {
	if s.data == nil {
		return ""
	}
	return s.data.capabilityDigest
}

// PolicyEpoch returns the policy revision captured during compilation.
func (s Snapshot) PolicyEpoch() uint64 {
	if s.data == nil {
		return 0
	}
	return s.data.policyEpoch
}

// TrustEpoch returns the project trust revision captured during compilation.
func (s Snapshot) TrustEpoch() uint64 {
	if s.data == nil {
		return 0
	}
	return s.data.trustEpoch
}

// Sources returns detached source identities without retaining source bytes.
func (s Snapshot) Sources() []Source {
	if s.data == nil {
		return nil
	}
	return append([]Source(nil), s.data.sources...)
}

// Nodes returns a detached dependency-ordered plan.
func (s Snapshot) Nodes() []Node {
	if s.data == nil {
		return nil
	}
	return cloneNodes(s.data.nodes)
}

// NewSnapshot validates a graph before resources are created and hashes canonical input.
func NewSnapshot(input Input) (Snapshot, error) {
	sources := append([]Source(nil), input.Sources...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	for i := range sources {
		if sources[i].ID == "" || (i > 0 && sources[i-1].ID == sources[i].ID) {
			return Snapshot{}, fmt.Errorf("capability: duplicate or empty source %q", sources[i].ID)
		}
		if sources[i].Content != nil {
			digest := hash(sources[i].Content)
			if sources[i].Digest != "" && sources[i].Digest != digest {
				return Snapshot{}, fmt.Errorf("capability: source %q digest mismatch", sources[i].ID)
			}
			sources[i].Digest = digest
		}
		sources[i].Content = nil
	}
	nodes, err := buildOrder(input.Nodes, input.GrantedPermissions)
	if err != nil {
		return Snapshot{}, err
	}
	nodeBytes, err := json.Marshal(nodes)
	if err != nil {
		return Snapshot{}, fmt.Errorf("capability: encode nodes: %w", err)
	}
	promptDigest := hash([]byte(input.Prompt))
	canonical, err := json.Marshal(struct {
		Version                 string
		Sources                 []Source
		Nodes                   []Node
		PromptDigest            string
		PolicyEpoch, TrustEpoch uint64
	}{"capability/v1", sources, nodes, promptDigest, input.PolicyEpoch, input.TrustEpoch})
	if err != nil {
		return Snapshot{}, fmt.Errorf("capability: encode snapshot: %w", err)
	}
	return Snapshot{data: &snapshotData{id: hash(canonical), promptDigest: promptDigest, capabilityDigest: hash(nodeBytes), sources: sources, nodes: nodes, policyEpoch: input.PolicyEpoch, trustEpoch: input.TrustEpoch}}, nil
}

func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func cloneNodes(input []Node) []Node {
	out := append([]Node(nil), input...)
	for i := range out {
		out[i].Requires = append([]string(nil), out[i].Requires...)
		out[i].Provides = append([]string(nil), out[i].Provides...)
		out[i].Permissions = append([]string(nil), out[i].Permissions...)
	}
	return out
}

func buildOrder(input []Node, granted []string) ([]Node, error) {
	nodes := cloneNodes(input)
	less := func(a, b Node) bool {
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		return a.ID < b.ID
	}
	sort.Slice(nodes, func(i, j int) bool { return less(nodes[i], nodes[j]) })
	ids, providers, permissions := map[string]int{}, map[string]int{}, map[string]bool{}
	for _, permission := range granted {
		permissions[permission] = true
	}
	for i := range nodes {
		n := &nodes[i]
		if _, duplicate := ids[n.ID]; n.ID == "" || duplicate {
			return nil, fmt.Errorf("capability: duplicate or empty node %q", n.ID)
		}
		ids[n.ID] = i
		sort.Strings(n.Requires)
		sort.Strings(n.Provides)
		sort.Strings(n.Permissions)
		for _, permission := range n.Permissions {
			if !permissions[permission] {
				return nil, fmt.Errorf("capability: node %q requires unauthorized permission %q", n.ID, permission)
			}
		}
		for _, provided := range n.Provides {
			if _, duplicate := providers[provided]; provided == "" || duplicate {
				return nil, fmt.Errorf("capability: duplicate or empty provider %q", provided)
			}
			providers[provided] = i
		}
	}
	degree, outgoing := make([]int, len(nodes)), make([][]int, len(nodes))
	for i, n := range nodes {
		seen := map[int]bool{}
		for _, required := range n.Requires {
			provider, found := providers[required]
			if !found {
				return nil, fmt.Errorf("capability: node %q missing provider %q", n.ID, required)
			}
			if !seen[provider] {
				degree[i]++
				outgoing[provider] = append(outgoing[provider], i)
				seen[provider] = true
			}
		}
	}
	result := make([]Node, 0, len(nodes))
	used := make([]bool, len(nodes))
	for len(result) < len(nodes) {
		pick := -1
		for i := range nodes {
			if !used[i] && degree[i] == 0 {
				pick = i
				break
			}
		}
		if pick < 0 {
			return nil, fmt.Errorf("capability: dependency cycle")
		}
		used[pick] = true
		result = append(result, nodes[pick])
		for _, next := range outgoing[pick] {
			degree[next]--
		}
	}
	return result, nil
}
