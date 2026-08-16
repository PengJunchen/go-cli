package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeCmdNonLoopbackNoAuthRejected verifies that binding to a
// non-loopback address with --no-auth is rejected (SEC-17).
func TestServeCmdNonLoopbackNoAuthRejected(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	err := c.Run(context.Background(), NewDefaultConfig(false), []string{
		"--no-auth",
		"--addr", "0.0.0.0:9090",
	})

	if err == nil {
		t.Fatal("expected error when --no-auth is used with non-loopback address")
	}

	ue, ok := err.(*UsageError)
	if !ok {
		t.Fatalf("expected UsageError, got %T: %v", err, err)
	}

	if !strings.Contains(ue.Error(), "non-loopback") {
		t.Errorf("error should mention non-loopback, got: %v", err)
	}
	if !strings.Contains(ue.Error(), "0.0.0.0:9090") {
		t.Errorf("error should mention the address, got: %v", err)
	}
}

// TestServeCmdNonLoopbackWarning verifies that binding to a non-loopback
// address without TLS (but with auth) prints a warning to stderr.
func TestServeCmdNonLoopbackWarning(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	tokenPath := filepath.Join(t.TempDir(), "serve-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Auth is enabled (default); no TLS. Should print warning.
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "0.0.0.0:0",
			"--token-file", tokenPath,
		})
	}()

	// Wait for the server to start.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "listening") {
			break
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server failed to start: %v", err)
			}
			break
		case <-time.After(50 * time.Millisecond):
		}
	}

	if !strings.Contains(out.String(), "listening") {
		t.Fatal("server did not start within timeout")
	}

	stderr := errOut.String()
	if !strings.Contains(stderr, "WARNING") {
		t.Errorf("expected 'WARNING' in stderr for non-loopback without TLS, got: %s", stderr)
	}
	if !strings.Contains(stderr, "TLS") {
		t.Errorf("expected 'TLS' mention in stderr warning, got: %s", stderr)
	}

	cancel()
	<-done
}

// TestServeCmdNonLoopbackWithTLSNoWarning verifies that binding to a
// non-loopback address with TLS does NOT print the unencrypted warning.
func TestServeCmdNonLoopbackWithTLSNoWarning(t *testing.T) {
	certPath, keyPath := generateRealSelfSignedCert(t)

	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	tokenPath := filepath.Join(t.TempDir(), "serve-token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--addr", "0.0.0.0:0",
			"--token-file", tokenPath,
			"--tls-cert", certPath,
			"--tls-key", keyPath,
		})
	}()

	// Wait for the server to start.
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "listening") {
			started = true
			break
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server failed to start: %v", err)
			}
			break
		case <-time.After(50 * time.Millisecond):
		}
	}

	if !started {
		t.Fatal("server did not start within timeout")
	}

	// Should NOT have the unencrypted warning.
	stderr := errOut.String()
	if strings.Contains(stderr, "unencrypted") {
		t.Errorf("expected no 'unencrypted' warning when TLS is configured, got: %s", stderr)
	}

	cancel()
	<-done
}

// TestServeCmdTLSMissingKey verifies that providing --tls-cert without
// --tls-key (or vice versa) returns an error.
func TestServeCmdTLSMissingKey(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	// Only cert, no key.
	err := c.Run(context.Background(), NewDefaultConfig(false), []string{
		"--addr", "127.0.0.1:0",
		"--tls-cert", "cert.pem",
	})
	if err == nil {
		t.Fatal("expected error when --tls-cert is provided without --tls-key")
	}
	ue, ok := err.(*UsageError)
	if !ok {
		t.Fatalf("expected UsageError, got %T: %v", err, err)
	}
	if !strings.Contains(ue.Error(), "tls-key") {
		t.Errorf("error should mention --tls-key, got: %v", err)
	}

	// Only key, no cert.
	err = c.Run(context.Background(), NewDefaultConfig(false), []string{
		"--addr", "127.0.0.1:0",
		"--tls-key", "key.pem",
	})
	if err == nil {
		t.Fatal("expected error when --tls-key is provided without --tls-cert")
	}
}

// TestServeCmdTLSFileNotReadable verifies that a non-existent TLS cert file
// produces an execution error.
func TestServeCmdTLSFileNotReadable(t *testing.T) {
	out := &safeBuffer{}
	c := newServeCmd(out)

	missingPath := filepath.Join(t.TempDir(), "nonexistent.pem")
	err := c.Run(context.Background(), NewDefaultConfig(false), []string{
		"--addr", "127.0.0.1:0",
		"--tls-cert", missingPath,
		"--tls-key", missingPath,
	})
	if err == nil {
		t.Fatal("expected error for non-existent TLS files")
	}
	ee, ok := err.(*ExecutionError)
	if !ok {
		t.Fatalf("expected ExecutionError, got %T: %v", err, err)
	}
	if !strings.Contains(ee.Error(), "TLS") {
		t.Errorf("error should mention TLS, got: %v", err)
	}
}

// TestServeCmdLoopbackNoWarning verifies that a loopback address does NOT
// produce the non-loopback TLS warning.
func TestServeCmdLoopbackNoWarning(t *testing.T) {
	out := &safeBuffer{}
	errOut := &safeBuffer{}
	c := newServeCmd(out)
	c.errOut = errOut

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, NewDefaultConfig(false), []string{
			"--no-auth",
			"--addr", "127.0.0.1:0",
		})
	}()

	// Wait for the server to start.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "listening") {
			break
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server failed to start: %v", err)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}

	if !strings.Contains(out.String(), "listening") {
		t.Fatal("server did not start within timeout")
	}

	// Should NOT have the non-loopback warning.
	if strings.Contains(errOut.String(), "non-loopback") {
		t.Errorf("expected no non-loopback warning for loopback address, got: %s", errOut.String())
	}

	cancel()
	<-done
}

// generateRealSelfSignedCert creates a real self-signed TLS certificate and
// key pair for testing, returning their file paths.
func generateRealSelfSignedCert(t *testing.T) (certPath, keyPath string) {
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
