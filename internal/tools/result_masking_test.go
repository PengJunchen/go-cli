package tools //nolint:scan003 // test file contains intentional secret patterns for masking tests

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestResultMaskerDefaultPatterns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker(nil)

	// API key
	out := m.Mask("key is sk-abcdefghijklmnopqrstuvwxyz1234 end")
	assert.Contains(t, out, "[REDACTED]")
	assert.NotContains(t, out, "sk-abcdefghijklmnopqrstuvwxyz1234")

	// GitHub token
	out = m.Mask("token ghp_0123456789012345678901234567890123456 done")
	assert.Contains(t, out, "[REDACTED]")
	assert.NotContains(t, out, "ghp_0123456789012345678901234567890123456")

	// Password field in JSON
	out = m.Mask(`{"password": "s3cr3t", "name": "bob"}`)
	assert.Contains(t, out, "[REDACTED]")
	assert.NotContains(t, out, "s3cr3t")
	// Key and surrounding structure preserved.
	assert.Contains(t, out, `"password"`)
	assert.Contains(t, out, `"name": "bob"`)

	// Credit card number
	out = m.Mask("card 4111 1111 1111 1111 here")
	assert.Contains(t, out, "[REDACTED]")
	assert.NotContains(t, out, "4111 1111 1111 1111")
}

func TestResultMaskerNoMatch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker(nil)

	in := "nothing sensitive here"
	out := m.Mask(in)
	assert.Equal(t, in, out)
}

func TestResultMaskerCustomPatterns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker([]string{`(foo+)`})

	out := m.Mask("foo bar fooooo")
	assert.Equal(t, "[REDACTED] bar [REDACTED]", out)
}

func TestResultMaskerEmptyContent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker(nil)
	assert.Equal(t, "", m.Mask(""))
}

func TestResultMaskerCustomMask(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker([]string{`(sk-[a-zA-Z0-9]{20,})`})
	m.SetMask("***")

	out := m.Mask("key sk-abcdefghijklmnopqrstuvwxyz1234")
	assert.Contains(t, out, "***")
	assert.NotContains(t, out, "sk-abcdefghijklmnopqrstuvwxyz1234")
}

func TestResultMaskerSetMaskEmptyFallsBack(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker([]string{`(sk-[a-zA-Z0-9]{20,})`})
	m.SetMask("")

	out := m.Mask("sk-abcdefghijklmnopqrstuvwxyz1234")
	assert.Contains(t, out, "[REDACTED]")
}

func TestResultMaskerInvalidPatternSkipped(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker([]string{
		`[invalid(`,             // invalid regex - skipped
		`(sk-[a-zA-Z0-9]{20,})`, // valid
	})

	out := m.Mask("sk-abcdefghijklmnopqrstuvwxyz1234")
	assert.Contains(t, out, "[REDACTED]")
}

func TestResultMaskerConcurrentMask(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker(nil)

	var wg sync.WaitGroup
	const goroutines = 8
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := m.Mask("sk-abcdefghijklmnopqrstuvwxyz1234")
			assert.Contains(t, out, "[REDACTED]")
		}()
	}
	wg.Wait()
}

func TestResultMaskerPasswordFieldPreservesStructure(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker(nil)

	in := `{"username":"admin","password":"hunter2"}`
	out := m.Mask(in)
	assert.Contains(t, out, `"username":"admin"`)
	assert.Contains(t, out, `[REDACTED]`)
	assert.NotContains(t, out, "hunter2")
	// The JSON key and quotes should remain.
	require.Contains(t, out, `"password"`)
}

func TestResultMaskerCreditCardVariations(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := NewResultMasker(nil)

	cases := []string{
		"4111111111111111",
		"4111-1111-1111-1111",
		"4111 1111 1111 1111",
	}
	for _, cc := range cases {
		out := m.Mask(cc)
		assert.Contains(t, out, "[REDACTED]", "cc: %s", cc)
		assert.NotContains(t, out, "4111", "cc: %s", cc)
	}
}
