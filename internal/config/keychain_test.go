package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipNonDarwin skips the test on non-macOS platforms where the keychain is
// unavailable.
func skipNonDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("keychain tests require macOS")
	}
}

// newFakeKeychain returns a KeychainSource whose run function dispatches to the
// provided handler. This avoids touching the real OS keychain during tests.
func newFakeKeychain(run func(name string, args ...string) ([]byte, error)) *KeychainSource {
	return &KeychainSource{
		service:       "go-cli",
		account:       "api-key",
		availableFunc: func() bool { return true },
		run:           run,
	}
}

func TestKeychainSource_Available(t *testing.T) {
	skipNonDarwin(t)
	kc := NewKeychainSource()
	assert.True(t, kc.Available(), "security command should be available on macOS")
}

func TestKeychainSource_Set(t *testing.T) {
	skipNonDarwin(t)
	var gotName string
	var gotArgs []string
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = args
		return nil, nil
	})

	require.NoError(t, kc.Set("sk-secret"))

	assert.Equal(t, "security", gotName)
	assert.Contains(t, gotArgs, "add-generic-password")
	assert.Contains(t, gotArgs, "-s")
	assert.Contains(t, gotArgs, "go-cli")
	assert.Contains(t, gotArgs, "-a")
	assert.Contains(t, gotArgs, "api-key")
	assert.Contains(t, gotArgs, "-w")
	assert.Contains(t, gotArgs, "sk-secret")
	assert.Contains(t, gotArgs, "-U")
}

func TestKeychainSource_Get(t *testing.T) {
	skipNonDarwin(t)
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		assert.Equal(t, "security", name)
		assert.Contains(t, args, "find-generic-password")
		return []byte("sk-retrieved"), nil
	})

	got, err := kc.Get()
	require.NoError(t, err)
	assert.Equal(t, "sk-retrieved", got)
}

func TestKeychainSource_GetTrimsWhitespace(t *testing.T) {
	skipNonDarwin(t)
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		return []byte("sk-retrieved\n"), nil
	})

	got, err := kc.Get()
	require.NoError(t, err)
	assert.Equal(t, "sk-retrieved", got)
}

func TestKeychainSource_GetNotFound(t *testing.T) {
	skipNonDarwin(t)
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("The specified item could not be found in the keychain")
	})

	_, err := kc.Get()
	assert.ErrorIs(t, err, ErrAuthNotFound)
}

func TestKeychainSource_Delete(t *testing.T) {
	skipNonDarwin(t)
	var gotArgs []string
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	})

	require.NoError(t, kc.Delete())
	assert.Contains(t, gotArgs, "delete-generic-password")
	assert.Contains(t, gotArgs, "go-cli")
	assert.Contains(t, gotArgs, "api-key")
}

func TestKeychainSource_DeleteError(t *testing.T) {
	skipNonDarwin(t)
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("delete failed")
	})

	err := kc.Delete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keychain: delete")
}

func TestKeychainSource_Lookup(t *testing.T) {
	skipNonDarwin(t)
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		return []byte("sk-via-lookup"), nil
	})

	// The provider argument is ignored — the keychain stores a single key.
	got, err := kc.Lookup("anthropic")
	require.NoError(t, err)
	assert.Equal(t, "sk-via-lookup", got)
}

func TestKeychainSource_InAuthChain(t *testing.T) {
	skipNonDarwin(t)
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		return []byte("sk-chain-key"), nil
	})

	c := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceKeychain, Lookup: kc.Lookup},
	})
	_, found, err := c.ResolveCredential(context.Background(), "openai", "openai-compatible", "https://api.openai.com")
	require.NoError(t, err)
	assert.False(t, found, "legacy provider-only keychain entry must not enter a scoped route")
}

func TestKeychainSource_ScopedCredentialInAuthChain(t *testing.T) {
	skipNonDarwin(t)
	scope := AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.openai.com"}
	var account string
	kc := newFakeKeychain(func(_ string, args ...string) ([]byte, error) {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-a" {
				account = args[i+1]
				break
			}
		}
		return []byte("scoped-key"), nil
	})
	c := NewAuthChain().WithSources([]AuthSource{{Name: authSourceKeychain, LookupScoped: kc.LookupScoped}})

	got, found, err := c.ResolveCredential(context.Background(), scope.ProviderID, scope.ProtocolID, scope.Origin)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "scoped-key", got)
	assert.NotEqual(t, "api-key", account, "scoped lookup must not read the legacy unscoped keychain item")
}

