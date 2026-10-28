package tools

import (
	"context"
	"errors"
	"reflect"
)

// WorkerRequestExecutor executes a request through one exact host-owned worker
// generation. Implementing this interface alone grants no completion contract.
type WorkerRequestExecutor interface {
	ExecuteWorkerTool(context.Context, ToolCall) (*ToolResult, error)
}

type ownedWorkerRequestTool struct {
	name, description string
	schema            ToolSchemaSnapshot
	execute           func(context.Context, ToolCall) (*ToolResult, error)
	lease             PreparedDispatchLease
}

type frozenWorkerRequestLease struct{ acquire func() (func(), error) }

func (l frozenWorkerRequestLease) Acquire() (func(), error) { return l.acquire() }

// NewOwnedWorkerRequestTool is an explicit host attestation of a reviewed
// worker transport's request-local join contract. The SDK loader calls it only
// for its exact owned rpcProcess; generic callers/wrappers must use Register
// without this constructor. It captures receiver methods and immutable schema,
// grants no trusted effects, and cannot convert unknown delivery into success.
func NewOwnedWorkerRequestTool(definition ToolDefinition, executor WorkerRequestExecutor, lease PreparedDispatchLease) (ToolDefinition, error) {
	if definition == nil || executor == nil || isNilPreparedDispatchLease(lease) {
		return nil, errors.New("tools: owned worker requires definition, exact executor and generation lease")
	}
	definitionValue := reflect.ValueOf(definition)
	if (definitionValue.Kind() == reflect.Pointer || definitionValue.Kind() == reflect.Interface) && definitionValue.IsNil() {
		return nil, errors.New("tools: nil owned worker definition")
	}
	if definition.Name() == "" {
		return nil, errors.New("tools: owned worker requires a tool name")
	}
	value := reflect.ValueOf(executor)
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil() {
		return nil, errors.New("tools: nil owned worker executor")
	}
	parameterized, ok := definition.(Parameterized)
	if !ok {
		return nil, errors.New("tools: owned worker requires a declared schema")
	}
	schema, err := freezeToolSchema(parameterized.Parameters())
	if err != nil {
		return nil, err
	}
	return &ownedWorkerRequestTool{name: definition.Name(), description: definition.Description(), schema: schema, execute: executor.ExecuteWorkerTool, lease: frozenWorkerRequestLease{acquire: lease.Acquire}}, nil
}

func (t *ownedWorkerRequestTool) Name() string        { return t.name }
func (t *ownedWorkerRequestTool) Description() string { return t.description }
func (t *ownedWorkerRequestTool) Parameters() any     { return t.schema.Schema() }
func (t *ownedWorkerRequestTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	return t.execute(ctx, call)
}

var _ ToolDefinition = (*ownedWorkerRequestTool)(nil)
var _ Parameterized = (*ownedWorkerRequestTool)(nil)
