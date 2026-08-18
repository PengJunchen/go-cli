package acp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generateSelfSignedCert creates a temporary self-signed TLS certificate and
// key pair, returning their file paths. The files are created in t.TempDir()
// and cleaned up automatically.
func generateSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("failed to create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		certOut.Close()
		t.Fatalf("failed to write cert: %v", err)
	}
	certOut.Close()

	keyDer, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("failed to create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}); err != nil {
		keyOut.Close()
		t.Fatalf("failed to write key: %v", err)
	}
	keyOut.Close()

	return certPath, keyPath
}

// TestHTTPServerTLS verifies that when TLS is configured, the server serves
// over HTTPS and a plain HTTP request fails.
func TestHTTPServerTLS(t *testing.T) {
	certPath, keyPath := generateSelfSignedCert(t)

	srv := NewHTTPServer("tls-test", "127.0.0.1:0", nil)
	srv.SetTLS(certPath, keyPath)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	addr := srv.Addr()
	if addr == "" {
		t.Fatal("expected non-empty Addr after Start")
	}

	baseURL := "https://" + addr

	// Create an HTTP client that trusts the self-signed certificate.
	cert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert: %v", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(cert)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caCertPool,
			},
		},
		Timeout: 5 * time.Second,
	}

	// Poll until the server accepts connections.
	ready := false
	for i := 0; i < 50; i++ {
		resp, err := client.Get(baseURL + "/stream?sender_id=test")
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("TLS server did not become ready")
	}

	// Verify that a plain HTTP request fails (confirming TLS is active).
	// Go's TLS server detects plain HTTP and returns a 400 with an error
	// message, so we check the status code rather than expecting a transport
	// error.
	httpClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := httpClient.Get("http://" + addr + "/stream?sender_id=test")
	if err == nil {
		// Some Go versions return a 400 response instead of a transport
		// error. Either way, a non-200 status confirms TLS is active.
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			t.Error("expected plain HTTP request to fail or return non-200 when TLS is enabled")
		}
		resp.Body.Close()
	}

	// Verify we can connect and send messages over HTTPS.
	connectBody, _ := json.Marshal(ACPMessage{
		Type:     TypeConnect,
		SenderID: "tls-client",
	})
	resp, err = client.Post(baseURL+"/connect", "application/json", strings.NewReader(string(connectBody)))
	if err != nil {
		t.Fatalf("HTTPS connect failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "connected") {
		t.Errorf("expected 'connected' in response, got %s", body)
	}
}

// TestHTTPServerNoTLSFallback verifies that when TLS is not configured, the
// server serves over plain HTTP.
func TestHTTPServerNoTLSFallback(t *testing.T) {
	srv := NewHTTPServer("no-tls-test", "127.0.0.1:0", nil)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	baseURL := "http://" + srv.Addr()

	// Poll until ready.
	ready := false
	for i := 0; i < 50; i++ {
		resp, err := http.Get(baseURL + "/stream?sender_id=test")
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("HTTP server did not become ready")
	}

	// Plain HTTP should work.
	resp, err := http.Get(baseURL + "/stream?sender_id=test")
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// TestIsLoopbackAddr verifies that various address formats are correctly
// classified as loopback or non-loopback.
func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:9090", true},
		{"localhost:9090", true},
		{"[::1]:9090", true},
		{":9090", true}, // empty host = all interfaces, treated as loopback for safety
		{"0.0.0.0:9090", false},
		{"192.168.1.100:9090", false},
		{"10.0.0.1:9090", false},
		{"example.com:9090", false},
		// No port — host-only.
		{"127.0.0.1", true},
		{"localhost", true},
		{"0.0.0.0", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := IsLoopbackAddr(tt.addr)
			if got != tt.loopback {
				t.Errorf("IsLoopbackAddr(%q) = %v, want %v", tt.addr, got, tt.loopback)
			}
		})
	}
}

// TestSetTLS verifies that SetTLS stores the cert and key file paths.
func TestSetTLS(t *testing.T) {
	srv := NewHTTPServer("test", "127.0.0.1:0", nil)
	srv.SetTLS("cert.pem", "key.pem")
	if srv.tlsCertFile != "cert.pem" {
		t.Errorf("expected tlsCertFile 'cert.pem', got %q", srv.tlsCertFile)
	}
	if srv.tlsKeyFile != "key.pem" {
		t.Errorf("expected tlsKeyFile 'key.pem', got %q", srv.tlsKeyFile)
	}
}
