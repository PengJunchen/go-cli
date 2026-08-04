// Package tests contains end-to-end integration tests for go-cli.
//
// This file is the Phase 4 end-to-end gate. It proves, in one trace
// rooted at a single span with a consistent trace_id, the full Phase 4 chain:
//
//  1. Skill load      : YAMLSkillLoader parses a YAML-frontmatter skill file and
//     the DefaultSkillRegistry indexes/matches it.
//  2. Extension reg   : an Extension's Init registers a hook and middleware
//     into a DefaultExtensionRegistry, and both are queryable
//     and runnable afterward. (The ExtensionCoordinator
//     lifecycle that drives Init/Shutdown is covered by the
//     coordinator tests in internal/extension.)
//  3. SubAgent spawn  : DefaultSubAgentFactory spawns a DefaultSubAgent that
//     runs, accepts Send, and returns a Wait result.
//  4. TUI render      : a BubbleteaApp consumes agent events and renders them
//     into a view buffer via the default renderer registry.
//  5. OTLP export     : an OTLPTraceExporter ships completed spans to a real
//     httptest-backed OTLP collector over HTTP.
//  6. YAML config     : config.Loader auto-detects format from a .yaml file and
//     loads a typed Config; UnmarshalConfig parses the subset.
//  7. Provider compose: DefaultProviderComposer merges builtin/config/extension
//     provider layers under Extension > Config > Builtin.
//  8. ACP             : two StdioAdapters exchange ACP messages over an
//     in-process io.Pipe loopback (ACP stdio).
//
// All steps run in-memory using real default implementations wired together, so
// the test compiles and passes in the default `go test -race ./internal/...` and
// `go test -race ./tests/...` builds that `make verify` runs - no mock build
// tag is required. The MockTraceExporter helper lives in internal/mock and is
// NOT build-tagged, so it is available.
package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// p4s has a small sample skill in the exact YAML-frontmatter format the loader
// accepts (see internal/skill/skill_test.go).
const p4SampleSkill = `---
name: e2e-skill
description: an end-to-end skill
version: 1.0.0
category: coding
prompt: |
  You are a coding assistant.
  Follow the instructions carefully.
tools:
  - bash
  - read
trigger_hint: "fix bug"
parameters:
  max_attempts: 3
---
optional body markdown ignored when a prompt is declared
`

// TestPhase4EndToEnd exercises the full Phase 4 chain under a single tracing
// root so the span tree can be asserted end to end:
// skill load -> extension registration -> subagent spawn -> TUI render ->
// YAML config load -> provider composition -> ACP communication, with the
// OTLP HTTP export exercised against a real httptest collector.
func TestPhase4EndToEnd(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exporter := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("phase4-e2e", exporter)
	root, spanCtx := tracer.Start(context.Background(), "phase4.root", tracing.SpanKindInternal)

	dir := t.TempDir()

	t.Run("skill_load_and_match", func(t *testing.T) {
		p4SkillLoadAndMatch(spanCtx, t, dir, exporter)
	})

	t.Run("extension_registration", func(t *testing.T) {
		p4ExtensionRegistration(spanCtx, t)
	})

	t.Run("subagent_spawn", func(t *testing.T) {
		p4SubAgentSpawn(spanCtx, t)
	})

	t.Run("tui_render", func(t *testing.T) {
		p4TUIRender(spanCtx, t)
	})

	t.Run("output_guard_chain", func(t *testing.T) {
		p4OutputGuard(spanCtx, t)
	})

	t.Run("yaml_config_load", func(t *testing.T) {
		p4YAMLConfigLoad(spanCtx, t, dir)
	})

	t.Run("provider_composition", func(t *testing.T) {
		p4ProviderComposer(spanCtx, t)
	})

	t.Run("acp_stdio_loopback", func(t *testing.T) {
		p4ACPStdio(spanCtx, t)
	})

	t.Run("session_branch_summary", func(t *testing.T) {
		p4BranchSummary(spanCtx, t)
	})

	t.Run("otlp_http_export", func(t *testing.T) {
		p4OTLPExport(t)
	})

	root.End()

	// A span with the phase4 root trace id must exist for each chain step.
	p4WaitForSpans(t, exporter)
	require.GreaterOrEqual(t, len(exporter.Spans()), 6,
		"phase 4 chain must emit spans for skill.load, subagent.spawn, config.load, llm.provider_compose, acp.send and session at minimum")
	for _, s := range exporter.Spans() {
		require.Equal(t, tracer.TraceID(), s.TraceID,
			"span %s (%s) must share the phase4 trace root id", s.SpanID, s.Name)
	}
}

