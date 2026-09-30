package cli

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
)

const (
	hookPayloadMaxDepth = 64
	hookPayloadMaxNodes = 65536
	hookPayloadMaxBytes = 32 * 1024 * 1024
)

var hookErrorType = reflect.TypeOf((*error)(nil)).Elem()

type hookPayloadBudget struct {
	nodes int
	bytes int
}

func (b *hookPayloadBudget) node(depth int) error {
	if depth > hookPayloadMaxDepth || b.nodes >= hookPayloadMaxNodes {
		return extension.ErrHookPayloadLimitExceeded
	}
	b.nodes++
	return nil
}

func (b *hookPayloadBudget) string(depth int, value string) error {
	if err := b.node(depth); err != nil {
		return err
	}
	if len(value) > hookPayloadMaxBytes-b.bytes {
		return extension.ErrHookPayloadLimitExceeded
	}
	b.bytes += len(value)
	return nil
}

func (b *hookPayloadBudget) boolean(depth int) error { return b.node(depth) }

func buildBeforeRunHookEvent(owner string, submission core.Submission) (extension.HookEvent, error) {
	event := extension.HookEvent{Name: "agent.before_run", Source: owner, Timestamp: time.Now()}
	budget := &hookPayloadBudget{}
	if err := beginHookEventBudget(budget, event); err != nil {
		return extension.HookEvent{}, err
	}
	snapshot, err := buildBeforeRunSnapshot(submission, budget, 2)
	if err != nil {
		return extension.HookEvent{}, err
	}
	event.Data = snapshot
	return cloneHookEvent(event)
}

func buildAfterRunHookEvent(ctx context.Context, owner string, submission core.Submission, result core.Result, runErr error) (extension.HookEvent, error) {
	event := extension.HookEvent{Name: "agent.after_run", Source: owner, Timestamp: time.Now()}
	budget := &hookPayloadBudget{}
	if err := beginHookEventBudget(budget, event); err != nil {
		return extension.HookEvent{}, err
	}
	snapshot, err := buildAfterRunSnapshot(ctx, submission, result, runErr, budget, 2)
	if err != nil {
		return extension.HookEvent{}, err
	}
	event.Data = snapshot
	return cloneHookEvent(event)
}

func beginHookEventBudget(budget *hookPayloadBudget, event extension.HookEvent) error {
	if err := budget.node(1); err != nil { // HookEvent root.
		return err
	}
	if err := budget.string(2, event.Name); err != nil {
		return err
	}
	if err := budget.string(2, event.Source); err != nil {
		return err
	}
	return budget.node(2) // Timestamp is a fixed scalar value.
}

func buildBeforeRunSnapshot(submission core.Submission, budget *hookPayloadBudget, depth int) (extension.HookBeforeRunSnapshot, error) {
	if err := budget.node(depth); err != nil {
		return extension.HookBeforeRunSnapshot{}, err
	}
	snapshot, err := buildHookSubmissionSnapshot(submission, budget, depth+1)
	if err != nil {
		return extension.HookBeforeRunSnapshot{}, err
	}
	return extension.HookBeforeRunSnapshot{Submission: snapshot}, nil
}

func buildAfterRunSnapshot(ctx context.Context, submission core.Submission, result core.Result, runErr error, budget *hookPayloadBudget, depth int) (extension.HookAfterRunSnapshot, error) {
	if err := budget.node(depth); err != nil {
		return extension.HookAfterRunSnapshot{}, err
	}
	submissionSnapshot, err := buildHookSubmissionSnapshot(submission, budget, depth+1)
	if err != nil {
		return extension.HookAfterRunSnapshot{}, err
	}
	runSnapshot, err := buildHookRunSnapshot(ctx, result, budget, depth+1)
	if err != nil {
		return extension.HookAfterRunSnapshot{}, err
	}
	errorSnapshot := classifyHookError(runErr)
	if err := buildHookErrorSnapshot(budget, depth+1, errorSnapshot); err != nil {
		return extension.HookAfterRunSnapshot{}, err
	}
	return extension.HookAfterRunSnapshot{Submission: submissionSnapshot, Run: runSnapshot, Error: errorSnapshot}, nil
}

