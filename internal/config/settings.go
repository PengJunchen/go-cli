package config

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// SettingLayer identifies the scope a setting lives in. Layer priority is
// resolved by the concrete implementation; project settings generally override
// global ones.
type SettingLayer string

// Setting layers supported by DefaultSettings.
const (
	// SettingGlobal holds settings shared across all projects.
	SettingGlobal SettingLayer = "global"
	// SettingProject holds per-project settings that override global ones.
	SettingProject SettingLayer = "project"
)

// String returns the canonical string form of the layer.
func (l SettingLayer) String() string { return string(l) }

// ErrUntrustedProject is returned when a SettingProject write is rejected by
// trust gating.
var ErrUntrustedProject = errors.New("project not trusted for setting write")

// Settings is a layered key/value configuration store with a global and a
// project layer. It is designed so a downstream approval/trust policy can gate
// project-level writes without the config package importing approval.
type Settings interface {
	// Get resolves a key, preferring the project layer over the global layer.
	Get(ctx context.Context, key string) (any, error)
	// Set stores value under key in layer. Project writes may be rejected by a
	// configured trust check.
	Set(ctx context.Context, key string, value any, layer SettingLayer) error
	// Delete removes key from layer. It is idempotent.
	Delete(ctx context.Context, key string, layer SettingLayer) error
	// List returns a copy of the requested layers (all layers when none given).
	List(ctx context.Context, layer ...SettingLayer) (map[string]any, error)
	// Name returns the settings identifier.
	Name() string
}

// DefaultSettings is a dual-layer in-memory Settings protected by a read-write
// lock. Project settings override global settings on Get.
type DefaultSettings struct {
	mu          sync.RWMutex
	layers      map[SettingLayer]map[string]any
	trustCheck  func(ctx context.Context, projectPath string) bool
	projectPath string
	name        string
}

// Compile-time assertion that DefaultSettings satisfies Settings.
var _ Settings = (*DefaultSettings)(nil)

// SettingsOption configures a DefaultSettings.
type SettingsOption func(*settingsOptions)

type settingsOptions struct {
	name        string
	trustCheck  func(ctx context.Context, projectPath string) bool
	projectPath string
}

// WithTrustCheck wires a trust-decision callback used to gate SettingProject
// writes. When nil (the default) project writes are allowed unconditionally.
func WithTrustCheck(fn func(ctx context.Context, projectPath string) bool) SettingsOption {
	return func(o *settingsOptions) { o.trustCheck = fn }
}

// WithProjectPath sets the project path evaluated by the trust check.
func WithProjectPath(path string) SettingsOption {
	return func(o *settingsOptions) { o.projectPath = path }
}

// WithSettingsName overrides the identifier returned by Name.
func WithSettingsName(name string) SettingsOption {
	return func(o *settingsOptions) { o.name = name }
}

// NewDefaultSettings returns an empty dual-layer DefaultSettings.
func NewDefaultSettings(opts ...SettingsOption) Settings {
	o := &settingsOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	name := o.name
	if name == "" {
		name = "default-settings"
	}
	return &DefaultSettings{
		layers: map[SettingLayer]map[string]any{
			SettingGlobal:  make(map[string]any),
			SettingProject: make(map[string]any),
		},
		trustCheck:  o.trustCheck,
		projectPath: o.projectPath,
		name:        name,
	}
}

// Get resolves a key preferring the project layer over the global layer. It
// emits a config.settings span carrying the key and resolved layer.
func (s *DefaultSettings) Get(ctx context.Context, key string) (any, error) {
	span, ctx := tracing.SpanFromContext(ctx, "config.settings", tracing.SpanKindInternal)

	s.mu.RLock()
	var value any
	found := false
	layer := SettingGlobal
	if v, ok := s.layers[SettingProject][key]; ok {
		value, found, layer = v, true, SettingProject
	} else if v, ok := s.layers[SettingGlobal][key]; ok {
		value, found = v, true
	}
	s.mu.RUnlock()

	span.SetAttributes(
		tracing.Attribute{Key: "key", Value: key},
		tracing.Attribute{Key: "layer", Value: layer.String()},
		tracing.Attribute{Key: "found", Value: found},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.DebugContext(ctx, "config.settings.get",
		"key", key,
		"layer", layer.String(),
		"found", found,
	)
	span.End()

	if !found {
		return nil, nil
	}
	return value, nil
}

// Set stores value under key in layer, gating project writes when a trust check
// is configured. It emits a config.settings span with a trusted attribute.
func (s *DefaultSettings) Set(ctx context.Context, key string, value any, layer SettingLayer) error {
	span, ctx := tracing.SpanFromContext(ctx, "config.settings", tracing.SpanKindInternal)
	trusted := true

	if layer == SettingProject && s.trustCheck != nil {
		trusted = s.trustCheck(ctx, s.projectPath)
		if !trusted {
			span.SetAttributes(
				tracing.Attribute{Key: "key", Value: key},
				tracing.Attribute{Key: "layer", Value: layer.String()},
				tracing.Attribute{Key: "trusted", Value: false},
			)
			logger := tracing.NewTraceLogger(span, slog.Default())
			logger.WarnContext(ctx, "config.settings.trust_rejected",
				"key", key,
				"layer", layer.String(),
				"project_path", s.projectPath,
			)
			span.End()
			return ErrUntrustedProject
		}
	}

	s.mu.Lock()
	if s.layers[layer] == nil {
		s.layers[layer] = make(map[string]any)
	}
	s.layers[layer][key] = value
	s.mu.Unlock()

	span.SetAttributes(
		tracing.Attribute{Key: "key", Value: key},
		tracing.Attribute{Key: "layer", Value: layer.String()},
		tracing.Attribute{Key: "trusted", Value: trusted},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.DebugContext(ctx, "config.settings.set",
		"key", key,
		"layer", layer.String(),
		"trusted", trusted,
	)
	span.End()
	return nil
}

// Delete removes key from layer. It is idempotent.
func (s *DefaultSettings) Delete(_ context.Context, key string, layer SettingLayer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.layers[layer] != nil {
		delete(s.layers[layer], key)
	}
	return nil
}

// List returns a copy of the requested layers (all when none requested),
// merged with the later layers overriding earlier ones.
func (s *DefaultSettings) List(_ context.Context, layers ...SettingLayer) (map[string]any, error) {
	if len(layers) == 0 {
		layers = []SettingLayer{SettingGlobal, SettingProject}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]any)
	for _, layer := range layers {
		for k, v := range s.layers[layer] {
			out[k] = v
		}
	}
	return out, nil
}

// Name returns the settings identifier.
func (s *DefaultSettings) Name() string { return s.name }

// registerSettings holds the process-wide active Settings implementation.
var (
	settingsMu      sync.RWMutex
	defaultSettings Settings
)

// RegisterSettings sets the active Settings. A nil value resets to a fresh
// DefaultSettings.
func RegisterSettings(s Settings) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if s == nil {
		s = NewDefaultSettings()
	}
	slog.Info("config.register.settings", "name", s.Name())
	defaultSettings = s
}

// GetSettings returns the active Settings, lazily defaulting to a
// DefaultSettings when none has been registered.
func GetSettings() Settings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	if defaultSettings == nil {
		return NewDefaultSettings()
	}
	return defaultSettings
}
