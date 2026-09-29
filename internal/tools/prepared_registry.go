package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
)

var _ ToolCallPreparer = (*MiddlewareToolRegistry)(nil)
var _ StreamingToolCallPreparer = (*MiddlewareToolRegistry)(nil)

func (*MiddlewareToolRegistry) toolCallPreparerCapability() {}

// PrepareToolCall pins one registered definition, its schema, trusted metadata,
// and the configured execution stages before asking policy to authorize it.
func (r *MiddlewareToolRegistry) PrepareToolCall(ctx context.Context, call ToolCall) (PreparedInvocation, error) {
	return r.prepareToolCall(ctx, call, nil)
}

// PrepareStreamingToolCall binds one exact host-attested synchronous sink to a
// prepared streaming registration. It does not upgrade the registered tool's
// trusted provenance or effects.
func (r *MiddlewareToolRegistry) PrepareStreamingToolCall(ctx context.Context, call ToolCall, sink SynchronousStreamSink) (PreparedStreamingInvocation, error) {
	sealedSink, ok := sink.(*synchronousStreamSink)
	if !ok || sealedSink == nil || isNilStreamSink(sealedSink.sink) {
		return nil, ErrPreparedStreamSinkUnavailable
	}
	prepared, err := r.prepareToolCallWithStreamSink(ctx, call, nil, sealedSink)
	if err != nil {
		return nil, err
	}
	streaming, ok := prepared.(PreparedStreamingInvocation)
	if !ok {
		return nil, ErrPreparedInvocationUnavailable
	}
	return streaming, nil
}

func (r *MiddlewareToolRegistry) prepareToolCall(ctx context.Context, call ToolCall, proxyPin *protectedRegistrationPin) (PreparedInvocation, error) {
	return r.prepareToolCallWithStreamSink(ctx, call, proxyPin, nil)
}

