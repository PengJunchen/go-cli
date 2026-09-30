package cli

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
)

const remoteExtensionAssemblyCanary = "assembly-remote-url-canary-710c"

func runAssemblyWithRemoteExtensionPath(t *testing.T) (requests int32, logs string, assemblyReady bool, err error) {
	t.Helper()
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requestCount.Add(1) }))
	defer srv.Close()

	var logOutput strings.Builder
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	rc := newAssembleTestConfig(t)
	enabled := true
	rc.Extensions = config.ExtensionsConfig{
		Enabled:     &enabled,
		PluginPaths: []string{srv.URL + "/extension?token=" + remoteExtensionAssemblyCanary},
	}
	assembly, err := AssembleAgent(context.Background(), rc, "openai", "test-model", io.Discard)
	if assembly != nil {
		assembly.Cleanup()
	}
	return requestCount.Load(), logOutput.String(), assembly != nil, err
}
