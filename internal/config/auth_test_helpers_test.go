package config

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func authSecretCanary(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate auth test canary: %v", err)
	}
	return "auth-test-" + hex.EncodeToString(value[:])
}
