package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderAdapterClassifiesRequestSerializationFailureBeforeDispatch(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"unexpected"}}]}`)
	}))
	defer server.Close()

	resolved := newAttemptTestResolvedModel(t, server, "predispatch-invalid-request")
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	authority := &attemptTestAuthority{id: "predispatch-request-operation"}
	ctx := WithOperationDispatchAuthority(context.Background(), authority)
	_, err = model.Generate(ctx, []Message{{Role: RoleUser, Content: "request"}}, WithTools([]ToolDefinition{{
		Name:       "unencodable",
		Parameters: make(chan int),
	}}))

	var setupErr *PreDispatchCandidateError
	require.ErrorAs(t, err, &setupErr)
	require.Equal(t, PreDispatchInvalidRequest, setupErr.Class)
	require.Equal(t, resolved.CandidateHandle().CandidateID(), setupErr.CandidateID)
	var jsonErr *json.UnsupportedTypeError
	require.ErrorAs(t, err, &jsonErr)
	require.Zero(t, requests, "request serialization failures must not cross client.Do")
	require.Empty(t, authority.attempts, "pre-dispatch failures must not create attempt rows")
}

type failingCredentialResolver struct{ err error }

func (r failingCredentialResolver) ResolveCredential(context.Context, string, string, string) (string, bool, error) {
	return "", false, r.err
}

func TestResolverClassifiesCredentialFailureWithoutCandidateIdentity(t *testing.T) {
	registry := NewProviderRegistry()
	registry.SetCredentialResolver(failingCredentialResolver{err: errors.New("credential source failed")})
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("predispatch-credential"), WithBaseURL("https://provider.example.test"))))

	_, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: "predispatch-credential",
		ModelID:    "m",
	})
	var setupErr *PreDispatchCandidateError
	require.ErrorAs(t, err, &setupErr)
	require.Equal(t, PreDispatchAuth, setupErr.Class)
	require.Empty(t, setupErr.CandidateID, "credential resolution fails before an opaque candidate exists")
	require.NotContains(t, err.Error(), "credential source failed", "setup diagnostics must not reveal credential resolver details")
}

func TestProviderAdapterClassifiesCancellationBeforeDispatch(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"unexpected"}}]}`)
	}))
	defer server.Close()

	resolved := newAttemptTestResolvedModel(t, server, "predispatch-cancelled")
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	authority := &attemptTestAuthority{id: "predispatch-cancel-operation"}
	ctx, cancel := context.WithCancel(WithOperationDispatchAuthority(context.Background(), authority))
	cancel()
	_, err = model.Generate(ctx, []Message{{Role: RoleUser, Content: "request"}})

	var setupErr *PreDispatchCandidateError
	require.ErrorAs(t, err, &setupErr)
	require.Equal(t, PreDispatchCancelled, setupErr.Class)
	require.Equal(t, resolved.CandidateHandle().CandidateID(), setupErr.CandidateID)
	require.Zero(t, requests, "cancellation before the dispatch fence must not call client.Do")
	require.Empty(t, authority.attempts, "cancellation before the fence must not create an attempt row")
}

func TestResolvedCandidateClassifiesMissingGuardedAdapterAsUnavailable(t *testing.T) {
	candidate := &resolvedCandidateHandle{id: "unavailable-candidate"}
	_, _, err := candidate.build(context.Background())
	var setupErr *PreDispatchCandidateError
	require.ErrorAs(t, err, &setupErr)
	require.Equal(t, PreDispatchCandidateUnavailable, setupErr.Class)
	require.Equal(t, candidate.id, setupErr.CandidateID)
}