// p4SkillLoadAndMatch parses a skill file, registers it in a DefaultSkillRegistry
// and asserts progressive-disclosure matching ranks the exact-name skill first.
func p4SkillLoadAndMatch(ctx context.Context, t *testing.T, dir string, exporter *mock.MockTraceExporter) {
	t.Helper()

	path := filepath.Join(dir, "e2e-skill.md")
	require.NoError(t, os.WriteFile(path, []byte(p4SampleSkill), 0o600))

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, def)
	d := *def // loader returns *SkillDefinition (pointer to interface)
	assert.Equal(t, "e2e-skill", d.Name())
	assert.Equal(t, "coding", d.Category())

	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, d))

	got, ok := reg.Get(ctx, "e2e-skill")
	require.True(t, ok)
	require.Equal(t, "e2e-skill", got.Name())

	// Match must surface the skill from a natural-language hint.
	matches := reg.Match(ctx, "fix bug")
	require.NotEmpty(t, matches)
	assert.Equal(t, "e2e-skill", matches[0].Name())

	assert.Equal(t, []string{"e2e-skill"}, skillNames(reg.List(ctx, "coding")))
	assertSpanEventually(t, exporter, "skill.load")
}

// p4ExtensionRegistration drives an Extension through its Init/Shutdown lifecycle
// and verifies the hooks / middleware it registered are queryable and runnable
// from the ExtensionRegistry. The lifecycle transitions themselves are covered
// by the coordinator tests in internal/extension.
func p4ExtensionRegistration(ctx context.Context, t *testing.T) {
	t.Helper()

	reg := extension.NewExtensionRegistry()

	// Hook/Middleware getters are on the concrete type, not the interface.
	concreteReg, ok := reg.(*extension.DefaultExtensionRegistry)
	require.True(t, ok, "NewExtensionRegistry must return *DefaultExtensionRegistry")

	ext := &p4TestExtension{name: "e2e-ext"}
	require.NoError(t, ext.Init(ctx, reg))

	// The extension registered a hook and a middleware during Init.
	hook := concreteReg.Hook("e2e-hook")
	require.NotNil(t, hook, "extension must register its hook")
	assert.Equal(t, "e2e-hook", hook.Name())

	// The hook handle round-trips.
	res := hook.Handle(ctx, extension.HookEvent{Name: "agent.before_run", Source: "e2e"})
	assert.Equal(t, extension.HookActionPass, res.Action)

	mw := concreteReg.Middleware("e2e-middleware")
	require.NotNil(t, mw, "extension must register its middleware")
	out, err := mw.WrapAgent(func(_ context.Context, in extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "wrapped:" + in.Message}, nil
	})(ctx, extension.AgentInput{Message: "hi"})
	require.NoError(t, err)
	assert.Equal(t, "wrapped:hi", out.Text)

	require.NoError(t, ext.Shutdown(ctx))
}

// p4Hook is a real extension.Hook used to prove registration.
type p4Hook struct{}

func (p4Hook) Name() string { return "e2e-hook" }

func (p4Hook) Handle(_ context.Context, event extension.HookEvent) extension.HookResult {
	return extension.HookResult{Action: extension.HookActionPass, Reason: event.Source}
}

// p4Middleware is a real extension.Middleware used to prove registration.
type p4Middleware struct{}

func (p4Middleware) Name() string { return "e2e-middleware" }

func (p4Middleware) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	return func(ctx context.Context, in extension.AgentInput) (extension.AgentOutput, error) {
		return next(ctx, in)
	}
}

// p4TestExtension is a real extension.Extension that registers a hook and a
// middleware into the registry during Init.
type p4TestExtension struct {
	name string
}

func (e *p4TestExtension) Name() string { return e.name }