func (r *MiddlewareToolRegistry) prepareToolCallWithStreamSink(ctx context.Context, call ToolCall, proxyPin *protectedRegistrationPin, attestedSink *synchronousStreamSink) (PreparedInvocation, error) {
	if err := validatePreparedContext(ctx); err != nil {
		return nil, err
	}
	if call.Name == "" {
		return nil, errors.New("tools: prepared call has no tool name")
	}

	snapshot, err := r.toolRegistrationSnapshot(ctx, call.Name)
	if err != nil {
		return nil, err
	}
	if proxyPin != nil && (snapshot.identity != proxyPin.identity || snapshot.generations.Registry != proxyPin.registryGeneration) {
		return nil, ErrPreparedProxyRegistrationStale
	}
	if attestedSink != nil {
		if _, ok := snapshot.definition.(StreamingBashTool); !ok {
			return nil, ErrPreparedInvocationUnavailable
		}
	}
	if !snapshot.pipeline.policyConfigured || !snapshot.pipeline.stagesConfigured || snapshot.pipeline.policy == nil {
		return nil, ErrPreparedToolPolicyUnavailable
	}
	if registryHasOpaqueLegacyWrapper(r) {
		return nil, ErrPreparedOpaqueWrapperUnsupported
	}
	if snapshot.definition == nil || snapshot.identity.ToolName() != call.Name {
		return nil, ErrPreparedRegistrationUnavailable
	}
	completionProof := snapshot.completionProof
	if len(snapshot.pipeline.stages.ExecutionWrappers) > 0 && !snapshot.pipeline.stages.SynchronousExecutionWrappers {
		completionProof = ""
	}
	var preparedQueue *DefaultFileMutationQueue
	if mutationToolNames[call.Name] {
		var ok bool
		preparedQueue, ok = snapshot.pipeline.stages.MutationQueue.(*DefaultFileMutationQueue)
		if !ok {
			return nil, ErrPreparedMutationQueueUnavailable
		}
		if completionProof == CompletionInlinePipelineJoined {
			completionProof = CompletionQueuedJobJoined
		}
	}

	normalized := call
	for _, preparer := range snapshot.pipeline.stages.ArgumentPreparers {
		if preparer == nil {
			continue
		}
		if _, ok := preparer.(*PathNormalizer); !ok {
			return nil, ErrPreparedNormalizerUnsupported
		}
		normalized, err = preparer.PrepareArguments(ctx, normalized)
		if err != nil {
			return nil, fmt.Errorf("tools: normalize prepared call %q: %w", call.Name, err)
		}
		if normalized.Name != call.Name || normalized.ID != call.ID {
			return nil, ErrPreparedCallMutation
		}
	}
	if snapshot.schema.Present() {
		schema := snapshot.schema.Schema()
		if err := validateCapturedSchema(normalized.Args, schema); err != nil {
			return nil, fmt.Errorf("tools: validate prepared call %q: %w", call.Name, err)
		}
	}
	if mutationToolNames[call.Name] {
		if err := validateMutationPayload(normalized); err != nil {
			return nil, fmt.Errorf("tools: invalid prepared mutation payload: %w", err)
		}
		if mutationPathFromCall(normalized) == "" {
			return nil, errors.New("tools: prepared mutation has no file path")
		}
	}
	frozenCall, err := freezeToolCall(normalized)
	if err != nil {
		return nil, err
	}
	fence := r.DispatchFence()
	if fence == nil {
		return nil, ErrDispatchFenceUnavailable
	}
	decision, err := snapshot.pipeline.policy(ctx, PreparedPolicyRequest{
		Call:        frozenCall,
		Identity:    snapshot.identity,
		Effects:     newTrustedEffectSet(snapshot.effects.Values()...),
		Schema:      snapshot.schema,
		Generations: snapshot.generations,
		Fence:       fence,
	})
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, ErrPreparedToolPolicyDenied
	}
	if decision.Generations != snapshot.generations {
		return nil, ErrDispatchFenceChanged
	}

	executor := preparedExecutor(snapshot.definition, snapshot.pipeline.stages.ExecutionWrappers, frozenCall)
	config := preparedInvocationConfig{
		fence:                   fence,
		generations:             decision.Generations,
		identity:                snapshot.identity,
		effects:                 snapshot.effects,
		dispatchLease:           snapshot.dispatchLease,
		canonicalWorkspaceRoot:  snapshot.canonicalWorkspaceRoot,
		workspaceReadWhitelist:  append([]string(nil), snapshot.workspaceReadWhitelist...),
		scopedReadProofEligible: snapshot.scopedReadProofEligible,
		schema:                  snapshot.schema,
		call:                    frozenCall,
		execute:                 executor,
		completionProof:         completionProof,
		streamingSink:           attestedSink,
		terminalObserver:        decision.TerminalObserver,
	}
	if preparedQueue != nil {
		config.schedule = preparedMutationScheduler(preparedQueue, frozenCall)
	}
	if streaming, ok := snapshot.definition.(StreamingBashTool); ok {
		streamExecutor := preparedStreamingExecutor(streaming, snapshot.pipeline.stages.ExecutionWrappers, frozenCall)
		return newPreparedStreamingInvocation(config, streamExecutor), nil
	}
	return newPreparedInvocation(config), nil
}

func (r *MiddlewareToolRegistry) validateProxyPin(ctx context.Context, pin protectedRegistrationPin) error {
	snapshot, err := r.toolRegistrationSnapshot(ctx, pin.identity.ToolName())
	if err != nil {
		if errors.Is(err, ErrToolNotFound) || errors.Is(err, ErrDeferredToolNotFound) {
			return ErrPreparedProxyRegistrationStale
		}
		return err
	}
	if snapshot.identity != pin.identity || snapshot.generations.Registry != pin.registryGeneration {
		return ErrPreparedProxyRegistrationStale
	}
	return nil
}

func registryHasOpaqueLegacyWrapper(reg ToolRegistry) bool {
	switch current := reg.(type) {
	case *MiddlewareToolRegistry:
		return len(current.wrappers) > 0 || registryHasOpaqueLegacyWrapper(current.inner)
	case *DeferredToolRegistryAdapter:
		return registryHasOpaqueLegacyWrapper(current.underlying)
	default:
		return false
	}
}

