package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
)

func TestAssembleAgentUsesDedicatedACPBearerTokenEnv(t *testing.T) {
	const envName = "GO_CLI_TEST_ACP_BEARER_TOKEN"
	const token = "acpTok7"
	t.Setenv(envName, token)

	type requestObservation struct {
		path          string
		authorization string
		status        int
	}
	observed := make(chan requestObservation, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation := requestObservation{path: r.URL.Path, authorization: r.Header.Get("Authorization")}
		if observation.authorization != "Bearer "+token {
			observation.status = http.StatusUnauthorized
			observed <- observation
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		observation.status = http.StatusOK
		observed <- observation
		switch r.URL.Path {
		case "/acp/connect":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":"go-cli-acp-json/1","session_id":"assembly-auth-session"}`)
		case "/acp/stream":
			w.Header().Set("Content-Type", "application/x-ndjson")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		case "/acp/disconnect":
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := newAssembleTestConfig(t)
	cfg.ACP.Transport = "grpc"
	cfg.ACP.Endpoints = []string{server.URL + "/acp"}
	cfg.ACP.BearerTokenEnv = envName
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "acp-auth-test", io.Discard)
	require.NoError(t, err)
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer func() { _ = assembly.Close(closeCtx) }()

	select {
	case got := <-observed:
		require.Equal(t, "/acp/connect", got.path)
		require.Equal(t, "Bearer "+token, got.authorization)
		require.Equal(t, http.StatusOK, got.status)
	case <-time.After(time.Second):
		t.Fatal("config-assembled ACP client did not connect to the local auth peer")
	}

	// Assembly.Close cancels the blocked stream and authenticates its session cleanup.
	require.NoError(t, assembly.Close(context.Background()))
	seenDisconnect := false
	for {
		select {
		case got := <-observed:
			require.Equal(t, "Bearer "+token, got.authorization)
			require.Equal(t, http.StatusOK, got.status)
			if got.path == "/acp/disconnect" {
				seenDisconnect = true
			}
		case <-time.After(100 * time.Millisecond):
			require.True(t, seenDisconnect, "the authenticated session disconnect must reach the peer")
			return
		}
	}
}

func TestAssembleAgentFailsClosedForInvalidACPBearerTokenEnvBeforeHTTP(t *testing.T) {
	tests := []struct {
		name    string
		envName string
		value   string
	}{
		{name: "missing value", envName: "GO_CLI_TEST_MISSING_ACP_BEARER_TOKEN", value: ""},
		{name: "malformed reference", envName: "GO-CLI-TEST-ACP-TOKEN", value: "must-not-leak"},
		{name: "token with embedded padding", envName: "GO_CLI_TEST_ACP_TOKEN_EMBEDDED_PADDING", value: "abc=def"},
		{name: "token with leading padding", envName: "GO_CLI_TEST_ACP_TOKEN_LEADING_PADDING", value: "=abc"},
		{name: "padding without token", envName: "GO_CLI_TEST_ACP_TOKEN_PADDING_ONLY", value: "="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envName, tt.value)
			requests := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- struct{}{}
				http.Error(w, "unexpected request", http.StatusUnauthorized)
			}))
			defer server.Close()

			cfg := newAssembleTestConfig(t)
			cfg.ACP.Transport = "grpc"
			cfg.ACP.Endpoints = []string{server.URL + "/acp"}
			cfg.ACP.BearerTokenEnv = tt.envName
			assembly, err := AssembleAgent(context.Background(), cfg, "openai", "acp-auth-invalid-test", io.Discard)
			require.Error(t, err)
			require.Nil(t, assembly)
			if tt.value != "" {
				require.NotContains(t, err.Error(), tt.value)
			}
			select {
			case <-requests:
				t.Fatal("invalid explicit bearer-token configuration must fail before sending an HTTP request")
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestResolveACPBearerTokenAcceptsRFC6750TrailingPadding(t *testing.T) {
	for _, token := range []string{"abc", "abc=", "abc=="} {
		t.Run(token, func(t *testing.T) {
			const envName = "GO_CLI_TEST_ACP_PADDED_TOKEN"
			t.Setenv(envName, token)
			got, err := resolveACPHTTPBearerToken(context.Background(), nil, envName, "http://127.0.0.1:43210/acp")
			require.NoError(t, err)
			require.Equal(t, token, got)
		})
	}
}

func TestResolveACPBearerTokenFailsClosedForMissingOrMalformedExplicitRef(t *testing.T) {
	const secret = "noLeak7"
	chain := config.NewAuthChain().WithSources([]config.AuthSource{{
		Name: "exact-scope-test",
		LookupScoped: func(config.AuthScope) (string, error) {
			return secret, nil
		},
	}})

	tests := []struct {
		name   string
		envRef string
	}{
		{name: "unset dedicated environment variable", envRef: "GO_CLI_TEST_MISSING_ACP_TOKEN"},
		{name: "malformed environment variable reference", envRef: "GO-CLI-ACP-TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Contains(tt.envRef, "MISSING") {
				t.Setenv(tt.envRef, "")
			} else {
				t.Setenv(tt.envRef, secret)
			}
			got, err := resolveACPHTTPBearerToken(context.Background(), chain, tt.envRef, "http://127.0.0.1:43210/acp")
			require.Error(t, err)
			require.Empty(t, got)
			require.NotContains(t, err.Error(), secret)
		})
	}
}

func TestResolveACPBearerTokenUsesExactPrivateProfileScopeOnly(t *testing.T) {
	const llmSecret = "llmFake"
	const wrongScopeSecret = "wrong7"
	var requested config.AuthScope
	chain := config.NewAuthChain().WithSources([]config.AuthSource{
		{
			Name: "legacy-openai-key",
			Lookup: func(string) (string, error) {
				return llmSecret, nil
			},
		},
		{
			Name: "wrong-scope-key",
			LookupScoped: func(scope config.AuthScope) (string, error) {
				requested = scope
				if scope.ProtocolID == "openai-compatible" {
					return wrongScopeSecret, nil
				}
				return "", config.ErrAuthNotFound
			},
		},
	})

	token, err := resolveACPHTTPBearerToken(context.Background(), chain, "", "https://peer.example.test:443/acp/v1")
	require.NoError(t, err)
	require.Empty(t, token, "provider-only and wrong-protocol credentials must never authenticate ACP")
	require.Equal(t, config.AuthScope{
		ProviderID: "acp",
		ProtocolID: "go-cli-acp-json/1",
		Origin:     "https://peer.example.test:443",
	}, requested)
	require.NotContains(t, token, llmSecret)
	require.NotContains(t, token, wrongScopeSecret)
}

func TestResolveACPBearerTokenRedactsScopedSourceErrors(t *testing.T) {
	const secret = "errMsg7"
	chain := config.NewAuthChain().WithSources([]config.AuthSource{{
		Name: "broken-source",
		LookupScoped: func(config.AuthScope) (string, error) {
			return "", errors.New(secret)
		},
	}})

	got, err := resolveACPHTTPBearerToken(context.Background(), chain, "", "https://peer.example.test/acp")
	require.Error(t, err)
	require.Empty(t, got)
	require.NotContains(t, err.Error(), secret)
}