func TestKeychainSource_GetScopedAccessFailureIsNotNotFound(t *testing.T) {
	secret := authSecretCanary(t)
	kc := newFakeKeychain(func(name string, args ...string) ([]byte, error) {
		assert.Equal(t, "security", name)
		assert.Contains(t, args, "find-generic-password")
		return nil, errors.New("keychain access denied")
	})

	got, err := kc.GetScoped(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAuthNotFound), "access failures must stop the auth chain")
	assert.Empty(t, got)
	assert.NotContains(t, err.Error(), secret)
}

func TestKeychainSource_GetScopedInvalidScopeIsSourceError(t *testing.T) {
	kc := newFakeKeychain(func(string, ...string) ([]byte, error) {
		t.Fatal("invalid scope must be rejected before invoking security")
		return nil, nil
	})

	got, err := kc.GetScoped(AuthScope{})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAuthNotFound))
	assert.Empty(t, got)
}

func TestKeychainSource_GetScopedInvalidScopeIsErrorWhenBackendUnavailable(t *testing.T) {
	kc := newFakeKeychain(func(string, ...string) ([]byte, error) {
		t.Fatal("invalid scope must be rejected without invoking security")
		return nil, nil
	})
	kc.availableFunc = func() bool { return false }

	key, err := kc.GetScoped(AuthScope{})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAuthNotFound))
	assert.Empty(t, key)
}

func TestKeychainSource_GetScopedFailureStopsAuthChainFallback(t *testing.T) {
	fallbackCalls := 0
	fallbackKey := authSecretCanary(t)
	kc := newFakeKeychain(func(string, ...string) ([]byte, error) {
		return nil, errors.New("keychain access denied")
	})
	chain := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceKeychain, LookupScoped: kc.LookupScoped},
		{Name: authSourceAuthFile, LookupScoped: func(AuthScope) (string, error) {
			fallbackCalls++
			return fallbackKey, nil
		}},
	})

	key, found, err := chain.ResolveCredential(context.Background(), "openai", "openai-compatible", "https://api.example.test")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAuthNotFound))
	assert.Empty(t, key)
	assert.False(t, found)
	assert.Zero(t, fallbackCalls, "a keychain source failure must stop before lower-priority sources")
}

func TestKeychainSource_GetScopedConfirmedMissingItemFallsThrough(t *testing.T) {
	fallbackKey := authSecretCanary(t)
	kc := newFakeKeychain(func(string, ...string) ([]byte, error) {
		return nil, errKeychainItemNotFound
	})
	fallbackCalls := 0
	chain := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceKeychain, LookupScoped: kc.LookupScoped},
		{Name: authSourceAuthFile, LookupScoped: func(AuthScope) (string, error) {
			fallbackCalls++
			return fallbackKey, nil
		}},
	})

	key, found, err := chain.ResolveCredential(context.Background(), "openai", "openai-compatible", "https://api.example.test")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, fallbackKey, key)
	assert.Equal(t, 1, fallbackCalls)
}

func TestKeychainSource_GetScopedUnavailableBackendFallsThrough(t *testing.T) {
	kc := newFakeKeychain(func(string, ...string) ([]byte, error) {
		t.Fatal("unavailable keychain must be skipped")
		return nil, nil
	})
	kc.availableFunc = func() bool { return false }

	fallbackCalls := 0
	chain := NewAuthChain().WithSources([]AuthSource{
		{Name: authSourceKeychain, LookupScoped: kc.LookupScoped},
		{Name: authSourceAuthFile, LookupScoped: func(AuthScope) (string, error) {
			fallbackCalls++
			return "lower-priority-key", nil
		}},
	})

	key, found, err := chain.ResolveCredential(context.Background(), "openai", "openai-compatible", "https://api.example.test")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "lower-priority-key", key)
	assert.Equal(t, 1, fallbackCalls)
}

