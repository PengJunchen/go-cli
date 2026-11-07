package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

type sdkUsageCallerFixture struct{ response string }

func (c sdkUsageCallerFixture) call(_ context.Context, method string, _ any, result any) error {
	if method != "provider.generate" {
		return errors.New("unexpected SDK method")
	}
	return json.Unmarshal([]byte(c.response), result)
}

func (sdkUsageCallerFixture) close(context.Context) error { return nil }

func TestSDKModelUsageDistinguishesMissingCountsFromReportedZero(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		known    bool
	}{
		{name: "empty object", response: `{"role":"assistant","content":"ok","usage":{}}`},
		{name: "explicit zero", response: `{"role":"assistant","content":"ok","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &sdkModel{provider: "fixture", config: llm.ModelConfig{Model: "m"}, caller: sdkUsageCallerFixture{response: tc.response}}
			message, err := model.Generate(context.Background(), nil)
			require.NoError(t, err)
			require.NotNil(t, message)
			if !tc.known {
				require.Nil(t, message.Usage, "an empty SDK usage object does not prove zero token use")
				return
			}
			require.NotNil(t, message.Usage)
			require.Equal(t, 0, message.Usage.InputTokens)
			require.Equal(t, 0, message.Usage.OutputTokens)
			require.Equal(t, llm.UsageCoverageComplete, message.Usage.PrimaryCoverage)
		})
	}
}

func TestSDKModelUsagePreservesPrimaryCountPresenceAndBoundaries(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	literal, err := json.Marshal(api.Message{Role: "assistant", Usage: &api.Usage{InputTokens: 0, OutputTokens: 0, TotalTokens: 0}})
	require.NoError(t, err)
	for _, tc := range []struct {
		name          string
		response      string
		known         bool
		invalid       bool
		input, output int
	}{
		{name: "omitted", response: `{"role":"assistant"}`},
		{name: "null usage", response: `{"role":"assistant","usage":null}`},
		{name: "empty object", response: `{"role":"assistant","usage":{}}`},
		{name: "input only", response: `{"role":"assistant","usage":{"input_tokens":7}}`},
		{name: "output only", response: `{"role":"assistant","usage":{"output_tokens":4}}`},
		{name: "null input", response: `{"role":"assistant","usage":{"input_tokens":null,"output_tokens":0}}`},
		{name: "invalid type", response: `{"role":"assistant","usage":{"input_tokens":"0","output_tokens":0}}`, invalid: true},
		{name: "explicit zero", response: `{"role":"assistant","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`, known: true},
		{name: "extension API literal", response: string(literal), known: true},
		{name: "nonzero", response: `{"role":"assistant","usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}`, known: true, input: 7, output: 4},
		{name: "nonzero omitted total derives", response: `{"role":"assistant","usage":{"input_tokens":7,"output_tokens":4}}`, known: true, input: 7, output: 4},
		{name: "unrepresentable total", response: fmt.Sprintf(`{"role":"assistant","usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d}}`, maxInt, maxInt)},
		{name: "largest representable", response: fmt.Sprintf(`{"role":"assistant","usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d}}`, maxInt-1, maxInt), known: true, input: maxInt - 1, output: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &sdkModel{provider: "fixture", config: llm.ModelConfig{Model: "m"}, caller: sdkUsageCallerFixture{response: tc.response}}
			message, err := model.Generate(context.Background(), nil)
			if tc.invalid {
				require.Error(t, err, "invalid integer fields must fail decoding")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, message)
			if !tc.known {
				require.Nil(t, message.Usage, "absent or unrepresentable primary counts are not known zero")
				return
			}
			require.NotNil(t, message.Usage)
			require.Equal(t, tc.input, message.Usage.InputTokens)
			require.Equal(t, tc.output, message.Usage.OutputTokens)
			require.Equal(t, llm.UsageCoverageComplete, message.Usage.PrimaryCoverage)
			if tc.name == "nonzero omitted total derives" {
				require.Equal(t, 11, message.Usage.TotalTokens)
			}
		})
	}
}

type sdkUsageContractAuthority struct{ result llm.ProviderAttemptResult }

func (*sdkUsageContractAuthority) ID() llm.ProviderOperationID { return "sdk-usage-contract" }
func (*sdkUsageContractAuthority) AuthorizeCandidate(context.Context, llm.CandidateProposal) (llm.DispatchDecision, error) {
	return llm.DispatchDecision{Authorized: true}, nil
}
func (*sdkUsageContractAuthority) RecordPreDispatchFailure(_ context.Context, err error) (llm.PreDispatchCandidateFailure, error) {
	if failure, ok := llm.PreDispatchCandidateFailureFromError(err); ok {
		return failure, nil
	}
	return llm.PreDispatchCandidateFailure{}, fmt.Errorf("missing pre-dispatch evidence: %w", err)
}
func (a *sdkUsageContractAuthority) BeginDispatch(permit *llm.DispatchPermit, candidate llm.ResolvedCandidateHandle) (llm.ProviderAttemptID, error) {
	if !permit.ValidFor(a, candidate) {
		return "", llm.ErrDispatchPermitRequired
	}
	return "sdk-usage-contract-attempt", nil
}
func (a *sdkUsageContractAuthority) FinishAttempt(result llm.ProviderAttemptResult) error {
	a.result = result
	return nil
}