func buildHookSubmissionSnapshot(submission core.Submission, budget *hookPayloadBudget, depth int) (extension.HookSubmissionSnapshot, error) {
	if err := budget.node(depth); err != nil {
		return extension.HookSubmissionSnapshot{}, err
	}
	snapshot := extension.HookSubmissionSnapshot{Type: extension.HookSubmissionType(submission.Type.String()), Content: submission.Content}
	if err := budget.string(depth+1, string(snapshot.Type)); err != nil {
		return extension.HookSubmissionSnapshot{}, err
	}
	if err := budget.string(depth+1, snapshot.Content); err != nil {
		return extension.HookSubmissionSnapshot{}, err
	}
	metadata, err := cloneJSONPayload(reflect.ValueOf(submission.Metadata), budget, depth+1, make(map[hookContainerID]struct{}))
	if err != nil {
		return extension.HookSubmissionSnapshot{}, err
	}
	if metadata != nil {
		var ok bool
		snapshot.Metadata, ok = metadata.(map[string]any)
		if !ok {
			return extension.HookSubmissionSnapshot{}, extension.ErrHookPayloadUnsupported
		}
	}
	if submission.History != nil {
		snapshot.Messages = make([]extension.HookMessageSnapshot, len(submission.History))
	}
	if err := budget.node(depth + 1); err != nil { // Messages slice, including nil.
		return extension.HookSubmissionSnapshot{}, err
	}
	for index, message := range submission.History {
		copied, err := buildHookMessageSnapshot(message, budget, depth+2)
		if err != nil {
			return extension.HookSubmissionSnapshot{}, err
		}
		snapshot.Messages[index] = copied
	}
	return snapshot, nil
}

func buildHookMessageSnapshot(message core.AgentMessage, budget *hookPayloadBudget, depth int) (extension.HookMessageSnapshot, error) {
	if err := budget.node(depth); err != nil {
		return extension.HookMessageSnapshot{}, err
	}
	snapshot := extension.HookMessageSnapshot{
		Role: message.Role, Content: message.Content,
		ToolCallID: message.ToolCallID, ToolName: message.ToolName,
		InputKind: message.InputKind, ReceiptOutcome: message.ReceiptOutcome,
	}
	for _, value := range []string{snapshot.Role, snapshot.Content, snapshot.ToolCallID, snapshot.ToolName, snapshot.InputKind, snapshot.ReceiptOutcome} {
		if err := budget.string(depth+1, value); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
	}
	if message.ContentBlocks != nil {
		snapshot.ContentBlocks = make([]extension.HookContentBlockSnapshot, len(message.ContentBlocks))
	}
	if err := budget.node(depth + 1); err != nil {
		return extension.HookMessageSnapshot{}, err
	}
	for index, block := range message.ContentBlocks {
		if err := budget.node(depth + 2); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		copied := extension.HookContentBlockSnapshot{Type: block.Type, Text: block.Text}
		if err := budget.string(depth+3, copied.Type); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		if err := budget.string(depth+3, copied.Text); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		if block.ImageURL != nil {
			if err := budget.node(depth + 3); err != nil {
				return extension.HookMessageSnapshot{}, err
			}
			if err := budget.node(depth + 4); err != nil {
				return extension.HookMessageSnapshot{}, err
			}
			copied.ImageURL = &extension.HookImageURLSnapshot{URL: block.ImageURL.URL, Detail: block.ImageURL.Detail}
			if err := budget.string(depth+5, copied.ImageURL.URL); err != nil {
				return extension.HookMessageSnapshot{}, err
			}
			if err := budget.string(depth+5, copied.ImageURL.Detail); err != nil {
				return extension.HookMessageSnapshot{}, err
			}
		} else if err := budget.node(depth + 3); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		snapshot.ContentBlocks[index] = copied
	}
	if message.Usage == nil {
		if err := budget.node(depth + 1); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
	} else {
		if err := budget.node(depth + 1); err != nil { // Optional pointer field.
			return extension.HookMessageSnapshot{}, err
		}
		if err := budget.node(depth + 2); err != nil { // Usage value.
			return extension.HookMessageSnapshot{}, err
		}
		usage := &extension.HookUsageSnapshot{
			InputTokens:  message.Usage.InputTokens,
			OutputTokens: message.Usage.OutputTokens,
			TotalTokens:  message.Usage.TotalTokens,
		}
		for range 3 {
			if err := budget.node(depth + 3); err != nil {
				return extension.HookMessageSnapshot{}, err
			}
		}
		snapshot.Usage = usage
	}
	if message.ToolCalls != nil {
		snapshot.ToolCalls = make([]extension.HookToolCallSnapshot, len(message.ToolCalls))
	}
	if err := budget.node(depth + 1); err != nil {
		return extension.HookMessageSnapshot{}, err
	}
	for index, call := range message.ToolCalls {
		if err := budget.node(depth + 2); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		copied := extension.HookToolCallSnapshot{ID: call.ID, Name: call.Name}
		if err := budget.string(depth+3, copied.ID); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		if err := budget.string(depth+3, copied.Name); err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		args, err := cloneJSONPayload(reflect.ValueOf(call.Args), budget, depth+3, make(map[hookContainerID]struct{}))
		if err != nil {
			return extension.HookMessageSnapshot{}, err
		}
		copied.Args = args
		snapshot.ToolCalls[index] = copied
	}
	return snapshot, nil
}