func validateCapturedSchema(args map[string]any, schema any) error {
	if err := validatePreparedSchemaNode(schema, true); err != nil {
		return err
	}
	root, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: schema root is not an object", ErrPreparedSchemaInvalid)
	}
	properties, ok := root["properties"].(map[string]any)
	if !ok {
		return fmt.Errorf("%w: schema properties are not an object", ErrPreparedSchemaInvalid)
	}
	if err := validatePreparedRequired(args, root); err != nil {
		return err
	}
	for name, value := range args {
		property, exists := properties[name]
		if !exists {
			if root["additionalProperties"] == false {
				return fmt.Errorf("unexpected parameter: %s", name)
			}
			continue
		}
		propertySchema, ok := property.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: property %q is not a schema object", ErrPreparedSchemaInvalid, name)
		}
		if err := validatePreparedSchemaValue(name, value, propertySchema); err != nil {
			return err
		}
	}
	return nil
}

func validatePreparedSchemaNode(value any, root bool) error {
	schema, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: schema node must be an object", ErrPreparedSchemaInvalid)
	}
	for key := range schema {
		switch key {
		case "type", "description", "properties", "required", "additionalProperties", "enum", "items", "minimum", "maximum":
		default:
			return fmt.Errorf("%w: unsupported keyword %q", ErrPreparedSchemaInvalid, key)
		}
	}
	typeValue, ok := schema["type"].(string)
	if !ok {
		return fmt.Errorf("%w: schema type must be a string", ErrPreparedSchemaInvalid)
	}
	if root && typeValue != "object" {
		return fmt.Errorf("%w: tool arguments must use object type", ErrPreparedSchemaInvalid)
	}
	switch typeValue {
	case "object", "array", "string", "number", "integer", "boolean":
	default:
		return fmt.Errorf("%w: unsupported schema type %q", ErrPreparedSchemaInvalid, typeValue)
	}
	if description, exists := schema["description"]; exists {
		if _, ok := description.(string); !ok {
			return fmt.Errorf("%w: description must be a string", ErrPreparedSchemaInvalid)
		}
	}
	if enum, exists := schema["enum"]; exists {
		values, ok := preparedSchemaArray(enum)
		if !ok || len(values) == 0 {
			return fmt.Errorf("%w: enum must be a non-empty array", ErrPreparedSchemaInvalid)
		}
		for _, item := range values {
			if !preparedSchemaValueMatchesType(item, typeValue) {
				return fmt.Errorf("%w: enum value does not match %s type", ErrPreparedSchemaInvalid, typeValue)
			}
		}
	}
	if typeValue != "object" {
		if _, exists := schema["properties"]; exists {
			return fmt.Errorf("%w: properties are only supported on object schemas", ErrPreparedSchemaInvalid)
		}
		if _, exists := schema["required"]; exists {
			return fmt.Errorf("%w: required is only supported on object schemas", ErrPreparedSchemaInvalid)
		}
		if _, exists := schema["additionalProperties"]; exists {
			return fmt.Errorf("%w: additionalProperties is only supported on object schemas", ErrPreparedSchemaInvalid)
		}
	}
	if typeValue != "array" {
		if _, exists := schema["items"]; exists {
			return fmt.Errorf("%w: items are only supported on array schemas", ErrPreparedSchemaInvalid)
		}
	}
	if typeValue != "number" && typeValue != "integer" {
		if _, exists := schema["minimum"]; exists {
			return fmt.Errorf("%w: minimum is only supported on numeric schemas", ErrPreparedSchemaInvalid)
		}
		if _, exists := schema["maximum"]; exists {
			return fmt.Errorf("%w: maximum is only supported on numeric schemas", ErrPreparedSchemaInvalid)
		}
	}
	minimum, hasMinimum := schemaRational(schema["minimum"])
	if _, exists := schema["minimum"]; exists && !hasMinimum {
		return fmt.Errorf("%w: minimum must be a finite number", ErrPreparedSchemaInvalid)
	}
	maximum, hasMaximum := schemaRational(schema["maximum"])
	if _, exists := schema["maximum"]; exists && !hasMaximum {
		return fmt.Errorf("%w: maximum must be a finite number", ErrPreparedSchemaInvalid)
	}
	if hasMinimum && hasMaximum && minimum.Cmp(maximum) > 0 {
		return fmt.Errorf("%w: minimum exceeds maximum", ErrPreparedSchemaInvalid)
	}
	if typeValue == "object" {
		propertiesValue, exists := schema["properties"]
		if !exists {
			return fmt.Errorf("%w: object schema must define properties", ErrPreparedSchemaInvalid)
		}
		properties, ok := propertiesValue.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: properties must be an object", ErrPreparedSchemaInvalid)
		}
		if additional, exists := schema["additionalProperties"]; exists {
			if _, ok := additional.(bool); !ok {
				return fmt.Errorf("%w: additionalProperties must be boolean", ErrPreparedSchemaInvalid)
			}
		}
		for name, property := range properties {
			if name == "" {
				return fmt.Errorf("%w: property name cannot be empty", ErrPreparedSchemaInvalid)
			}
			if err := validatePreparedSchemaNode(property, false); err != nil {
				return fmt.Errorf("%w: property %q: %v", ErrPreparedSchemaInvalid, name, err)
			}
		}
		if _, err := preparedRequiredNames(schema); err != nil {
			return err
		}
		return nil
	}
	if typeValue == "array" {
		itemSchema, exists := schema["items"]
		if !exists {
			return fmt.Errorf("%w: array schema must define items", ErrPreparedSchemaInvalid)
		}
		if err := validatePreparedSchemaNode(itemSchema, false); err != nil {
			return fmt.Errorf("%w: array items: %v", ErrPreparedSchemaInvalid, err)
		}
	}
	return nil
}

