package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"testing"
)

func startNoAuthLoopbackPolicyServer(t *testing.T) (*HTTPServer, string) {
	t.Helper()
	server := NewHTTPServer("no-auth-test", "127.0.0.1:0", nil)
	server.SetNoAuthLoopbackPolicy()
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start no-auth HTTP server: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) }) //nolint:errcheck // test cleanup
	return server, "http://" + server.Addr()
}

func connectWithAuthority(t *testing.T, baseURL, authority string, origins []string, withProfile bool) int {
	t.Helper()
	envelope, err := json.Marshal(httpACPEnvelope{
		Version:  ACPHTTPProfileV1,
		Type:     TypeConnect,
		SenderID: "origin-test-client",
	})
	if err != nil {
		t.Fatalf("marshal connect envelope: %v", err)
	}
	request := httpTestRequest(t, http.MethodPost, baseURL+"/connect", bytes.NewReader(envelope))
	request.Host = authority
	if !withProfile {
		request.Header.Del(ACPHTTPProfileHeader)
	}
	for _, origin := range origins {
		request.Header.Add("Origin", origin)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send connect request: %v", err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func TestHTTPServerNoAuthLoopbackPolicyAllowsLocalAuthoritiesAndNativePeers(t *testing.T) {
	_, baseURL := startNoAuthLoopbackPolicyServer(t)
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port := base.Port()

	tests := []struct {
		name    string
		host    string
		origins []string
	}{
		{
			name:    "IPv4 same origin",
			host:    net.JoinHostPort("127.0.0.1", port),
			origins: []string{"http://" + net.JoinHostPort("127.0.0.1", port)},
		},
		{
			name:    "localhost same origin",
			host:    net.JoinHostPort("localhost", port),
			origins: []string{"http://" + net.JoinHostPort("localhost", port)},
		},
		{
			name:    "IPv6 loopback same origin",
			host:    net.JoinHostPort("::1", port),
			origins: []string{"http://" + net.JoinHostPort("::1", port)},
		},
		{
			name:    "IPv4-mapped IPv6 same origin",
			host:    net.JoinHostPort("::ffff:127.0.0.1", port),
			origins: []string{"http://" + net.JoinHostPort("::ffff:127.0.0.1", port)},
		},
		{
			name:    "localhost trailing dot same origin",
			host:    "localhost.:" + port,
			origins: []string{"http://localhost.:" + port},
		},
		{
			name: "native CLI without Origin",
			host: net.JoinHostPort("127.0.0.1", port),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectWithAuthority(t, baseURL, tt.host, tt.origins, true); got != http.StatusOK {
				t.Fatalf("local connect got HTTP %d; want %d", got, http.StatusOK)
			}
		})
	}
}

