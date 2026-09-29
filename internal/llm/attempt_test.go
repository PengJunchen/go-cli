package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type attemptTestAuthority struct {
	id                   ProviderOperationID
	denial               *DispatchDenial
	wrongPermitCandidate bool
	proposals            []CandidateProposal
	attempts             []*attemptTestProviderAttempt
	onFinish             func()
}

func attemptTestContext() context.Context {
	return WithOperationDispatchAuthority(context.Background(), &attemptTestAuthority{id: "resolver-test-operation"})
}

func (a *attemptTestAuthority) ID() ProviderOperationID { return a.id }

func (a *attemptTestAuthority) AuthorizeCandidate(_ context.Context, proposal CandidateProposal) (DispatchDecision, error) {
	a.proposals = append(a.proposals, proposal)
	if a.denial != nil {
		return DispatchDecision{Denial: a.denial}, nil
	}
	candidateID := proposal.Candidate.CandidateID()
	if a.wrongPermitCandidate {
		candidateID = "another-candidate"
	}
	return DispatchDecision{Permit: &attemptTestPermit{
		owner:       a,
		operationID: a.id,
		candidateID: candidateID,
		candidate:   proposal.Candidate,
	}}, nil
}

func (a *attemptTestAuthority) BeginDispatch(permit DispatchPermit, candidate ResolvedCandidateHandle) (ProviderAttempt, error) {
	issued, ok := permit.(*attemptTestPermit)
	if !ok || issued.owner != a || issued.consumed || issued.operationID != a.id || issued.candidate != candidate {
		return nil, ErrDispatchPermitRequired
	}
	issued.consumed = true
	attempt := &attemptTestProviderAttempt{
		id:        ProviderAttemptID(fmt.Sprintf("attempt-%d", len(a.attempts)+1)),
		operation: a.id,
		identity:  candidate.Identity(),
		onFinish:  a.onFinish,
	}
	a.attempts = append(a.attempts, attempt)
	return attempt, nil
}

type attemptTestPermit struct {
	owner       *attemptTestAuthority
	operationID ProviderOperationID
	candidateID CandidateID
	candidate   ResolvedCandidateHandle
	consumed    bool
}

func (p *attemptTestPermit) OperationID() ProviderOperationID { return p.operationID }
func (p *attemptTestPermit) CandidateID() CandidateID         { return p.candidateID }

type attemptTestProviderAttempt struct {
	id        ProviderAttemptID
	operation ProviderOperationID
	identity  AttemptIdentity
	finished  bool
	terminal  ProviderAttemptTerminal
	result    ProviderAttemptResult
	onFinish  func()
}

func (a *attemptTestProviderAttempt) ID() ProviderAttemptID { return a.id }

func (a *attemptTestProviderAttempt) Finish(terminal ProviderAttemptTerminal) (ProviderAttemptResult, error) {
	if a.finished {
		return ProviderAttemptResult{}, errors.New("attempt already finished")
	}
	a.finished = true
	a.terminal = cloneProviderAttemptTerminal(terminal)
	if a.onFinish != nil {
		a.onFinish()
	}
	a.result = ProviderAttemptResult{
		OperationID:             a.operation,
		AttemptID:               a.id,
		Identity:                a.identity,
		Outcome:                 terminal.Outcome,
		Failure:                 terminal.Failure,
		Usage:                   cloneUsageEvidence(terminal.Usage),
		ProviderReportedCostUSD: cloneFloat(terminal.ProviderReportedCostUSD),
	}
	return a.result, nil
}

func newAttemptTestResolvedModel(t *testing.T, server *httptest.Server, providerID string) ResolvedModel {
	t.Helper()
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(
		WithProviderName(providerID),
		WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: providerID,
		ModelID:    "attempt-test-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	return resolved
}

func TestResolvedBuildFailsClosedWithoutOperationAuthority(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	resolved := newAttemptTestResolvedModel(t, server, "attempt-guard")
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "private prompt canary"}})
	require.ErrorIs(t, err, ErrOperationAuthorityRequired)
	assert.Zero(t, requests, "a resolver-built model without authority must stop before client.Do")
}

