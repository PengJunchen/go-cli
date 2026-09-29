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
	id                       ProviderOperationID
	denial                   *DispatchDenial
	beginErr                 error
	proposals                []CandidateProposal
	attempts                 []*attemptTestProviderAttempt
	permits                  []*DispatchPermit
	permitCopy               *DispatchPermit
	rejectedWrongOwner       bool
	rejectedAliasedCandidate bool
	onFinish                 func()
}

// attemptFencedTestModel adapts a mock model through the same operation-owner
// authorization and attempt terminal APIs as a guarded provider adapter.
type attemptFencedTestModel struct {
	inner     BaseChatModel
	candidate *resolvedCandidateHandle
}

func (m *attemptFencedTestModel) Generate(ctx context.Context, messages []Message, opts ...Option) (*Message, error) {
	attempt, err := (providerDispatchBinding{mode: GuardedCandidateBuild, candidate: m.candidate}).begin(ctx)
	if err != nil {
		return nil, err
	}
	message, err := m.inner.Generate(ctx, messages, opts...)
	var usage *Usage
	if message != nil {
		usage = message.Usage
	}
	return message, finishProviderAttempt(ctx, attempt, err, usage)
}

func (m *attemptFencedTestModel) Stream(ctx context.Context, messages []Message, opts ...Option) (<-chan MessageChunk, error) {
	attempt, err := (providerDispatchBinding{mode: GuardedCandidateBuild, candidate: m.candidate}).begin(ctx)
	if err != nil {
		return nil, err
	}
	attemptCtx, cancel := context.WithCancelCause(ctx)
	chunks, err := m.inner.Stream(attemptCtx, messages, opts...)
	if err != nil || chunks == nil {
		cancelStreamAttempt(ctx, cancel, err)
		drainMessageChunks(chunks)
		if err == nil {
			err = errors.New("test provider returned a nil stream")
		}
		if finishErr := finishProviderAttempt(ctx, attempt, err, nil); finishErr != nil {
			err = finishErr
		}
		return nil, err
	}
	return finishAttemptTestStream(ctx, cancel, chunks, attempt), nil
}

func finishAttemptTestStream(parent context.Context, cancel context.CancelCauseFunc, source <-chan MessageChunk, attempt *ProviderAttempt) <-chan MessageChunk {
	out := make(chan MessageChunk, cap(source))
	go func() {
		defer close(out)
		var usage *Usage
		var terminalErr error
		for chunk := range source {
			if chunk.Usage != nil {
				copyUsage := *chunk.Usage
				usage = &copyUsage
			}
			if chunk.Error != nil {
				terminalErr = chunk.Error
				cancelStreamAttempt(parent, cancel, terminalErr)
				continue
			}
			if terminalErr != nil {
				continue
			}
			select {
			case out <- chunk:
			case <-parent.Done():
				cause := context.Cause(parent)
				if cause == nil {
					cause = parent.Err()
				}
				terminalErr = cause
				cancelStreamAttempt(parent, cancel, cause)
			}
		}
		finishErr := finishProviderAttempt(parent, attempt, terminalErr, usage)
		if finishErr != nil {
			select {
			case out <- MessageChunk{Error: finishErr}:
			case <-parent.Done():
			}
		}
		cancel(nil)
	}()
	return out
}

var _ BaseChatModel = (*attemptFencedTestModel)(nil)

func attemptTestContext() context.Context {
	return withAttemptTestAuthority(context.Background())
}

func withAttemptTestAuthority(ctx context.Context) context.Context {
	return WithOperationDispatchAuthority(ctx, &attemptTestAuthority{id: "resolver-test-operation"})
}

func (a *attemptTestAuthority) ID() ProviderOperationID { return a.id }

func (a *attemptTestAuthority) AuthorizeCandidate(_ context.Context, proposal CandidateProposal) (DispatchDecision, error) {
	a.proposals = append(a.proposals, proposal)
	if a.denial != nil {
		return DispatchDecision{Denial: a.denial}, nil
	}
	return DispatchDecision{Authorized: true}, nil
}

