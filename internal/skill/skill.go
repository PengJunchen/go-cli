// Package skill defines the skill system contracts: the SkillDefinition
// model, the SkillLoader that parses skill files from disk, and the
// SkillRegistry that discovers and matches skills for the agent loop. It also
// provides a SkillAdapter that maps a SkillDefinition onto the tools contract
// so a skill can be executed as a tool by the agent loop.
package skill

import (
	"encoding/json"
	"reflect"
)

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
	// Prompt returns the instruction text to feed to the model when the
	// skill executes.
	Prompt() string
	// Tools lists tools suggested by the skill. SkillAdapter does not enforce
	// this list as an executor permission boundary.
	Tools() []string
	// Parameters holds legacy parameter metadata or defaults. It is not a JSON
	// Schema for tool-call arguments.
	Parameters() map[string]any
	// TriggerHint returns a natural-language phrase that hints when the skill
	// should be matched.
	TriggerHint() string
}

// ParameterSchemaProvider is an optional capability for definitions that
// declare the JSON Schema accepted by a skill tool call. SkillDefinition is
// intentionally unchanged so existing implementations remain compatible.
type ParameterSchemaProvider interface {
	ParameterSchema() any
}

// skillOptions holds the configurable fields of a skill during construction.
// It is an internal construction aid so that SkillOption closures do not
// reference the concrete DefaultSkillDefinition type.
type skillOptions struct {
	description     string
	version         string
	category        string
	prompt          string
	tools           []string
	parameters      map[string]any
	parameterSchema any
	triggerHint     string
}

// SkillOption configures a skill during construction.
type SkillOption func(*skillOptions)

// DefaultSkillDefinition is the default SkillDefinition implementation. It is
// a plain data holder returned by NewSkill and by the YAMLSkillLoader.
type DefaultSkillDefinition struct {
	name            string
	description     string
	version         string
	category        string
	prompt          string
	tools           []string
	parameters      map[string]any
	parameterSchema any
	triggerHint     string
}

// Compile-time assertion that DefaultSkillDefinition satisfies SkillDefinition.
var _ SkillDefinition = (*DefaultSkillDefinition)(nil)
var _ ParameterSchemaProvider = (*DefaultSkillDefinition)(nil)

// NewSkill constructs a skill name with optional option overrides.
func NewSkill(name string, opts ...SkillOption) SkillDefinition {
	o := &skillOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return &DefaultSkillDefinition{
		name:            name,
		description:     o.description,
		version:         o.version,
		category:        o.category,
		prompt:          o.prompt,
		tools:           append([]string(nil), o.tools...),
		parameters:      cloneSkillMap(o.parameters),
		parameterSchema: cloneSkillValue(o.parameterSchema),
		triggerHint:     o.triggerHint,
	}
}

// WithDescription sets the skill description.
func WithDescription(d string) SkillOption {
	return func(o *skillOptions) { o.description = d }
}

// WithVersion sets the skill version.
func WithVersion(v string) SkillOption {
	return func(o *skillOptions) { o.version = v }
}

// WithCategory sets the skill category.
func WithCategory(c string) SkillOption {
	return func(o *skillOptions) { o.category = c }
}

// WithPrompt sets the skill prompt.
func WithPrompt(p string) SkillOption {
	return func(o *skillOptions) { o.prompt = p }
}

// WithTools sets tool names suggested by the skill prompt. The adapter does not
// enforce these names as executor permissions.
func WithTools(tools ...string) SkillOption {
	return func(o *skillOptions) { o.tools = append([]string(nil), tools...) }
}

// WithParameters sets legacy parameter metadata or defaults. The adapter does
// not interpret this map as an argument schema.
func WithParameters(p map[string]any) SkillOption {
	return func(o *skillOptions) {
		o.parameters = cloneSkillMap(p)
	}
}

// WithParameterSchema declares the JSON Schema accepted by the skill tool.
// Nil leaves the optional schema undeclared; legacy Parameters are not used as
// a substitute.
func WithParameterSchema(schema any) SkillOption {
	return func(o *skillOptions) { o.parameterSchema = cloneSkillValue(schema) }
}