func TestHTTPServerNoAuthLoopbackPolicyRejectsRemoteAndMalformedAuthorities(t *testing.T) {
	_, baseURL := startNoAuthLoopbackPolicyServer(t)
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port := base.Port()
	localHost := net.JoinHostPort("127.0.0.1", port)
	wrongPort := net.JoinHostPort("127.0.0.1", "1")
	if port == "1" {
		wrongPort = net.JoinHostPort("127.0.0.1", "2")
	}
	remoteHost := net.JoinHostPort("attacker.example", port)
	localOrigin := "http://" + localHost

	tests := []struct {
		name        string
		host        string
		origins     []string
		withProfile bool
	}{
		{
			name:        "non-loopback IP Host",
			host:        net.JoinHostPort("192.0.2.10", port),
			origins:     []string{"http://192.0.2.10:" + port},
			withProfile: true,
		},
		{
			name:        "wrong listener port",
			host:        wrongPort,
			origins:     []string{"http://" + wrongPort},
			withProfile: true,
		},
		{
			name:        "cross-origin browser Origin",
			host:        localHost,
			origins:     []string{"http://attacker.example:" + port},
			withProfile: true,
		},
		{
			name:        "non-local Origin",
			host:        localHost,
			origins:     []string{"http://192.0.2.10:" + port},
			withProfile: true,
		},
		{
			name:        "null Origin",
			host:        localHost,
			origins:     []string{"null"},
			withProfile: true,
		},
		{
			name:        "malformed Origin",
			host:        localHost,
			origins:     []string{"http://["},
			withProfile: true,
		},
		{
			name:        "multiple Origin headers",
			host:        localHost,
			origins:     []string{localOrigin, "http://localhost:" + port},
			withProfile: true,
		},
		{
			name:        "combined Origin values",
			host:        localHost,
			origins:     []string{localOrigin + ", http://localhost:" + port},
			withProfile: true,
		},
		{
			name:        "Origin with credentials",
			host:        localHost,
			origins:     []string{"http://user@127.0.0.1:" + port},
			withProfile: true,
		},
		{
			name:        "Origin with path",
			host:        localHost,
			origins:     []string{localOrigin + "/path"},
			withProfile: true,
		},
		{
			name:        "Origin with query",
			host:        localHost,
			origins:     []string{localOrigin + "?query=1"},
			withProfile: true,
		},
		{
			name:        "Origin with fragment",
			host:        localHost,
			origins:     []string{localOrigin + "#fragment"},
			withProfile: true,
		},
		{
			name:        "HTTPS Origin to HTTP server",
			host:        localHost,
			origins:     []string{"https://127.0.0.1:" + port},
			withProfile: true,
		},
		{
			name:        "Origin with a different local authority",
			host:        localHost,
			origins:     []string{"http://localhost:" + port},
			withProfile: true,
		},
		{
			name:        "Origin localhost with trailing dot differs from Host localhost",
			host:        "localhost:" + port,
			origins:     []string{"http://localhost.:" + port},
			withProfile: true,
		},
		{
			name:        "Origin localhost differs from Host localhost with trailing dot",
			host:        "localhost.:" + port,
			origins:     []string{"http://localhost:" + port},
			withProfile: true,
		},
		{
			name:        "IPv4 Origin differs from IPv4-mapped IPv6 Host",
			host:        net.JoinHostPort("::ffff:127.0.0.1", port),
			origins:     []string{"http://" + localHost},
			withProfile: true,
		},
		{
			name:        "IPv4-mapped IPv6 Origin differs from IPv4 Host",
			host:        localHost,
			origins:     []string{"http://" + net.JoinHostPort("::ffff:127.0.0.1", port)},
			withProfile: true,
		},
		{
			name:        "bad authority rejected before profile dispatch",
			host:        remoteHost,
			withProfile: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectWithAuthority(t, baseURL, tt.host, tt.origins, tt.withProfile); got != http.StatusForbidden {
				t.Fatalf("invalid authority got HTTP %d; want %d", got, http.StatusForbidden)
			}
		})
	}
}

func TestHTTPServerNoAuthLoopbackPolicyDoesNotChangeAuthenticatedServers(t *testing.T) {
	server := NewHTTPServer("authenticated-test", "127.0.0.1:0", nil)
	server.SetNoAuthLoopbackPolicy()
	server.SetAuth("test-token", "test-subject")
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("start authenticated HTTP server: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) }) //nolint:errcheck // test cleanup

	_, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split actual listener address: %v", err)
	}
	authority := net.JoinHostPort("attacker.example", port)
	envelope, err := json.Marshal(httpACPEnvelope{
		Version:  ACPHTTPProfileV1,
		Type:     TypeConnect,
		SenderID: "authenticated-client",
	})
	if err != nil {
		t.Fatalf("marshal connect envelope: %v", err)
	}
	request := httpTestRequest(t, http.MethodPost, "http://"+server.Addr()+"/connect", bytes.NewReader(envelope))
	request.Host = authority
	request.Header.Set("Origin", "http://"+authority)
	request.Header.Set("Authorization", "Bearer test-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send authenticated connect request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated request got HTTP %d; want %d", response.StatusCode, http.StatusOK)
	}
}

func TestHTTPServerNoAuthRejectsDNSRebindingAuthority(t *testing.T) {
	_, baseURL := startNoAuthLoopbackPolicyServer(t)
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	authority := net.JoinHostPort("attacker.example", base.Port())
	if got := connectWithAuthority(t, baseURL, authority, []string{"http://" + authority}, true); got != http.StatusForbidden {
		t.Fatalf("rebinding authority got HTTP %d; want %d", got, http.StatusForbidden)
	}
}