func (a *attemptTestAuthority) RecordPreDispatchFailure(_ context.Context, err error) (PreDispatchCandidateFailure, error) {
	failure, ok := PreDispatchCandidateFailureFromError(err)
	if !ok {
		return PreDispatchCandidateFailure{}, errors.New("test authority: missing resolver pre-dispatch evidence")
	}
	return failure, nil
}

func (a *attemptTestAuthority) BeginDispatch(permit *DispatchPermit, candidate ResolvedCandidateHandle) (ProviderAttemptID, error) {
	if !permit.ValidFor(a, candidate) {
		return "", ErrDispatchPermitRequired
	}
	copy := *permit
	a.permitCopy = &copy
	a.rejectedWrongOwner = !permit.ValidFor(&attemptTestAuthority{id: a.id}, candidate)
	a.rejectedAliasedCandidate = !permit.ValidFor(a, &resolvedCandidateHandle{id: candidate.CandidateID()})
	a.permits = append(a.permits, permit)
	if a.beginErr != nil {
		return "", a.beginErr
	}
	attempt := &attemptTestProviderAttempt{id: ProviderAttemptID(fmt.Sprintf("attempt-%d", len(a.attempts)+1))}
	a.attempts = append(a.attempts, attempt)
	return attempt.id, nil
}

func TestDispatchPermitIsBoundToOwnerCandidateAndOneBegin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	resolved := newAttemptTestResolvedModel(t, server, "attempt-permit-binding")
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	authority := &attemptTestAuthority{id: "permit-binding-operation"}
	_, err = model.Generate(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)
	require.Len(t, authority.permits, 1)
	permit := authority.permits[0]
	require.True(t, authority.rejectedWrongOwner, "a permit must reject a different authority with the same operation ID")
	require.True(t, authority.rejectedAliasedCandidate, "a permit must reject a different handle with the same candidate ID")
	require.False(t, permit.ValidFor(authority, resolved.CandidateHandle()), "a permit is valid only during its one BeginDispatch callback")
	require.False(t, authority.permitCopy.ValidFor(authority, resolved.CandidateHandle()), "copying a permit must not copy its one-shot state")
	require.False(t, permit.ValidFor(&attemptTestAuthority{id: authority.id}, resolved.CandidateHandle()), "a permit is bound to the exact operation authority")
	require.False(t, permit.ValidFor(authority, &resolvedCandidateHandle{id: resolved.CandidateHandle().CandidateID()}), "a permit is bound to the exact resolved candidate handle")
}