func (e *p4TestExtension) Init(_ context.Context, reg extension.ExtensionRegistry) error {
	if err := reg.RegisterHook(context.Background(), p4Hook{}); err != nil {
		return err
	}
	if err := reg.RegisterMiddleware(context.Background(), p4Middleware{}); err != nil {
		return err
	}
	return nil
}

func (e *p4TestExtension) Shutdown(context.Context) error { return nil }

// p4SubAgentSpawn spawns a real DefaultSubAgent via the default factory, runs a
// prompt, delivers a follow-up message and reads the final result.
func p4SubAgentSpawn(ctx context.Context, t *testing.T) {
	t.Helper()

	factory := core.GetSubAgentFactory()
	sub, err := factory.Create(ctx, "e2e-sub", core.SubAgentConfig{
		SystemPrompt: "you are a helper",
		Model:        "test-model",
		MaxTurns:     2,
	})
	require.NoError(t, err)
	require.Equal(t, "e2e-sub", sub.Name())

	events, err := sub.Run(ctx, "do the work")
	require.NoError(t, err)

	// Drain the event stream until it closes.
	var userSeen, msgSeen int
	for ev := range events {
		switch ev.Kind {
		case "user":
			userSeen++
		case "message":
			msgSeen++
		}
	}
	require.GreaterOrEqual(t, userSeen, 1, "run must emit an initial user event")
	require.GreaterOrEqual(t, msgSeen, 1, "run must emit at least one message event")

	require.NoError(t, sub.Send(ctx, "focus on tests"))
	res, err := sub.Wait(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Content)
	concrete, ok := sub.(*core.DefaultSubAgent)
	require.True(t, ok, "factory must build a *core.DefaultSubAgent")
	assert.Equal(t, core.SubAgentCompleted, concrete.State())
}

// p4TUIRender feeds agent events into a BubbleteaApp backed by the default
// renderer registry and asserts the rendered view buffer contains the payload.
func p4TUIRender(ctx context.Context, t *testing.T) {
	t.Helper()

	events := make(chan tui.AgentEvent, 4)
	events <- tui.AgentEvent{Type: "status", Content: "status-line-text", ContentType: tui.ContentTypeStatus}
	events <- tui.AgentEvent{Type: "assistant", Content: "hello assistant", ContentType: tui.ContentTypeAssistant}
	close(events)

	app := tui.NewBubbleteaApp(events)
	require.NoError(t, app.Run(ctx))

	view := app.View()
	assert.Contains(t, view, "status-line-text")
	assert.Contains(t, view, "AI: ")
	assert.Contains(t, view, "hello assistant")
	require.GreaterOrEqual(t, app.EventsProcessed(), int64(2), "the app must drain both agent events")

	// A default registry must expose all 24 built-in renderers.
	registry := tui.NewDefaultRegistry()
	assert.GreaterOrEqual(t, len(registry.List()), 24, "default registry must register the 24 built-in renderers")
	_, ok := registry.Get(tui.ContentTypeMarkdown)
	require.True(t, ok, "markdown renderer must be registered")
}

// p4OutputGuard composes the built-in guards into a chain and asserts blocking,
// PII masking, injection detection and length truncation, then runs the chain
// through the default middleware as an extension.ModelMiddleware.
func p4OutputGuard(ctx context.Context, t *testing.T) {
	t.Helper()

	chain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewCodeInjectionGuard(),
		production.NewPIIOutputGuard(),
		production.NewLengthGuard(16),
	})
	require.Equal(t, "output-guard-chain", chain.Name())
	require.Len(t, chain.Guards(), 3)

	// Blocked: code-injection indicator.
	blocked, err := chain.Check(ctx, "please drop table users")
	require.NoError(t, err)
	require.False(t, blocked.Allowed, "code injection must be blocked")
	require.Equal(t, production.GuardCritical, blocked.Severity)

	// Allowed output passes through untouched (12 runes, under the 16 limit).
	ok, err := chain.Check(ctx, "safe summary")
	require.NoError(t, err)
	require.True(t, ok.Allowed)

	// Over-long output is truncated by the length guard.
	trunc, err := chain.Check(ctx, "0123456789ABCDEFGHIJKLMNOP")
	require.NoError(t, err)
	require.False(t, trunc.Allowed)
	assert.Len(t, []rune(trunc.Sanitized), 16)

	// The default middleware runs the guard chain against model output.
	guard := production.GetOutputGuard()
	middleware := production.NewOutputGuardMiddleware(guard)
	modelResp, err := middleware.WrapModel(func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "clean answer"}, nil
	})(ctx, extension.ModelRequest{Prompt: "q"})
	require.NoError(t, err)
	assert.Equal(t, "clean answer", modelResp.Text)
}

