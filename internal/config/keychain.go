package config

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// KeychainSource stores and retrieves the API key from the OS keychain. On
// macOS it shells out to the built-in `security` command; on every other
// platform it reports itself as unavailable and all operations are no-ops.
type KeychainSource struct {
	service string
	account string
	// run executes a command and returns its stdout. It is a field so tests
	// can inject a fake without touching the real keychain.
	run func(name string, args ...string) ([]byte, error)
}

// NewKeychainSource creates a KeychainSource with the default service
// ("go-cli") and account ("api-key") names.
func NewKeychainSource() *KeychainSource {
	return &KeychainSource{
		service: "go-cli",
		account: "api-key",
		run:     keychainExec,
	}
}

// keychainExec is the default command runner used by KeychainSource.
func keychainExec(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Available reports whether the OS keychain is usable. On non-macOS systems
// it always returns false.
func (k *KeychainSource) Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("security")
	return err == nil
}

// Set stores the API key in the keychain, creating or updating the entry.
// Using -U ensures an existing entry is updated rather than producing a
// duplicate-item error.
func (k *KeychainSource) Set(key string) error {
	if !k.Available() {
		return errors.New("keychain: not available on this platform")
	}
	if _, err := k.run("security", "add-generic-password",
		"-s", k.service, "-a", k.account, "-w", key, "-U"); err != nil {
		return fmt.Errorf("keychain: set: %w", err)
	}
	return nil
}

// Get retrieves the API key from the keychain. A missing entry is reported as
// ErrAuthNotFound so the auth chain can transparently fall through to the
// next source.
func (k *KeychainSource) Get() (string, error) {
	if !k.Available() {
		return "", ErrAuthNotFound
	}
	out, err := k.run("security", "find-generic-password",
		"-s", k.service, "-a", k.account, "-w")
	if err != nil {
		return "", ErrAuthNotFound
	}
	return strings.TrimSpace(string(out)), nil
}

// Delete removes the API key entry from the keychain.
func (k *KeychainSource) Delete() error {
	if !k.Available() {
		return errors.New("keychain: not available on this platform")
	}
	if _, err := k.run("security", "delete-generic-password",
		"-s", k.service, "-a", k.account); err != nil {
		return fmt.Errorf("keychain: delete: %w", err)
	}
	return nil
}

// Lookup adapts KeychainSource to the AuthSource.Lookup signature so it can
// be inserted into an AuthChain. The provider argument is ignored because the
// keychain stores a single API key shared across providers.
func (k *KeychainSource) Lookup(_ string) (string, error) {
	return k.Get()
}