func buildHookRunSnapshot(ctx context.Context, result core.Result, budget *hookPayloadBudget, depth int) (extension.HookRunSnapshot, error) {
	if err := budget.node(depth); err != nil {
		return extension.HookRunSnapshot{}, err
	}
	runID := core.RunIDFromContext(ctx)
	snapshot := extension.HookRunSnapshot{
		Message: result.Message, Success: result.Success,
		RunID: runID, RunIDAvailable: runID != "",
		Outcome: extension.HookRunOutcome(result.Outcome), OutcomeAvailable: result.Outcome != "",
		ExecutionState: extension.HookRunExecutionState(result.ExecutionState), ExecutionStateAvailable: result.ExecutionState != "",
	}
	if err := budget.string(depth+1, snapshot.Message); err != nil {
		return extension.HookRunSnapshot{}, err
	}
	if err := budget.boolean(depth + 1); err != nil {
		return extension.HookRunSnapshot{}, err
	}
	for _, value := range []string{snapshot.RunID, string(snapshot.Outcome), string(snapshot.ExecutionState)} {
		if err := budget.string(depth+1, value); err != nil {
			return extension.HookRunSnapshot{}, err
		}
	}
	for _, value := range []bool{snapshot.RunIDAvailable, snapshot.OutcomeAvailable, snapshot.ExecutionStateAvailable} {
		if err := budget.boolean(depth + 1); err != nil {
			return extension.HookRunSnapshot{}, err
		}
		_ = value
	}
	return snapshot, nil
}

func classifyHookError(err error) extension.HookErrorSnapshot {
	if err == nil {
		return extension.HookErrorSnapshot{}
	}
	class := extension.HookErrorClassUnknown
	switch {
	case errors.Is(err, context.Canceled):
		class = extension.HookErrorClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		class = extension.HookErrorClassDeadline
	case errors.Is(err, extension.ErrHookBlocked):
		class = extension.HookErrorClassBlocked
	case errors.Is(err, extension.ErrUnsupportedHookAction):
		class = extension.HookErrorClassUnsupportedAction
	case errors.Is(err, extension.ErrHookPayloadUnsupported), errors.Is(err, extension.ErrHookPayloadLimitExceeded):
		class = extension.HookErrorClassUnsupported
	default:
		var harnessErr *core.AgentHarnessError
		if errors.As(err, &harnessErr) && harnessErr != nil {
			switch harnessErr.Code {
			case core.ErrCodeHookRejected:
				class = extension.HookErrorClassBlocked
			case core.ErrCodeTimeout:
				class = extension.HookErrorClassDeadline
			}
		}
	}
	return extension.HookErrorSnapshot{Present: true, Class: class}
}