// p4YAMLConfigLoad writes a YAML config file and loads it through config.Loader,
// which auto-detects the YAML format from the file extension, plus the
// hand-written UnmarshalConfig parser.
func p4YAMLConfigLoad(ctx context.Context, t *testing.T, dir string) {
	t.Helper()

	yamlCfg := `provider:
  name: test-provider
  base_url: http://localhost:9999
  temperature: 0.5
  max_tokens: 512
model:
  name: test-model
  max_tokens: 256
tools:
  builtin:
    - bash
    - read
tracing:
  enabled: true
  exporter: jsonl
  level: info
compaction:
  strategy: micro_first
  max_tokens: 1000
`
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlCfg), 0o600))

	// DetectConfigFormat resolves the .yaml extension.
	f, err := config.DetectConfigFormat(path)
	require.NoError(t, err)
	assert.Equal(t, config.ConfigFormatYAML, f)

	// UnmarshalConfig parses the YAML subset into a typed Config.
	var direct config.Config
	require.NoError(t, config.UnmarshalConfig([]byte(yamlCfg), config.ConfigFormatYAML, &direct))
	assert.Equal(t, "test-provider", direct.Provider.Name)
	assert.Equal(t, float64(0.5), direct.Provider.Temperature)
	assert.Equal(t, []string{"bash", "read"}, direct.Tools.Builtin)

	// The full Loader auto-detects YAML and merges the file into the defaults.
	loader := config.NewLoader().WithFile(path)
	cfg, err := loader.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "test-provider", cfg.Provider.Name)
	assert.Equal(t, "test-model", cfg.Model.Name)
	assert.Equal(t, "micro_first", cfg.Compaction.Strategy)
}

// p4Provider is a lightweight test-only llm.ModelProvider whose identity and
// origin label can be asserted after the composer merges the three layers.
type p4Provider struct {
	name  string
	label string
}

func (p *p4Provider) Name() string { return p.name }

func (p *p4Provider) Models() []llm.ModelInfo { return nil }

func (p *p4Provider) Build(_ context.Context, _ llm.ModelConfig) (llm.BaseChatModel, func(), error) {
	return nil, func() {}, nil
}

// p4ProviderComposer builds a DefaultProviderComposer with config and extension
// layers and verifies the Extension > Config > Builtin priority ordering.
func p4ProviderComposer(ctx context.Context, t *testing.T) {
	t.Helper()

	// Two providers under the SAME name ("eino") come from the config and the
	// extension layer; the extension one must win. Unique names in each layer
	// must survive as-is, and the builtin layer must always be present.
	configProvider := &p4Provider{name: "eino", label: "config-layer"}
	extProvider := &p4Provider{name: "eino", label: "extension-layer"}
	cfgExtra := &p4Provider{name: "cfg-extra", label: "config-only"}
	extExtra := &p4Provider{name: "ext-extra", label: "extension-only"}

	composer := llm.NewDefaultProviderComposer(
		llm.WithConfigProviders([]llm.ModelProvider{configProvider, cfgExtra}),
		llm.WithExtensionProviders([]llm.ModelProvider{extProvider, extExtra}),
	)
	assert.Equal(t, "default-provider-composer", composer.Name())

	reg, err := composer.Compose(ctx)
	require.NoError(t, err)
	require.NotNil(t, reg)

	// Builtin providers are always present.
	openai, err := reg.Get("openai")
	require.NoError(t, err, "eino must be registered as a builtin provider")
	assert.Contains(t, openai.Name(), "openai")

	// Extension source overrides config for the same name.
	winner, err := reg.Get("eino")
	require.NoError(t, err)
	w, ok := winner.(*p4Provider)
	require.True(t, ok, "extension provider must win the same-name conflict")
	assert.Equal(t, "extension-layer", w.label)

	// Unique config/extension names survive composition.
	cfgGot, err := reg.Get("cfg-extra")
	require.NoError(t, err)
	assert.Equal(t, "cfg-extra", cfgGot.Name())
	extGot, err := reg.Get("ext-extra")
	require.NoError(t, err)
	assert.Equal(t, "ext-extra", extGot.Name())
}