func TestZeroValueBuildModeDoesNotImplyUnboundLibraryMode(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"unexpected"}}]}`)
	}))
	defer server.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithProviderName("zero-mode")),
		cfg:      ModelConfig{Model: "zero-mode-model", BaseURL: server.URL},
		client:   server.Client(),
	}
	_, err := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "request"}})
	require.ErrorIs(t, err, ErrGuardedCandidateUnavailable)
	assert.Zero(t, requests, "zero-value state must not silently create unbound HTTP capability")
}

func TestResolvedBuildUsesMatchingOperationPermitAtHTTPFence(t *testing.T) {
	secret := "attempt-credential-canary"
	var gotAuthorization string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer server.Close()

	registry := NewProviderRegistry()
	registry.SetCredentialResolver(&countingCredentialResolver{key: secret})
	require.NoError(t, registry.Register(NewEinoProvider(
		WithProviderName("attempt-guard"),
		WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: "attempt-guard",
		ModelID:    "attempt-test-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)

	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	authority := &attemptTestAuthority{id: "main-operation"}
	ctx := WithOperationDispatchAuthority(context.Background(), authority)
	_, err = model.Generate(ctx, []Message{{Role: RoleUser, Content: "prompt-canary"}})
	require.NoError(t, err)

	require.Equal(t, 1, requests)
	assert.Equal(t, "Bearer "+secret, gotAuthorization)
	require.Len(t, authority.proposals, 1)
	assert.Equal(t, CandidateInitial, authority.proposals[0].Reason)
	assert.Equal(t, AntecedentInitial, authority.proposals[0].Antecedent.Kind)
	require.Len(t, authority.attempts, 1)
	attempt := authority.attempts[0]
	require.True(t, attempt.finished)
	assert.Equal(t, AttemptSuccess, attempt.result.Outcome)
	assert.Equal(t, UsageKnown, attempt.result.Usage.State)
	require.NotNil(t, attempt.result.Usage.Usage)
	assert.Equal(t, 5, attempt.result.Usage.Usage.InputTokens)
	assert.Equal(t, 2, attempt.result.Usage.Usage.OutputTokens)
	assert.Equal(t, "attempt-guard", attempt.result.Identity.ProviderID)
	assert.Equal(t, "attempt-test-model", attempt.result.Identity.ModelID)
	assert.Equal(t, ProtocolOpenAICompatible, attempt.result.Identity.ProtocolID)
	assert.Equal(t, OriginExplicitRequestEndpoint, attempt.result.Identity.OriginKind)

	encodedIdentity, err := json.Marshal(attempt.result.Identity)
	require.NoError(t, err)
	identityText := string(encodedIdentity)
	assert.NotContains(t, identityText, server.URL)
	assert.NotContains(t, identityText, secret)
	assert.NotContains(t, identityText, "prompt-canary")
}

func TestResolvedOpenAINativeBuildUsesTheSameAttemptFence(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer server.Close()

	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewOpenAIProvider(WithNativeHTTPClient(server.Client()))))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: "openai",
		ModelID:    "native-attempt-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	authority := &attemptTestAuthority{id: "native-operation"}
	_, err = model.Generate(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)

	assert.Equal(t, 1, requests)
	require.Len(t, authority.attempts, 1)
	attempt := authority.attempts[0]
	require.True(t, attempt.finished)
	assert.Equal(t, "openai", attempt.result.Identity.ProviderID)
	assert.Equal(t, ProtocolOpenAICompatible, attempt.result.Identity.ProtocolID)
	assert.Equal(t, UsageKnown, attempt.result.Usage.State)
}

func TestResolvedCandidateIDsAreOpaqueAndUnique(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("attempt-default"), WithBaseURL(server.URL))))
	resolver := registry.SnapshotResolver()

	first, err := resolver.ResolveModel(context.Background(), ModelRequest{ProviderID: "attempt-default", ModelID: "m"})
	require.NoError(t, err)
	second, err := resolver.ResolveModel(context.Background(), ModelRequest{ProviderID: "attempt-default", ModelID: "m"})
	require.NoError(t, err)
	firstCandidate := first.CandidateHandle()
	secondCandidate := second.CandidateHandle()
	require.NotNil(t, firstCandidate)
	require.NotNil(t, secondCandidate)

	assert.Equal(t, OriginResolverDefaultEndpoint, firstCandidate.Identity().OriginKind)
	assert.NotEmpty(t, firstCandidate.CandidateID())
	assert.NotEqual(t, firstCandidate.CandidateID(), secondCandidate.CandidateID())
	encoded, err := json.Marshal(firstCandidate)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), server.URL)
}

func TestResolvedBuildDenialAndMismatchedPermitDoNotReachHTTP(t *testing.T) {
	for _, test := range []struct {
		name    string
		setup   func(*attemptTestAuthority, ResolvedCandidateHandle) context.Context
		wantErr error
	}{
		{
			name: "policy denial",
			setup: func(authority *attemptTestAuthority, _ ResolvedCandidateHandle) context.Context {
				authority.denial = &DispatchDenial{Code: DeniedSafetyFact}
				return WithOperationDispatchAuthority(context.Background(), authority)
			},
			wantErr: ErrDispatchPermitDenied,
		},
		{
			name: "wrong candidate permit",
			setup: func(authority *attemptTestAuthority, candidate ResolvedCandidateHandle) context.Context {
				permit := &attemptTestPermit{
					owner:       authority,
					operationID: authority.id,
					candidateID: "different-candidate",
					candidate:   candidate,
				}
				ctx := WithOperationDispatchAuthority(context.Background(), authority)
				return WithDispatchPermit(ctx, permit)
			},
			wantErr: ErrDispatchPermitRequired,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"unexpected"}}]}`)
			}))
			defer server.Close()

			resolved := newAttemptTestResolvedModel(t, server, "attempt-denial")
			model, cleanup, err := resolved.Build(context.Background())
			require.NoError(t, err)
			defer cleanup()
			authority := &attemptTestAuthority{id: "denied-operation"}
			ctx := test.setup(authority, resolved.CandidateHandle())
			_, err = model.Generate(ctx, []Message{{Role: RoleUser, Content: "request"}})
			require.ErrorIs(t, err, test.wantErr)
			assert.Zero(t, requests)
			assert.Empty(t, authority.attempts)
		})
	}
}