func preparedRequiredNames(schema map[string]any) ([]string, error) {
	value, exists := schema["required"]
	if !exists {
		return nil, nil
	}
	items, ok := preparedSchemaArray(value)
	if !ok {
		return nil, fmt.Errorf("%w: required must be an array of strings", ErrPreparedSchemaInvalid)
	}
	names := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	properties, _ := schema["properties"].(map[string]any)
	for _, item := range items {
		name, ok := item.(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("%w: required entries must be non-empty strings", ErrPreparedSchemaInvalid)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate required property %q", ErrPreparedSchemaInvalid, name)
		}
		if _, exists := properties[name]; !exists {
			return nil, fmt.Errorf("%w: required property %q is not declared", ErrPreparedSchemaInvalid, name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

func validatePreparedRequired(args map[string]any, schema map[string]any) error {
	names, err := preparedRequiredNames(schema)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, exists := args[name]; !exists {
			return fmt.Errorf("missing required parameter: %s", name)
		}
	}
	return nil
}

func validatePreparedSchemaValue(name string, value any, schema map[string]any) error {
	typeValue, ok := schema["type"].(string)
	if !ok {
		return fmt.Errorf("%w: schema type for parameter %q is not a string", ErrPreparedSchemaInvalid, name)
	}
	if !preparedSchemaValueMatchesType(value, typeValue) {
		return fmt.Errorf("parameter %q must be a %s", name, typeValue)
	}
	if enum, exists := schema["enum"]; exists {
		values, ok := preparedSchemaArray(enum)
		if !ok {
			return fmt.Errorf("%w: enum for parameter %q is not an array", ErrPreparedSchemaInvalid, name)
		}
		matched := false
		for _, candidate := range values {
			if reflect.DeepEqual(value, candidate) {
				matched = true
				break
			}
			left, leftOK := schemaRational(value)
			right, rightOK := schemaRational(candidate)
			if leftOK && rightOK && left.Cmp(right) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("parameter %q is not an allowed value", name)
		}
	}
	switch typeValue {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("parameter %q must be an object", name)
		}
		if err := validatePreparedRequired(object, schema); err != nil {
			return err
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("%w: properties for parameter %q are not an object", ErrPreparedSchemaInvalid, name)
		}
		for propertyName, propertyValue := range object {
			property, exists := properties[propertyName]
			if !exists {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("parameter %q contains unexpected property %q", name, propertyName)
				}
				continue
			}
			propertySchema, ok := property.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: property %q.%s is not a schema object", ErrPreparedSchemaInvalid, name, propertyName)
			}
			if err := validatePreparedSchemaValue(name+"."+propertyName, propertyValue, propertySchema); err != nil {
				return err
			}
		}
	case "array":
		items, ok := preparedSchemaArray(value)
		if !ok {
			return fmt.Errorf("parameter %q must be an array", name)
		}
		itemSchema, ok := schema["items"].(map[string]any)
		if !ok {
			return fmt.Errorf("%w: array items for parameter %q are not a schema object", ErrPreparedSchemaInvalid, name)
		}
		for index, item := range items {
			if err := validatePreparedSchemaValue(fmt.Sprintf("%s[%d]", name, index), item, itemSchema); err != nil {
				return err
			}
		}
	case "number", "integer":
		numeric, ok := schemaRational(value)
		if !ok {
			return fmt.Errorf("parameter %q must be a %s", name, typeValue)
		}
		if rawMinimum, exists := schema["minimum"]; exists {
			minimum, ok := schemaRational(rawMinimum)
			if !ok {
				return fmt.Errorf("%w: minimum for parameter %q is not a number", ErrPreparedSchemaInvalid, name)
			}
			if numeric.Cmp(minimum) < 0 {
				return fmt.Errorf("parameter %q must be at least %s", name, minimum.RatString())
			}
		}
		if rawMaximum, exists := schema["maximum"]; exists {
			maximum, ok := schemaRational(rawMaximum)
			if !ok {
				return fmt.Errorf("%w: maximum for parameter %q is not a number", ErrPreparedSchemaInvalid, name)
			}
			if numeric.Cmp(maximum) > 0 {
				return fmt.Errorf("parameter %q must be at most %s", name, maximum.RatString())
			}
		}
	}
	return nil
}

