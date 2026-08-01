// Package skill defines the skill system contracts: the SkillDefinition
// model, the SkillLoader that parses skill files from disk, and the
// SkillRegistry that discovers and matches skills for the agent loop. It also
// provides a SkillAdapter that maps a SkillDefinition onto the tools contract
// so a skill can be executed as a tool by the agent loop.
package skill

// SkillDefinition describes a reusable instruction package (a "skill") that an
// agent can load and execute. It is the common contract shared by the loader,
// the registry and the adapter.
type SkillDefinition interface {
	// Name returns the unique name of the skill.
	Name() string
	// Description returns a human-readable description of the skill.
	Description() string
	// Version returns the semantic version of the skill.
	Version() string
	// Category returns the coarse grouping a skill belongs to (e.g. coding).
	Category() string
	// Prompt returns the instrution text to feed to the model when the
	// skill executes.
	Prompt() string
	// Tools lists the names of the tools the skill may use.
	Tools() []string
	// Parameters holds the structured parameters a skill accepts.
	Parameters() map[string]any
	// TriggerHint returns a natural-language phrase that hints when the skill
	// should be matched.
	TriggerHint() string
}

// SkillOption configures a DefaultSkillDefinition during construction.
type SkillOption func(*DefaultSkillDefinition)

// DefaultSkillDefinition is the default SkillDefinition implementation. It is
// a plain data holder returned by NewSkill and by the YAMLSkillLoader.
type DefaultSkillDefinition struct {
	name        string
	description string
	version     string
	category    string
	prompt      string
	tools       []string
	parameters  map[string]any
	triggerHint string
}

// Compile-time assertion that DefaultSkillDefinition satisfies SkillDefinition.
var _ SkillDefinition = (*DefaultSkillDefinition)(nil)

// NewSkill constructs a skill name with optional option overrides.
func NewSkill(name string, opts ...SkillOption) *DefaultSkillDefinition {
	def := &DefaultSkillDefinition{name: name}
	for _, opt := range opts {
		opt(def)
	}
	return def
}

// WithDescription sets the skill description.
func WithDescription(d string) SkillOption {
	return func(def *DefaultSkillDefinition) { def.description = d }
}

// WithVersion sets the skill version.
func WithVersion(v string) SkillOption {
	return func(def *DefaultSkillDefinition) { def.version = v }
}

// WithCategory sets the skill category.
func WithCategory(c string) SkillOption {
	return func(def *DefaultSkillDefinition) { def.category = c }
}

// WithPrompt sets the skill prompt.
func WithPrompt(p string) SkillOption {
	return func(def *DefaultSkillDefinition) { def.prompt = p }
}

// WithTools sets the list of tool names the skill may use.
func WithTools(tools ...string) SkillOption {
	return func(def *DefaultSkillDefinition) { def.tools = append([]string(nil), tools...) }
}

// WithParameters sets the structured parameters map.
func WithParameters(p map[string]any) SkillOption {
	return func(def *DefaultSkillDefinition) {
		m := make(map[string]any, len(p))
		for k, v := range p {
			m[k] = v
		}
		def.parameters = m
	}
}

// WithTriggerHint sets the natural-language trigger hint.
func WithTriggerHint(h string) SkillOption {
	return func(def *DefaultSkillDefinition) { def.triggerHint = h }
}

// Name returns the skill name.
func (d *DefaultSkillDefinition) Name() string { return d.name }

// Description returns the skill description.
func (d *DefaultSkillDefinition) Description() string { return d.description }

// Version returns the skill version.
func (d *DefaultSkillDefinition) Version() string { return d.version }

// Category returns the skill category.
func (d *DefaultSkillDefinition) Category() string { return d.category }

// Prompt returns the skill prompt.
func (d *DefaultSkillDefinition) Prompt() string { return d.prompt }

// Tools returns the list of tool names the skill may use.
func (d *DefaultSkillDefinition) Tools() []string { return d.tools }

// Parameters returns the structured parameters map.
func (d *DefaultSkillDefinition) Parameters() map[string]any { return d.parameters }

// TriggerHint returns the natural-language trigger hint.
func (d *DefaultSkillDefinition) TriggerHint() string { return d.triggerHint }