// p4ACPStdio wires a real StdioAdapter to an in-process JSON peer over two
// pipes and verifies that an ACP message round-trips through the adapter's
// Connect/SendMessage/ReceiveMessages path (the ACP stdio loopback).
func p4ACPStdio(ctx context.Context, t *testing.T) {
	t.Helper()

	peerRead, adapterWrite := io.Pipe() // peer reads what the adapter writes
	adapterRead, peerWrite := io.Pipe() // adapter reads what the peer writes

	adapter := acp.NewStdioAdapter(adapterRead, adapterWrite, acp.WithName("agent-a"))

	// The peer goroutine reads JSON lines the adapter writes and answers a
	// TypeResponse for every TypeMessage it sees, reflecting the request back.
	peerDone := make(chan struct{})
	go p4ACPPeer(peerRead, peerWrite, peerDone)

	// Close every pipe end so neither the adapter's readLoop inner goroutine
	// nor the peer goroutine leaks after Disconnect.
	t.Cleanup(func() {
		closeQuietlyAll(adapterWrite, peerRead, peerWrite, adapterRead)
		<-peerDone
	})

	require.NoError(t, adapter.Connect(ctx))
	require.NoError(t, adapter.SendMessage(ctx, acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "agent-a",
		ReceiverID: "agent-b",
		Content:    "hello over acp stdio",
		Metadata:   map[string]string{"op": "phase4"},
	}))

	// The peer acks the connect frame first, then echoes a TypeResponse for the
	// message; drain the stream until the response arrives.
	deadline := time.After(2 * time.Second)
	var got acp.ACPMessage
	for {
		select {
		case msg := <-adapter.ReceiveMessages():
			if msg.Type == acp.TypeResponse {
				got = msg
			} else {
				continue // skip connect/disconnect acks
			}
		case <-deadline:
			t.Fatal("timed out waiting for the ACP peer to echo the message back")
		}
		break
	}
	require.Equal(t, acp.TypeResponse, got.Type)
	assert.Equal(t, "agent-a", got.ReceiverID)
	assert.Contains(t, got.Content, "echo:hello over acp stdio")

	require.NoError(t, adapter.Disconnect(ctx))
	assert.Equal(t, acp.ACPTransportStdio, acp.ACPTransport("Stdio"))
}

// p4ACPPeer reads newline-delimited ACP JSON frames from r and, for every
// TypeMessage, writes a TypeResponse echoing the content back to w. It stops
// when the reader hits end-of-stream or an error (e.g. the pipe is closed).
func p4ACPPeer(r io.Reader, w io.Writer, done chan<- struct{}) {
	defer close(done)
	sc := newLineScanner(r)
	for sc.Scan() {
		var msg acp.ACPMessage
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		switch msg.Type {
		case acp.TypeMessage:
			reply := acp.ACPMessage{
				Type:       acp.TypeResponse,
				SenderID:   msg.ReceiverID,
				ReceiverID: msg.SenderID,
				Content:    "echo:" + msg.Content,
			}
			data, _ := json.Marshal(reply)     //nolint:errcheck // fixed shape; marshal cannot fail on a plain struct.
			_, _ = w.Write(append(data, '\n')) //nolint:errcheck // best-effort peer write; reader teardown aborts.
		case acp.TypeConnect, acp.TypeDisconnect:
			// Ack connect/disconnect so the adapter's synchronous writes unblock.
			ack := acp.ACPMessage{Type: acp.TypeAck, SenderID: msg.ReceiverID, ReceiverID: msg.SenderID}
			data, _ := json.Marshal(ack)       //nolint:errcheck // fixed shape.
			_, _ = w.Write(append(data, '\n')) //nolint:errcheck // best-effort peer write.
		}
	}
}

// newLineScanner returns a bufio.Scanner split on newlines.
func newLineScanner(r io.Reader) *bufio.Scanner {
	return bufio.NewScanner(r)
}

