package core

import (
	"reflect"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// cloneAgentMessages returns an independent copy of the canonical history
// values. Tool arguments are JSON value graphs, so maps and arrays are copied
// recursively while immutable scalar values retain their original types.
func cloneAgentMessages(messages []AgentMessage) []AgentMessage {
	if messages == nil {
		return nil
	}

	cloned := make([]AgentMessage, len(messages))
	for i, message := range messages {
		cloned[i] = message
		if message.ContentBlocks != nil {
			cloned[i].ContentBlocks = append([]llm.ContentBlock(nil), message.ContentBlocks...)
			for j, block := range cloned[i].ContentBlocks {
				if block.ImageURL != nil {
					imageURL := *block.ImageURL
					cloned[i].ContentBlocks[j].ImageURL = &imageURL
				}
			}
		}
		if message.Usage != nil {
			usage := *message.Usage
			cloned[i].Usage = &usage
		}
		if message.ToolCalls != nil {
			cloned[i].ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
			for j := range cloned[i].ToolCalls {
				cloned[i].ToolCalls[j].Args = cloneProtocolJSONValue(message.ToolCalls[j].Args)
			}
		}
	}
	return cloned
}

func cloneUsage(usage *llm.Usage) *llm.Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

// cloneProtocolJSONValue copies the maps and arrays used by JSON-decoded tool
// arguments. It deliberately leaves arbitrary structs and pointers untouched:
// protocol arguments are JSON values, not general Go object graphs.
func cloneProtocolJSONValue(value any) any {
	if value == nil {
		return nil
	}
	cloned := cloneJSONValue(reflect.ValueOf(value))
	if !cloned.IsValid() {
		return nil
	}
	return cloned.Interface()
}

func cloneJSONValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clonedElement := cloneJSONValue(value.Elem())
		cloned := reflect.New(value.Type()).Elem()
		if clonedElement.IsValid() && clonedElement.Type().AssignableTo(value.Type()) {
			cloned.Set(clonedElement)
		} else if clonedElement.IsValid() && clonedElement.Type().AssignableTo(value.Elem().Type()) {
			cloned.Set(clonedElement)
		}
		return cloned
	case reflect.Map:
		if value.IsNil() || value.Type().Key().Kind() != reflect.String {
			return value
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			cloned.SetMapIndex(iterator.Key(), cloneJSONValue(iterator.Value()))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneJSONValue(value.Index(i)))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneJSONValue(value.Index(i)))
		}
		return cloned
	default:
		return value
	}
}
