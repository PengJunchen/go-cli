package production

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestDefaultChainIncludesPromptInjectionGuard verifies that the default output
// guard chain includes PromptInjectionGuard so prompt-injection protection is
// active out of the box.
func TestDefaultChainIncludesPromptInjectionGuard(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := defaultOutputGuardChain()
	c, ok := chain.(*OutputGuardChain)
	require.True(t, ok, "default guard chain must be an OutputGuardChain")

	var found bool
	for _, g := range c.Guards() {
		if g.Name() == "prompt-injection-guard" {
			found = true
			break
		}
	}
	assert.True(t, found, "default chain must include prompt-injection-guard")
}