func preparedSchemaValueMatchesType(value any, typeValue string) bool {
	switch typeValue {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := schemaRational(value)
		return ok
	case "integer":
		number, ok := schemaRational(value)
		return ok && number.IsInt()
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := preparedSchemaArray(value)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}

func preparedSchemaArray(value any) ([]any, bool) {
	if values, ok := value.([]any); ok {
		return values, true
	}
	if values, ok := value.([]string); ok {
		result := make([]any, len(values))
		for index, item := range values {
			result[index] = item
		}
		return result, true
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) || rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	result := make([]any, rv.Len())
	for index := range result {
		result[index] = rv.Index(index).Interface()
	}
	return result, true
}

const (
	maxPreparedNumberText     = 256
	maxPreparedNumberExponent = 1024
)

func schemaRational(value any) (*big.Rat, bool) {
	var text string
	switch v := value.(type) {
	case json.Number:
		text = v.String()
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		text = strconv.FormatFloat(v, 'g', -1, 64)
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		text = strconv.FormatFloat(float64(v), 'g', -1, 32)
	case int:
		text = strconv.FormatInt(int64(v), 10)
	case int8:
		text = strconv.FormatInt(int64(v), 10)
	case int16:
		text = strconv.FormatInt(int64(v), 10)
	case int32:
		text = strconv.FormatInt(int64(v), 10)
	case int64:
		text = strconv.FormatInt(v, 10)
	case uint:
		text = strconv.FormatUint(uint64(v), 10)
	case uint8:
		text = strconv.FormatUint(uint64(v), 10)
	case uint16:
		text = strconv.FormatUint(uint64(v), 10)
	case uint32:
		text = strconv.FormatUint(uint64(v), 10)
	case uint64:
		text = strconv.FormatUint(v, 10)
	default:
		return nil, false
	}
	if len(text) == 0 || len(text) > maxPreparedNumberText {
		return nil, false
	}
	if exponentAt := strings.IndexAny(text, "eE"); exponentAt >= 0 {
		exponent, err := strconv.ParseInt(text[exponentAt+1:], 10, 32)
		if err != nil || exponent < -maxPreparedNumberExponent || exponent > maxPreparedNumberExponent {
			return nil, false
		}
	}
	if !json.Valid([]byte(text)) {
		return nil, false
	}
	var normalized json.Number
	if err := json.Unmarshal([]byte(text), &normalized); err != nil {
		return nil, false
	}
	rational, ok := new(big.Rat).SetString(normalized.String())
	return rational, ok
}

func preparedExecutor(def ToolDefinition, wrappers []ToolExecutorWrapper, frozen FrozenToolCall) func(context.Context, ToolCall) (*ToolResult, error) {
	canonical := frozen.ToolCall()
	var entered atomic.Bool
	executor := func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		if !entered.CompareAndSwap(false, true) {
			return nil, ErrPreparedInvocationConsumed
		}
		if !matchesFrozenCall(call, frozen) {
			return nil, ErrPreparedCallMutation
		}
		return def.Execute(ctx, canonical)
	}
	for i := len(wrappers) - 1; i >= 0; i-- {
		if wrappers[i] != nil {
			executor = wrappers[i](executor)
		}
	}
	return func(ctx context.Context, _ ToolCall) (*ToolResult, error) {
		return executor(ctx, frozen.ToolCall())
	}
}