// WithTriggerHint sets the natural-language trigger hint.
func WithTriggerHint(h string) SkillOption {
	return func(o *skillOptions) { o.triggerHint = h }
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

// Tools returns tool names suggested by the skill. The adapter does not
// enforce these names as executor permissions.
func (d *DefaultSkillDefinition) Tools() []string { return append([]string(nil), d.tools...) }

// Parameters returns legacy parameter metadata or defaults. This map is not a
// JSON Schema for tool-call arguments.
func (d *DefaultSkillDefinition) Parameters() map[string]any { return cloneSkillMap(d.parameters) }

// ParameterSchema returns a defensive copy of the optional tool-argument
// schema. A nil result means no schema was declared.
func (d *DefaultSkillDefinition) ParameterSchema() any { return cloneSkillValue(d.parameterSchema) }

// TriggerHint returns the natural-language trigger hint.
func (d *DefaultSkillDefinition) TriggerHint() string { return d.triggerHint }

func cloneSkillMap(value map[string]any) map[string]any {
	cloned := cloneSkillReflect(reflect.ValueOf(value), make(map[skillCloneVisitKey]reflect.Value))
	if !cloned.IsValid() {
		return nil
	}
	result, ok := cloned.Interface().(map[string]any)
	if !ok {
		return nil
	}
	return result
}

// cloneSkillValue copies the JSON-shaped schema graph accepted by the prepared
// protocol while leaving semantic schema validation to the shared R4 gate.
func cloneSkillValue(value any) any {
	cloned, ok := cloneSkillSchemaValue(reflect.ValueOf(value), 0)
	if !ok {
		// SkillOption cannot return an error. Preserve invalidity as an
		// explicit non-JSON value so the shared R4 validator rejects the
		// schema instead of seeing a truncated or aliased graph.
		return invalidSkillSchemaClone{}
	}
	if !cloned.IsValid() {
		return nil
	}
	return cloned.Interface()
}

type invalidSkillSchemaClone struct{}

var (
	skillJSONNumberType    = reflect.TypeOf(json.Number(""))
	skillJSONMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
)

// cloneSkillSchemaValue copies the bounded JSON value subset accepted by the
// prepared-tool protocol. The bound and supported kinds mirror R4's frozen
// schema boundary; semantic keyword validation remains owned by R4.
func cloneSkillSchemaValue(value reflect.Value, depth int) (reflect.Value, bool) {
	if !value.IsValid() {
		return value, true
	}
	if depth > 128 || value.Type() != skillJSONNumberType && value.Type().Implements(skillJSONMarshalerType) {
		return reflect.Value{}, false
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type()), true
		}
		// Like the protocol JSON copier, interface wrappers are transparent to
		// the depth budget; map/slice/array containers consume the depth.
		cloned, ok := cloneSkillSchemaValue(value.Elem(), depth)
		if !ok {
			return reflect.Value{}, false
		}
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result, true
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type()), true
		}
		if value.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, false
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			item, ok := cloneSkillSchemaValue(iterator.Value(), depth+1)
			if !ok {
				return reflect.Value{}, false
			}
			key := iterator.Key()
			result.SetMapIndex(key, item)
		}
		return result, true
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type()), true
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			item, ok := cloneSkillSchemaValue(value.Index(index), depth+1)
			if !ok {
				return reflect.Value{}, false
			}
			result.Index(index).Set(item)
		}
		return result, true
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			item, ok := cloneSkillSchemaValue(value.Index(index), depth+1)
			if !ok {
				return reflect.Value{}, false
			}
			result.Index(index).Set(item)
		}
		return result, true
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return value, true
	default:
		return reflect.Value{}, false
	}
}

type skillCloneVisitKey struct {
	typeOf   reflect.Type
	address  any
	length   int
	capacity int
}

// cloneSkillReflect copies containers in legacy metadata and argument snapshots
// without changing scalar values. JSON argument encoding rejects cycles and
// unsupported runtime values before this helper is used by SkillAdapter.Execute.
func cloneSkillReflect(value reflect.Value, seen map[skillCloneVisitKey]reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneSkillReflect(value.Elem(), seen)
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		key := skillCloneKey(value)
		if previous, ok := seen[key]; ok {
			return previous
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		seen[key] = result
		iterator := value.MapRange()
		for iterator.Next() {
			clonedKey := cloneSkillReflect(iterator.Key(), seen)
			clonedValue := cloneSkillReflect(iterator.Value(), seen)
			result.SetMapIndex(clonedKey, clonedValue)
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		if value.Len() > 0 {
			key := skillCloneKey(value)
			if previous, ok := seen[key]; ok {
				return previous
			}
			seen[key] = result
		}
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneSkillReflect(value.Index(index), seen))
		}
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		key := skillCloneKey(value)
		if previous, ok := seen[key]; ok {
			return previous
		}
		result := reflect.New(value.Type().Elem())
		seen[key] = result
		result.Elem().Set(cloneSkillReflect(value.Elem(), seen))
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneSkillReflect(value.Index(index), seen))
		}
		return result
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// Preserve legacy metadata values as-is. If such a value appears in
		// tool arguments, json.Marshal rejects it before SkillAdapter formats
		// the prompt output.
		return value
	default:
		return value
	}
}

func skillCloneKey(value reflect.Value) skillCloneVisitKey {
	key := skillCloneVisitKey{typeOf: value.Type()}
	switch value.Kind() {
	case reflect.Map:
		key.address = value.UnsafePointer()
	case reflect.Slice:
		key.address = value.Pointer()
		key.length = value.Len()
		key.capacity = value.Cap()
	case reflect.Pointer:
		key.address = value.Pointer()
	}
	return key
}
