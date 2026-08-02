package extension

import (
	"context"
	"log/slog"
	"time"
)

// This file defines the Phase 4 richer Extension system model: the Extension
// lifecycle contract, the event-hook contract and the hook result/action model.
// These types coexist in this package with the earlier ConfigProvider concept
// (extension.go) but are intentionally kept separate from it.

// Extension is the lifecycle contract a Phase 4 extension satisfies. An
// extension is initialized once against a registry, runs, and is shut down
// when the CLI terminates.
type Extension interface {
	// Name returns the extension identifier.
	Name() string
	// Init is invoked once at startup to let the extension register its
	// building blocks (tools, commands, providers, hooks, middleware) with the
	// given registry.
	Init(ctx context.Context, registry ExtensionRegistry) error
	// Shutdown is invoked when the CLI terminates to release resources.
	Shutdown(ctx context.Context) error
}

// defaultExtension is a pass-through stub that satisfies the Extension
// contract without registering anything or acquiring resources.
type defaultExtension struct {
	name string
}

var _ Extension = (*defaultExtension)(nil)

// Name returns the extension name, defaulting to "default-extension".
func (d *defaultExtension) Name() string {
	if d.name == "" {
		return "default-extension"
	}
	return d.name
}

// Init is a no-op that accepts the registry and logs the event.
func (d *defaultExtension) Init(_ context.Context, _ ExtensionRegistry) error {
	slog.Info("extension.init", "name", d.Name())
	return nil
}

// Shutdown is a no-op that logs the event.
func (d *defaultExtension) Shutdown(_ context.Context) error {
	slog.Info("extension.shutdown", "name", d.Name())
	return nil
}

// HookAction describes the disposition of a hook event after a Hook handles it.
type HookAction string

const (
	// HookActionPass indicates the event was observed and processing should
	// continue with the next hook.
	HookActionPass HookAction = "pass"
	// hookActionBlock indicates processing should stop and the event should be
	// rejected (e.g. a permission denial).
	hookActionBlock HookAction = "block"
	// hookActionTerminate indicates the whole run should stop immediately.
	hookActionTerminate HookAction = "terminate"
	// hookActionReplace indicates the caller should substitute its payload with
	// the HookResult.Replacement value.
	hookActionReplace HookAction = "replace"
)

// HookEvent is the immutable description of an event delivered to a Hook.
type HookEvent struct {
	// Name is the event name (e.g. "agent.before_run").
	Name string
	// Data carries optional structured payload associated with the event.
	Data any
	// Source identifies where the event originated (e.g. an extension name).
	Source string
	// Timestamp records when the event was produced.
	Timestamp time.Time
}

// HookResult is what a Hook returns after handling an event.
type HookResult struct {
	// Action is the disposition requested by the hook.
	Action HookAction
	// Reason describes the outcome, especially for block/terminate.
	Reason string
	// Replacement carries the substituted value when Action is hookActionReplace.
	Replacement any
}

// Hook is an event hook that observes agent events and may influence their
// processing through the returned HookResult.
type Hook interface {
	// Name returns the hook identifier.
	Name() string
	// Handle processes the given event and returns a HookResult.
	Handle(ctx context.Context, event HookEvent) HookResult
}

// defaultHook is a pass-through hook that always returns HookActionPass.
type defaultHook struct {
	name string
}

var _ Hook = (*defaultHook)(nil)

// Name returns the hook name, defaulting to "default-hook".
func (h *defaultHook) Name() string {
	return h.name
}

// Handle logs the event and returns HookActionPass.
func (h *defaultHook) Handle(_ context.Context, event HookEvent) HookResult {
	slog.Info("extension.hook", "name", h.Name(), "event", event.Name)
	return HookResult{Action: HookActionPass}
}
