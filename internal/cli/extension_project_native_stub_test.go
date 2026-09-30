//go:build no_plugin

package cli

import (
	"os"
	"testing"
)

func TestProjectExtensionNativeTrustRegrantUsesStableModulePath(t *testing.T) {
	const reason = "native Go plugin loading is disabled by the no_plugin build tag"
	if os.Getenv("GOCLI_REQUIRE_NATIVE_PLUGIN_TEST") == "1" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}