func buildHookErrorSnapshot(budget *hookPayloadBudget, depth int, snapshot extension.HookErrorSnapshot) error {
	if err := budget.node(depth); err != nil {
		return err
	}
	if err := budget.boolean(depth + 1); err != nil {
		return err
	}
	return budget.string(depth+1, string(snapshot.Class))
}

func cloneHookEvent(event extension.HookEvent) (extension.HookEvent, error) {
	budget := &hookPayloadBudget{}
	if err := budget.node(1); err != nil {
		return extension.HookEvent{}, err
	}
	if err := budget.string(2, event.Name); err != nil {
		return extension.HookEvent{}, err
	}
	data, err := cloneHookSnapshot(reflect.ValueOf(event.Data), budget, 2, make(map[hookContainerID]struct{}))
	if err != nil {
		return extension.HookEvent{}, err
	}
	if err := budget.string(2, event.Source); err != nil {
		return extension.HookEvent{}, err
	}
	if err := budget.node(2); err != nil {
		return extension.HookEvent{}, err
	}
	return extension.HookEvent{Name: event.Name, Data: data.Interface(), Source: event.Source, Timestamp: event.Timestamp}, nil
}

func cloneHookSnapshot(value reflect.Value, budget *hookPayloadBudget, depth int, stack map[hookContainerID]struct{}) (reflect.Value, error) {
	if !value.IsValid() {
		return reflect.Value{}, extension.ErrHookPayloadUnsupported
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return reflect.Zero(value.Type()), budget.node(depth)
		}
		child, err := cloneHookSnapshot(value.Elem(), budget, depth, stack)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type()).Elem()
		result.Set(child)
		return result, nil
	}
	if value.Type().Implements(hookErrorType) {
		return reflect.Value{}, extension.ErrHookPayloadUnsupported
	}
	switch value.Kind() {
	case reflect.String:
		if err := budget.string(depth, value.String()); err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type()).Elem()
		result.SetString(value.String())
		return result, nil
	case reflect.Bool:
		if err := budget.boolean(depth); err != nil {
			return reflect.Value{}, err
		}
		return value, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		if value.Kind() == reflect.Float32 || value.Kind() == reflect.Float64 {
			f := value.Float()
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return reflect.Value{}, extension.ErrHookPayloadUnsupported
			}
		}
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		return value, nil
	case reflect.Struct:
		if !isHookSnapshotStruct(value.Type()) {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.NumField(); index++ {
			field, err := cloneHookSnapshot(value.Field(index), budget, depth+1, stack)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Field(index).Set(field)
		}
		return result, nil
	case reflect.Pointer:
		if !isHookSnapshotPointer(value.Type()) {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		child, err := cloneHookSnapshot(value.Elem(), budget, depth+1, stack)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(child)
		return result, nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		id := hookContainerID{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind()}
		if _, exists := stack[id]; exists {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		stack[id] = struct{}{}
		defer delete(stack, id)
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key, err := cloneHookSnapshot(iter.Key(), budget, depth+1, stack)
			if err != nil {
				return reflect.Value{}, err
			}
			item, err := cloneHookSnapshot(iter.Value(), budget, depth+1, stack)
			if err != nil {
				return reflect.Value{}, err
			}
			result.SetMapIndex(key, item)
		}
		return result, nil
	case reflect.Slice:
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		id := hookContainerID{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind()}
		if _, exists := stack[id]; exists {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		stack[id] = struct{}{}
		defer delete(stack, id)
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			item, err := cloneHookSnapshot(value.Index(index), budget, depth+1, stack)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(index).Set(item)
		}
		return result, nil
	default:
		return reflect.Value{}, extension.ErrHookPayloadUnsupported
	}
}

func cloneJSONPayload(value reflect.Value, budget *hookPayloadBudget, depth int, stack map[hookContainerID]struct{}) (any, error) {
	cloned, err := cloneJSONValue(value, budget, depth, stack)
	if err != nil || !cloned.IsValid() {
		return nil, err
	}
	return cloned.Interface(), nil
}