func TestKeychainExecClassifiesOnlyConfirmedMissingItem(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the fake security executable uses a POSIX shell")
	}
	binDir := t.TempDir()
	shim := filepath.Join(binDir, "security")
	script := "#!/bin/sh\n" +
		"case \"$FAKE_SECURITY_MODE\" in\n" +
		"  missing) printf '%s\\n' 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.' >&2; exit 44 ;;\n" +
		"  denied) printf 'access denied canary=%s\\n' \"$FAKE_SECRET\" >&2; exit 36 ;;\n" +
		"  empty) exit 0 ;;\n" +
		"esac\n"
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o700))
	t.Setenv("PATH", binDir)

	t.Run("confirmed missing", func(t *testing.T) {
		t.Setenv("FAKE_SECURITY_MODE", "missing")
		_, err := keychainExec("security", "find-generic-password", "-s", "go-cli", "-a", "exact-scope", "-w")
		require.ErrorIs(t, err, errKeychainItemNotFound)
	})
	t.Run("access failure", func(t *testing.T) {
		t.Setenv("FAKE_SECURITY_MODE", "denied")
		secret := authSecretCanary(t)
		t.Setenv("FAKE_SECRET", secret)
		_, err := keychainExec("security", "find-generic-password", "-s", "go-cli", "-a", "exact-scope", "-w")
		require.Error(t, err)
		assert.False(t, errors.Is(err, errKeychainItemNotFound))
		assert.NotContains(t, err.Error(), secret)
	})
	t.Run("empty success output", func(t *testing.T) {
		t.Setenv("FAKE_SECURITY_MODE", "empty")
		kc := NewKeychainSource()
		kc.availableFunc = func() bool { return true }
		key, err := kc.GetScoped(AuthScope{ProviderID: "openai", ProtocolID: "openai-compatible", Origin: "https://api.example.test"})
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrAuthNotFound))
		assert.Empty(t, key)
	})
}

func TestNewAuthChain_IncludesKeychainSource(t *testing.T) {
	c := NewAuthChain()
	c.mu.RLock()
	defer c.mu.RUnlock()

	var names []string
	for _, s := range c.sources {
		names = append(names, s.Name)
	}

	assert.Contains(t, names, authSourceKeychain)
	// Priority: CLI flag -> keychain -> auth.json -> env -> models.json.
	require.Len(t, names, 5)
	assert.Equal(t, authSourceCLI, names[0])
	assert.Equal(t, authSourceKeychain, names[1])
	assert.Equal(t, authSourceAuthFile, names[2])
}

// --- lookupAuthFile tests (api_key single-key format) ---

func TestLookupAuthFile_APIKeyFormat(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	authDir := filepath.Join(tmpDir, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(authDir, 0o750))
	require.NoError(t, os.Chmod(authDir, 0o700))
	authPath := filepath.Join(authDir, "auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte(`{"api_key":"sk-from-file"}`), 0o600))

	got, err := lookupAuthFile("openai")
	require.NoError(t, err)
	assert.Equal(t, "sk-from-file", got)
}

func TestLookupAuthFile_ProviderMapFormat(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	authDir := filepath.Join(tmpDir, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(authDir, 0o750))
	require.NoError(t, os.Chmod(authDir, 0o700))
	authPath := filepath.Join(authDir, "auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte(`{"openai":"sk-map-key"}`), 0o600))

	got, err := lookupAuthFile("openai")
	require.NoError(t, err)
	assert.Equal(t, "sk-map-key", got)
}

func TestLookupAuthFile_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, err := lookupAuthFile("openai")
	assert.ErrorIs(t, err, ErrAuthNotFound)
}

func TestLookupAuthFile_APIKeyTakesPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	authDir := filepath.Join(tmpDir, ".config", "go-cli")
	require.NoError(t, os.MkdirAll(authDir, 0o750))
	require.NoError(t, os.Chmod(authDir, 0o700))
	authPath := filepath.Join(authDir, "auth.json")
	// When both api_key and a provider entry exist, api_key wins.
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"api_key":"sk-primary","openai":"sk-secondary"}`), 0o600))

	got, err := lookupAuthFile("openai")
	require.NoError(t, err)
	assert.Equal(t, "sk-primary", got)
}