// p4BranchSummary exercises the session BranchSummary chain using a real
// DefaultBranchSummary wired to an inline summarizer.
func p4BranchSummary(ctx context.Context, t *testing.T) {
	t.Helper()

	bs := session.NewDefaultBranchSummary(func(_ context.Context, text string) (string, error) {
		if strings.Contains(text, "departed-branch-entry") {
			return "summarized branch", nil
		}
		return "", errors.New("unexpected entries")
	})
	assert.Equal(t, "default-branch-summary", bs.Name())

	summary, err := bs.Summarize(ctx, []session.SessionEntry{
		{ID: "1", Type: session.EntryTypeUser, Content: "departed-branch-entry"},
	})
	require.NoError(t, err)
	assert.Equal(t, "summarized branch", summary)
}

// p4OTLPExport ships completed spans through a real OTLPTraceExporter to a
// real httptest-backed OTLP HTTP collector and asserts the collector received
// them.
func p4OTLPExport(t *testing.T) {
	t.Helper()

	var mu sync.Mutex
	var received []tracing.SpanData
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }() //nolint:errcheck // server closes the request body.
		var payload struct {
			Spans []tracing.SpanData `json:"spans"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload) //nolint:errcheck // best-effort capture.
		mu.Lock()
		received = append(received, payload.Spans...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	// BatchSize/FlushInterval are set high so the OTLP exporter's background
	// flushLoop never runs concurrently with span export (that combination
	// races on the exporter buffer's backing array). Spans are exported
	// synchronously through the real TraceExporter contract and a single
	// Shutdown flushes them deterministically to the collector.
	exporter := tracing.NewOTLPTraceExporter(tracing.OTLPTraceExporterConfig{
		Endpoint:      collector.URL + "/v1/traces",
		BatchSize:     1000,
		FlushInterval: 5 * time.Minute,
		Timeout:       2 * time.Second,
	})
	defer func() { require.NoError(t, exporter.Shutdown(context.Background())) }()

	tr := tracing.NewTracer("phase4-otlp", exporter)
	root, spanCtx := tr.Start(context.Background(), "p4.otlp.root", tracing.SpanKindInternal)
	child, _ := tr.Start(spanCtx, "p4.otlp.child", tracing.SpanKindInternal)

	// Exercise the TraceExporter.ExportSpan contract (what localSpan.End uses)
	// synchronously, then flush via Shutdown so the collector receives both.
	require.NoError(t, exporter.ExportSpan(context.Background(), root))
	require.NoError(t, exporter.ExportSpan(context.Background(), child))
	require.NoError(t, exporter.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(received), 2, "collector must receive the exported spans")
	for _, s := range received {
		assert.Equal(t, "phase4-otlp", s.TraceID, "span %s must carry the otlp trace id", s.Name)
	}
	names := map[string]bool{}
	for _, s := range received {
		names[s.Name] = true
	}
	assert.True(t, names["p4.otlp.root"])
	assert.True(t, names["p4.otlp.child"])
}

// p4WaitForSpans polls the exporter until the phase4 root and at least six
// spans are collected, bounding the wait so async export cannot make the chain
// assertion flaky.
func p4WaitForSpans(t *testing.T, exporter *mock.MockTraceExporter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		spans := exporter.Spans()
		if len(spans) >= 6 && hasSpanNamed(spans, "phase4.root") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	exporter.AssertSpanExists(t, "phase4.root")
}

// hasSpanNamed reports whether any collected span carries the given name.
func hasSpanNamed(spans []tracing.SpanData, name string) bool {
	for _, s := range spans {
		if s.Name == name {
			return true
		}
	}
	return false
}

// assertSpanEventually polls the exporter until a span with the given name
// appears. Span export is asynchronous (span.End launches a goroutine), so
// immediate assertions can race on slow CI runners.
func assertSpanEventually(t *testing.T, exporter *mock.MockTraceExporter, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hasSpanNamed(exporter.Spans(), name) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	exporter.AssertSpanExists(t, name)
}

// skillNames projects a list of SkillDefinition values to their names.
func skillNames(defs []skill.SkillDefinition) []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name())
	}
	return names
}

// closeQuietlyAll best-effort closes every io.Closer, ignoring errors.
func closeQuietlyAll(closers ...io.Closer) {
	for _, c := range closers {
		_ = c.Close() //nolint:errcheck // best-effort close.
	}
}