func TestBeginDispatchRejectionDoesNotCreateAttemptOrReachHTTP(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"unexpected"}}]}`)
	}))
	defer server.Close()

	resolved := newAttemptTestResolvedModel(t, server, "attempt-begin-denied")
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	beginErr := errors.New("operation budget exhausted")
	authority := &attemptTestAuthority{id: "begin-denied-operation", beginErr: beginErr}
	_, err = model.Generate(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.ErrorIs(t, err, beginErr)
	require.Zero(t, requests, "BeginDispatch rejection must happen before client.Do")
	require.Empty(t, authority.attempts, "a rejected dispatch must not allocate an attempt ID")
	require.Len(t, authority.permits, 1)
	require.False(t, authority.permits[0].ValidFor(authority, resolved.CandidateHandle()), "the rejected permit cannot be replayed")
}

func (a *attemptTestAuthority) FinishAttempt(result ProviderAttemptResult) error {
	for _, attempt := range a.attempts {
		if attempt.id == result.AttemptID {
			if attempt.finished {
				return errors.New("attempt already finished")
			}
			attempt.finished = true
			attempt.result = cloneProviderAttemptResult(result)
			if a.onFinish != nil {
				a.onFinish()
			}
			return nil
		}
	}
	return errors.New("unknown attempt")
}

type attemptTestProviderAttempt struct {
	id       ProviderAttemptID
	finished bool
	result   ProviderAttemptResult
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

func TestResolvedBuildTreatsMissingOpenAIUsageTokensAsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`)
	}))
	defer server.Close()

	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(
		WithProviderName("attempt-unknown-usage"),
		WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: "attempt-unknown-usage",
		ModelID:    "unknown-usage-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	authority := &attemptTestAuthority{id: "unknown-usage-operation"}
	_, err = model.Generate(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)
	require.Len(t, authority.attempts, 1)
	assert.Equal(t, UsageUnknown, authority.attempts[0].result.Usage.State,
		"an empty usage object has no authoritative token counts and must not become known zero usage")
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

func TestResolvedOpenAINativeBuildTreatsMissingUsageTokensAsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`)
	}))
	defer server.Close()

	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewOpenAIProvider(WithNativeHTTPClient(server.Client()))))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: "openai",
		ModelID:    "native-unknown-usage-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	authority := &attemptTestAuthority{id: "native-unknown-usage-operation"}
	_, err = model.Generate(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)
	require.Len(t, authority.attempts, 1)
	assert.Equal(t, UsageUnknown, authority.attempts[0].result.Usage.State)
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

func TestResolvedBuildDenialAndCrossCandidateProposalDoNotReachHTTP(t *testing.T) {
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
			name: "cross-candidate proposal",
			setup: func(authority *attemptTestAuthority, candidate ResolvedCandidateHandle) context.Context {
				ctx := WithOperationDispatchAuthority(context.Background(), authority)
				other := &resolvedCandidateHandle{id: CandidateID(string(candidate.CandidateID()) + "-other")}
				return WithCandidateProposal(ctx, CandidateProposal{
					OperationID: authority.id,
					Candidate:   other,
					Reason:      CandidateRetry,
					Antecedent:  CandidateAntecedent{Kind: AntecedentInitial},
				})
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

func TestProviderClientTimeoutIsTransientUnlessRequestContextEnded(t *testing.T) {
	liveContext := context.Background()
	timeout := &providerTransportFailure{cause: context.DeadlineExceeded}
	terminal := providerAttemptTerminal(liveContext, timeout, nil)
	assert.Equal(t, AttemptTransportFailure, terminal.Outcome)
	assert.Equal(t, FailureTransientProvider, terminal.Failure)

	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	terminal = providerAttemptTerminal(canceledContext, &providerTransportFailure{cause: context.Canceled}, nil)
	assert.Equal(t, AttemptCancelled, terminal.Outcome)
	assert.Equal(t, FailureCancelled, terminal.Failure)
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

func TestGuardedStreamErrorFinishesAfterResponseBodyClose(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		writeSSE(w, `{ malformed json`)
		writeSSE(w, "[DONE]")
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
		WithProviderName("attempt-stream-error"),
		WithHTTPClient(client),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: "attempt-stream-error",
		ModelID:    "attempt-test-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	authority := &attemptTestAuthority{id: "stream-error-operation"}
	authority.onFinish = func() { finishSawClosedBody.Store(responseBodyClosed.Load()) }
	stream, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
	require.NoError(t, err)
	var streamErr error
	for chunk := range stream {
		if chunk.Error != nil {
			streamErr = chunk.Error
			assert.True(t, responseBodyClosed.Load(), "the response body must close before an error reaches the caller")
			require.Len(t, authority.attempts, 1)
			assert.True(t, authority.attempts[0].finished, "the provider attempt must finish before the error reaches the caller")
		}
	}

	require.Equal(t, 1, requests)
	require.Error(t, streamErr)
	assert.True(t, finishSawClosedBody.Load(), "Finish must join response-body cleanup on an error stream")
	require.Len(t, authority.attempts, 1)
	assert.True(t, authority.attempts[0].finished, "the attempt must finish before the output stream closes")
	assert.Equal(t, FailureProtocol, authority.attempts[0].result.Failure)
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