func TestDispatchedProviderFailureFinishesWithUnknownUsage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
		writeResponse(t, w, `{"error":{"message":"temporary"}}`)
	}))
	defer server.Close()

	resolved := newAttemptTestResolvedModel(t, server, "attempt-error")
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	authority := &attemptTestAuthority{id: "error-operation"}
	_, err = model.Generate(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.Error(t, err)

	assert.Equal(t, 1, requests)
	require.Len(t, authority.attempts, 1)
	attempt := authority.attempts[0]
	require.True(t, attempt.finished)
	assert.Equal(t, AttemptProviderFailure, attempt.result.Outcome)
	assert.Equal(t, FailureTransientProvider, attempt.result.Failure)
	assert.Equal(t, UsageUnknown, attempt.result.Usage.State)
	assert.Nil(t, attempt.result.Usage.Usage)
}

func TestDirectBuildRemainsUnboundOneCall(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	provider := NewEinoProvider(WithProviderName("direct-attempt-test"), WithHTTPClient(server.Client()))
	model, cleanup, err := provider.Build(context.Background(), ModelConfig{Model: "direct-model", BaseURL: server.URL})
	require.NoError(t, err)
	defer cleanup()
	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
}

func TestDirectNativeBuildRemainsUnboundOneCall(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	provider := NewOpenAIProvider(WithNativeHTTPClient(server.Client()))
	model, cleanup, err := provider.Build(context.Background(), ModelConfig{Model: "direct-native-model", BaseURL: server.URL})
	require.NoError(t, err)
	defer cleanup()
	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
}

func TestGuardedStreamFinishesAfterSourceClosesWithUsage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			"",
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"",
			`data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))
	}))
	defer server.Close()
	var responseBodyClosed atomic.Bool
	var finishSawClosedBody atomic.Bool
	client := &http.Client{Transport: closeTrackingRoundTripper{
		next:   server.Client().Transport,
		closed: &responseBodyClosed,
	}}

	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(
		WithProviderName("attempt-stream"),
		WithHTTPClient(client),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: "attempt-stream",
		ModelID:    "attempt-test-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	authority := &attemptTestAuthority{id: "stream-operation"}
	authority.onFinish = func() { finishSawClosedBody.Store(responseBodyClosed.Load()) }
	stream, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)
	var chunks []MessageChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}

	assert.Equal(t, 1, requests)
	assert.True(t, finishSawClosedBody.Load(), "the source response body must close before Finish")
	assert.NotEmpty(t, chunks)
	require.Len(t, authority.attempts, 1)
	attempt := authority.attempts[0]
	require.True(t, attempt.finished, "the attempt must finish before the adapter output channel closes")
	assert.Equal(t, AttemptSuccess, attempt.result.Outcome)
	assert.Equal(t, UsageKnown, attempt.result.Usage.State)
	require.NotNil(t, attempt.result.Usage.Usage)
	assert.Equal(t, 4, attempt.result.Usage.Usage.InputTokens)
	assert.Equal(t, 3, attempt.result.Usage.Usage.OutputTokens)
}

type closeTrackingRoundTripper struct {
	next   http.RoundTripper
	closed *atomic.Bool
}

func (t closeTrackingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &closeTrackingReadCloser{ReadCloser: response.Body, closed: t.closed}
	return response, nil
}

type closeTrackingReadCloser struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (r *closeTrackingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.closed.Store(true)
	return err
}
