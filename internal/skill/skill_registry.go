package skill

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// SkillRegistry is the contract a repository of skills satisfies. It supports
// registering, looking up, enumerating, matching and removing skills. Matching
// supports progressive disclosure: narrower queries (a category filter or a
// hint) yield better-ranked results.
type SkillRegistry interface {
	// Register adds a skill definition to the registry.
	Register(ctx context.Context, def SkillDefinition) error
	// Get returns the skill with the given name, if present.
	Get(ctx context.Context, name string) (SkillDefinition, bool)
	// List returns all registered skills, optionally filtered by category.
	List(ctx context.Context, category ...string) []SkillDefinition
	// Match returns skills that best match a natural-language hint.
	Match(ctx context.Context, hint string) []SkillDefinition
	// Unregister removes the named skill, returning ok if it was present.
	Unregister(ctx context.Context, name string) error
}

// ErrSkillNotFound is returned by Get when no skill with the requested name
// has been registered.
var ErrSkillNotFound = errors.New("skill: skill not found")

// ErrNilSkill is returned by Register when def is nil.
var ErrNilSkill = errors.New("skill: cannot register a nil skill definition")

// DefaultSkillRegistry is the default SkillRegistry implementation. It is
// concurrency-safe: all methods may be called concurrently. Skills are indexed
// by name and cached by category so List can filter without a full scan.
//
// Progressive disclosure: List(category...) narrows by category when any is
// given; Match(hint) does case-insensitive substring matching against the
// name, description and trigger hint and returns matches sorted by relevance.
type DefaultSkillRegistry struct {
	mu         sync.RWMutex
	byName     map[string]SkillDefinition
	byCategory map[string][]string // category -> names, in registration order
	order      []string            // global registration order
}

// Compile-time assertion that DefaultSkillRegistry satisfies SkillRegistry.
var _ SkillRegistry = (*DefaultSkillRegistry)(nil)

// NewDefaultSkillRegistry returns an empty, ready-to-use registry.
func NewDefaultSkillRegistry() *DefaultSkillRegistry {
	return &DefaultSkillRegistry{
		byName:     map[string]SkillDefinition{},
		byCategory: map[string][]string{},
	}
}

// Register stores def under its name and indexes it by category. It returns an
// error when def is nil or its name is empty. Registering a name a second time
// overwrites the previous definition and keeps its registration position.
func (r *DefaultSkillRegistry) Register(ctx context.Context, def SkillDefinition) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.register", tracing.SpanKindInternal)
	spanDefer := func() { span.End() }
	defer func() { spanDefer() }()

	if err := spanCtx.Err(); err != nil {
		return err
	}
	if def == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, ErrNilSkill.Error())
		return ErrNilSkill
	}
	name := def.Name()
	if name == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, "skill: empty skill name")
		return errors.New("skill: cannot register a skill with an empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byName[name]; !ok {
		r.order = append(r.order, name)
	}
	r.byName[name] = def

	category := def.Category()
	if category != "" {
		names := r.byCategory[category]
		if !containsString(names, name) {
			r.byCategory[category] = append(names, name)
		}
	}

	span.SetAttributes(
		tracing.Attribute{Key: "skill_name", Value: name},
		tracing.Attribute{Key: "category", Value: category},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	slog.Info("skill.register", "name", name, "category", category)
	return nil
}

// Get returns the skill registered under name, or ErrSkillNotFound.
func (r *DefaultSkillRegistry) Get(_ context.Context, name string) (SkillDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.byName[name]
	return def, ok
}

// List returns all registered skills in registration order. When one or more
// categories are given, only skills belonging to those categories are returned.
// The returned slice is a copy, so callers cannot mutate the registry through it.
func (r *DefaultSkillRegistry) List(_ context.Context, category ...string) []SkillDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(category) > 0 {
		return r.listByCategoryLocked(category)
	}

	defs := make([]SkillDefinition, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.byName[name])
	}
	return defs
}

// listByCategoryLocked returns skills that belong to any of the given
// categories, preserving the global registration order.
func (r *DefaultSkillRegistry) listByCategoryLocked(categories []string) []SkillDefinition {
	seen := map[string]bool{}
	var defs []SkillDefinition
	for _, name := range r.order {
		def := r.byName[name]
		if seen[name] {
			continue
		}
		for _, cat := range categories {
			if def.Category() == cat {
				seen[name] = true
				defs = append(defs, def)
				break
			}
		}
	}
	return defs
}

// Match returns skills whose name, description or trigger hint contains hint
// (case-insensitive). Results are sorted best-first: exact name matches and
// name-prefix matches rank above description/hint substring matches
// (progressive disclosure).
func (r *DefaultSkillRegistry) Match(_ context.Context, hint string) []SkillDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(hint))
	if q == "" {
		return nil
	}

	type scored struct {
		def   SkillDefinition
		score int
	}
	matches := make([]scored, 0)

	// Rebuild candidates from the ordered index so iteration is deterministic.
	for _, name := range r.order {
		def := r.byName[name]
		score := matchScore(def, q)
		if score > 0 {
			matches = append(matches, scored{def: def, score: score})
		}
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		// Tie-break by name so the order is stable.
		return matches[i].def.Name() < matches[j].def.Name()
	})

	result := make([]SkillDefinition, 0, len(matches))
	for _, m := range matches {
		result = append(result, m.def)
	}
	return result
}

// matchScore returns a relevance score (>0) when def matches q, else 0.
// Exact name and name-prefix matches score highest; description and trigger
// hint substring matches score lower.
func matchScore(def SkillDefinition, q string) int {
	name := strings.ToLower(def.Name())
	if name == q {
		return 5
	}
	if strings.HasPrefix(name, q) {
		return 4
	}
	if strings.Contains(name, q) {
		return 3
	}
	if strings.Contains(strings.ToLower(def.Description()), q) {
		return 2
	}
	if strings.Contains(strings.ToLower(def.TriggerHint()), q) {
		return 2
	}
	return 0
}

// Unregister removes the skill with the given name and its category index
// entries. It returns ErrSkillNotFound when the skill was not present.
func (r *DefaultSkillRegistry) Unregister(ctx context.Context, name string) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.unregister", tracing.SpanKindInternal)
	defer span.End()

	if err := spanCtx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	def, ok := r.byName[name]
	if !ok {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusOK, "")
		slog.Info("skill.unregister.not_found", "name", name)
		return ErrSkillNotFound
	}

	delete(r.byName, name)
	category := def.Category()
	if category != "" {
		names := r.byCategory[category]
		for i, n := range names {
			if n == name {
				r.byCategory[category] = append(names[:i], names[i+1:]...)
				break
			}
		}
	}
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}

	span.SetAttributes(
		tracing.Attribute{Key: "skill_name", Value: name},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	slog.Info("skill.unregister", "name", name)
	return nil
}

// containsString reports whether list contains s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