func cloneJSONValue(value reflect.Value, budget *hookPayloadBudget, depth int, stack map[hookContainerID]struct{}) (reflect.Value, error) {
	if !value.IsValid() {
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		return reflect.Value{}, nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			if err := budget.node(depth); err != nil {
				return reflect.Value{}, err
			}
			return reflect.Value{}, nil
		}
		return cloneJSONValue(value.Elem(), budget, depth, stack)
	}
	if value.Type().Implements(hookErrorType) {
		return reflect.Value{}, extension.ErrHookPayloadUnsupported
	}
	switch value.Kind() {
	case reflect.String:
		if err := budget.string(depth, value.String()); err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(value.String()), nil
	case reflect.Bool:
		if err := budget.boolean(depth); err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(value.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(value.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(value.Uint()), nil
	case reflect.Float32, reflect.Float64:
		f := value.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(f), nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		if value.IsNil() {
			return reflect.ValueOf(map[string]any(nil)), nil
		}
		id := hookContainerID{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind()}
		if _, exists := stack[id]; exists {
			return reflect.Value{}, extension.ErrHookPayloadUnsupported
		}
		stack[id] = struct{}{}
		defer delete(stack, id)
		result := make(map[string]any, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key()
			if key.Type().Implements(hookErrorType) {
				return reflect.Value{}, extension.ErrHookPayloadUnsupported
			}
			if err := budget.string(depth+1, key.String()); err != nil {
				return reflect.Value{}, err
			}
			item, err := cloneJSONValue(iter.Value(), budget, depth+1, stack)
			if err != nil {
				return reflect.Value{}, err
			}
			if !item.IsValid() {
				result[key.String()] = nil
			} else {
				result[key.String()] = item.Interface()
			}
		}
		return reflect.ValueOf(result), nil
	case reflect.Slice, reflect.Array:
		if err := budget.node(depth); err != nil {
			return reflect.Value{}, err
		}
		if value.Kind() == reflect.Slice && value.IsNil() {
			return reflect.ValueOf([]any(nil)), nil
		}
		if value.Kind() == reflect.Slice {
			id := hookContainerID{typ: value.Type(), ptr: uintptr(value.UnsafePointer()), kind: value.Kind()}
			if _, exists := stack[id]; exists {
				return reflect.Value{}, extension.ErrHookPayloadUnsupported
			}
			stack[id] = struct{}{}
			defer delete(stack, id)
		}
		result := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			item, err := cloneJSONValue(value.Index(index), budget, depth+1, stack)
			if err != nil {
				return reflect.Value{}, err
			}
			if item.IsValid() {
				result[index] = item.Interface()
			}
		}
		return reflect.ValueOf(result), nil
	default:
		return reflect.Value{}, extension.ErrHookPayloadUnsupported
	}
}

type hookContainerID struct {
	typ  reflect.Type
	ptr  uintptr
	kind reflect.Kind
}

func translateExtensionHookResult(result extension.HookResult) error {
	if result.Replacement != nil {
		return extension.ErrUnsupportedHookAction
	}
	switch result.Action {
	case extension.HookActionPass:
		return nil
	case extension.HookActionBlock:
		return extension.ErrHookBlocked
	default:
		return extension.ErrUnsupportedHookAction
	}
}

func isHookSnapshotStruct(value reflect.Type) bool {
	switch value {
	case reflect.TypeOf(extension.HookBeforeRunSnapshot{}), reflect.TypeOf(extension.HookAfterRunSnapshot{}),
		reflect.TypeOf(extension.HookSubmissionSnapshot{}), reflect.TypeOf(extension.HookMessageSnapshot{}),
		reflect.TypeOf(extension.HookContentBlockSnapshot{}), reflect.TypeOf(extension.HookImageURLSnapshot{}),
		reflect.TypeOf(extension.HookUsageSnapshot{}), reflect.TypeOf(extension.HookToolCallSnapshot{}),
		reflect.TypeOf(extension.HookRunSnapshot{}), reflect.TypeOf(extension.HookErrorSnapshot{}):
		return true
	default:
		return false
	}
}

func isHookSnapshotPointer(value reflect.Type) bool {
	return value == reflect.TypeOf((*extension.HookImageURLSnapshot)(nil)) || value == reflect.TypeOf((*extension.HookUsageSnapshot)(nil))
}