func preparedStreamingExecutor(def StreamingBashTool, wrappers []ToolExecutorWrapper, frozen FrozenToolCall) func(context.Context, ToolCall, StreamSink) (*ToolResult, error) {
	var entered atomic.Bool
	return func(ctx context.Context, _ ToolCall, sink StreamSink) (*ToolResult, error) {
		canonical := frozen.ToolCall()
		executor := func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			if !entered.CompareAndSwap(false, true) {
				return nil, ErrPreparedInvocationConsumed
			}
			if !matchesFrozenCall(call, frozen) {
				return nil, ErrPreparedCallMutation
			}
			return def.ExecuteStreaming(ctx, canonical, sink)
		}
		for i := len(wrappers) - 1; i >= 0; i-- {
			if wrappers[i] != nil {
				executor = wrappers[i](executor)
			}
		}
		return executor(ctx, frozen.ToolCall())
	}
}

func matchesFrozenCall(call ToolCall, frozen FrozenToolCall) bool {
	if call.ID != frozen.id || call.Name != frozen.name {
		return false
	}
	candidate, err := freezeToolCall(call)
	return err == nil && candidate.digest == frozen.digest
}

func preparedMutationScheduler(queue *DefaultFileMutationQueue, frozen FrozenToolCall) func(context.Context, *preparedInvocation) (*ToolResult, error) {
	return func(ctx context.Context, invocation *preparedInvocation) (*ToolResult, error) {
		call := frozen.ToolCall()
		mutation := FileMutation{
			FilePath:  mutationPathFromCall(call),
			Operation: call.Name,
			Content:   mutationContentFromCall(call),
			ToolName:  call.Name,
		}
		resultCh, err := queue.enqueuePrepared(ctx, mutation, invocation)
		if err != nil {
			return nil, fmt.Errorf("tools: enqueue prepared %s: %w", call.Name, err)
		}
		select {
		case result := <-resultCh:
			if result.Error != nil {
				return nil, result.Error
			}
			if result.ToolResult == nil {
				return &ToolResult{
					Output:   fmt.Sprintf("%s queued and applied for %s", call.Name, mutation.FilePath),
					Metadata: map[string]any{"path": mutation.FilePath, "queued": true},
				}, nil
			}
			copy := *result.ToolResult
			copy.Metadata = make(map[string]any, len(result.ToolResult.Metadata)+1)
			for key, value := range result.ToolResult.Metadata {
				copy.Metadata[key] = value
			}
			copy.Metadata["queued"] = true
			return &copy, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
