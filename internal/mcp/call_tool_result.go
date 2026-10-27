package mcp

import (
	"encoding/json"
	"fmt"
	"math"
)

// validateCallToolResultFieldEncoding runs before JSON objects become maps, so
// duplicate and case-aliased protocol fields cannot disappear during decoding.
// Required method fields are checked separately by decodeCallToolResult.
func validateCallToolResultFieldEncoding(raw json.RawMessage) error {
	if err := validateJSONRPCObjectFields(raw, "result object", "content", "isError", "structuredContent", "_meta"); err != nil {
		return err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(result["content"], &blocks); err != nil {
		return nil
	}
	for _, block := range blocks {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(block, &object); err != nil || object == nil {
			continue
		}
		if err := validateJSONRPCObjectFields(block, "content block", "type", "text", "data", "mimeType", "resource", "uri", "name", "title", "description", "size", "annotations", "_meta"); err != nil {
			return err
		}
		if resource, ok := object["resource"]; ok {
			var resourceObject map[string]json.RawMessage
			if err := json.Unmarshal(resource, &resourceObject); err == nil && resourceObject != nil {
				if err := validateJSONRPCObjectFields(resource, "embedded resource", "uri", "mimeType", "text", "blob", "_meta"); err != nil {
					return err
				}
			}
		}
		if annotations, ok := object["annotations"]; ok {
			var annotationObject map[string]json.RawMessage
			if err := json.Unmarshal(annotations, &annotationObject); err == nil && annotationObject != nil {
				if err := validateJSONRPCObjectFields(annotations, "content annotations", "audience", "priority", "lastModified"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func decodeCallToolResult(result map[string]any) (*MCPToolResult, error) {
	invalid := func(reason string) (*MCPToolResult, error) {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: invalid tools/call result: %s", reason), true, false)
	}
	content, ok := result["content"].([]any)
	if !ok {
		return invalid("content must be an array")
	}
	isError := false
	if value, present := result["isError"]; present {
		var valid bool
		isError, valid = value.(bool)
		if !valid {
			return invalid("isError must be a boolean")
		}
	}
	if value, present := result["structuredContent"]; present {
		if object, ok := value.(map[string]any); !ok || object == nil {
			return invalid("structuredContent must be an object")
		}
	}
	if err := validateCallToolMetadata(result); err != nil {
		return invalid(err.Error())
	}
	for _, value := range content {
		block, ok := value.(map[string]any)
		if !ok || block == nil {
			return invalid("content entries must be objects")
		}
		if err := validateCallToolContentBlock(block); err != nil {
			return invalid(err.Error())
		}
	}
	return &MCPToolResult{Content: content, IsError: isError}, nil
}

func validateCallToolContentBlock(block map[string]any) error {
	requireString := func(object map[string]any, key string) error {
		if _, ok := object[key].(string); !ok {
			return fmt.Errorf("%s must be a string", key)
		}
		return nil
	}
	kind, ok := block["type"].(string)
	if !ok {
		return fmt.Errorf("content type must be a string")
	}
	switch kind {
	case "text":
		if err := requireString(block, "text"); err != nil {
			return err
		}
	case "image", "audio":
		if err := requireString(block, "data"); err != nil {
			return err
		}
		if err := requireString(block, "mimeType"); err != nil {
			return err
		}
	case "resource_link":
		if err := requireString(block, "uri"); err != nil {
			return err
		}
		if err := requireString(block, "name"); err != nil {
			return err
		}
		for _, key := range []string{"title", "description"} {
			if _, present := block[key]; present {
				if err := requireString(block, key); err != nil {
					return err
				}
			}
		}
		// The negotiated Resource schema and official SDK define size as a
		// number, without an integer or minimum constraint.
		if size, present := block["size"]; present {
			if _, valid := callToolFiniteNumber(size); !valid {
				return fmt.Errorf("resource size must be a finite number")
			}
		}
	case "resource":
		resource, ok := block["resource"].(map[string]any)
		if !ok || resource == nil {
			return fmt.Errorf("resource must be an object")
		}
		if err := requireString(resource, "uri"); err != nil {
			return err
		}
		_, text := resource["text"]
		_, blob := resource["blob"]
		if text == blob {
			return fmt.Errorf("resource must contain exactly one of text or blob")
		}
		if text {
			if err := requireString(resource, "text"); err != nil {
				return err
			}
		}
		if blob {
			if err := requireString(resource, "blob"); err != nil {
				return err
			}
		}
		if err := validateCallToolMetadata(resource); err != nil {
			return err
		}
		if _, present := resource["mimeType"]; present {
			if err := requireString(resource, "mimeType"); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported content type %q", kind)
	}
	if _, present := block["mimeType"]; present {
		if err := requireString(block, "mimeType"); err != nil {
			return err
		}
	}
	if err := validateCallToolMetadata(block); err != nil {
		return err
	}
	if annotations, present := block["annotations"]; present {
		if err := validateCallToolAnnotations(annotations); err != nil {
			return err
		}
	}
	return nil
}

func validateCallToolMetadata(object map[string]any) error {
	if metadata, present := object["_meta"]; present {
		if object, valid := metadata.(map[string]any); !valid || object == nil {
			return fmt.Errorf("_meta must be an object")
		}
	}
	return nil
}

// Validate only fields defined by the negotiated 2025-06-18 Annotations schema;
// vendor fields and future metadata contents remain open.
func validateCallToolAnnotations(value any) error {
	annotations, valid := value.(map[string]any)
	if !valid || annotations == nil {
		return fmt.Errorf("annotations must be an object")
	}
	if audience, present := annotations["audience"]; present {
		roles, valid := audience.([]any)
		if !valid {
			return fmt.Errorf("annotations audience must be an array")
		}
		for _, value := range roles {
			role, valid := value.(string)
			if !valid || (role != "user" && role != "assistant") {
				return fmt.Errorf("annotations audience must contain user or assistant roles")
			}
		}
	}
	if priority, present := annotations["priority"]; present {
		number, valid := callToolFiniteNumber(priority)
		if !valid || number < 0 || number > 1 {
			return fmt.Errorf("annotations priority must be a number between 0 and 1")
		}
	}
	if modified, present := annotations["lastModified"]; present {
		if _, valid := modified.(string); !valid {
			return fmt.Errorf("annotations lastModified must be a string")
		}
	}
	return nil
}

func callToolFiniteNumber(value any) (float64, bool) {
	number, valid := value.(float64)
	return number, valid && !math.IsNaN(number) && !math.IsInf(number, 0)
}