func TestSDKUsagePresenceReachesGuardedAttemptEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		known          bool
	}{
		{name: "empty object", response: `{"role":"assistant","content":"ok","usage":{}}`},
		{name: "explicit zero", response: `{"role":"assistant","content":"ok","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &sdkProvider{declaration: api.Provider{Name: "sdk-fixture", Models: []api.Model{{Name: "m"}}}, caller: sdkUsageCallerFixture{response: tc.response}}
			registry := llm.NewProviderRegistry()
			require.NoError(t, registry.RegisterExtensionProvider(provider))
			resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{ProviderID: "sdk-fixture", ModelID: "m"})
			require.NoError(t, err)
			model, cleanup, err := resolved.Build(context.Background())
			require.NoError(t, err)
			defer cleanup()
			authority := &sdkUsageContractAuthority{}
			message, err := model.Generate(llm.WithOperationDispatchAuthority(context.Background(), authority), []llm.Message{{Role: llm.RoleUser, Content: "request"}})
			require.NoError(t, err)
			require.NotNil(t, message)
			require.NotEmpty(t, authority.result.AttemptID)
			require.Equal(t, llm.AttemptSuccess, authority.result.Outcome)
			if tc.known {
				require.NotNil(t, message.Usage)
				require.Equal(t, llm.UsageKnown, authority.result.Usage.State)
				require.NotNil(t, authority.result.Usage.Usage)
				require.Equal(t, 0, authority.result.Usage.Usage.InputTokens)
				require.Equal(t, 0, authority.result.Usage.Usage.OutputTokens)
				return
			}
			require.Nil(t, message.Usage)
			require.Equal(t, llm.UsageUnknown, authority.result.Usage.State)
			require.Nil(t, authority.result.Usage.Usage)
		})
	}
}

func SDKUsageDispatchFixture(t *testing.T, body string) (*llm.Message, llm.ProviderAttemptResult) {
	t.Helper()
	provider := &sdkProvider{declaration: api.Provider{Name: "sdk-fixture", Models: []api.Model{{Name: "m"}}}, caller: sdkUsageCallerFixture{response: body}}
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.RegisterExtensionProvider(provider))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{ProviderID: "sdk-fixture", ModelID: "m"})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	authority := &sdkUsageContractAuthority{}
	message, err := model.Generate(llm.WithOperationDispatchAuthority(context.Background(), authority), []llm.Message{{Role: llm.RoleUser, Content: "request"}})
	require.NoError(t, err)
	return message, authority.result
}

func TestSDKWireUsagePreservesMessageAndToolFields(t *testing.T) {
	payload := `{"role":"assistant","content":"answer","finish_reason":"tool_calls","tool_calls":[{"id":"call","name":"read","args":{"path":"fixture"}}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}`
	model := &sdkModel{provider: "fixture", config: llm.ModelConfig{Model: "m"}, caller: sdkUsageCallerFixture{response: payload}}
	got, err := model.Generate(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, llm.RoleAssistant, got.Role)
	require.Equal(t, "answer", got.Content)
	require.Equal(t, "tool_calls", got.FinishReason)
	require.Len(t, got.ToolCalls, 1)
	require.Equal(t, "call", got.ToolCalls[0].ID)
	require.Equal(t, "read", got.ToolCalls[0].Name)
	require.NotNil(t, got.Usage)
	require.Equal(t, llm.UsageCoverageComplete, got.Usage.PrimaryCoverage)
	args, ok := got.ToolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fixture", args["path"])
}

type sdkMessageCaptureCaller struct {
	request  api.ModelRequest
	response string
}

func (c *sdkMessageCaptureCaller) call(_ context.Context, method string, input any, result any) error {
	if method != "provider.generate" {
		return errors.New("unexpected SDK method")
	}
	request, ok := input.(api.ModelRequest)
	if !ok {
		return fmt.Errorf("unexpected model request type %T", input)
	}
	c.request = request
	return json.Unmarshal([]byte(c.response), result)
}

func (*sdkMessageCaptureCaller) close(context.Context) error { return nil }

func TestSDKModelPreservesToolImageReceiptContentBlocks(t *testing.T) {
	caller := &sdkMessageCaptureCaller{response: `{"role":"assistant","content":"done"}`}
	model := &sdkModel{provider: "fixture", config: llm.ModelConfig{Model: "m"}, caller: caller}
	messages := []llm.Message{
		{Role: llm.RoleAssistant, Content: "inspect image", ToolCalls: []llm.ToolCall{{ID: "read-image", Name: "read_image"}}},
		{Role: llm.RoleTool, ToolCallID: "read-image", Name: "read_image", Content: "image receipt", ContentBlocks: []llm.ContentBlock{
			{Type: "text", Text: "image receipt"},
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,AA==", Detail: "high"}},
		}},
	}
	_, err := model.Generate(context.Background(), messages)
	require.NoError(t, err)
	require.Len(t, caller.request.Messages, 2)
	require.Equal(t, "tool", caller.request.Messages[1].Role)
	require.Equal(t, "read-image", caller.request.Messages[1].ToolCallID)
	require.Equal(t, "read_image", caller.request.Messages[1].Name)
	var blocks []llm.ContentBlock
	require.NoError(t, json.Unmarshal(caller.request.Messages[1].ContentBlocks, &blocks))
	require.Equal(t, messages[1].ContentBlocks, blocks, "extension SDK's JSON wire preserves tool image receipts as typed content blocks")
}
