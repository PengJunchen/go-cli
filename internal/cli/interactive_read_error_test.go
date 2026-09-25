package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractiveRunReturnsTTYInputSetupErrorAfterCleanup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	cfg := newE2ETestConfig(t, server.URL)
	var out bytes.Buffer
	cmd := newInteractiveCmd(strings.NewReader(""), &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{err: errTTYReaderCancellationUnsupported}}}

	err := cmd.Run(context.Background(), cfg, []string{"--no-onboarding"})
	require.ErrorIs(t, err, errTTYReaderCancellationUnsupported)
	assert.Contains(t, out.String(), "Interactive session started")
	assert.Contains(t, out.String(), "Session ended")
}
