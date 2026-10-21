package extensionapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestExtensionAPIGoldenCompatibility(t *testing.T) {
	const golden = `{"id":"example","version":"1","host_api":"^1.0","capabilities":["tools"],"tools":[{"name":"echo","description":"Echo","parameters":{"type":"object"}}]}`
	var manifest Manifest
	if err := json.Unmarshal([]byte(golden), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != golden {
		t.Fatalf("wire changed:\n%s", data)
	}
	manifest.HostAPI = "2"
	if manifest.Validate() == nil {
		t.Fatal("unsupported API accepted")
	}
	manifest.HostAPI = "1"
	manifest.Capabilities = []string{"unknown"}
	if manifest.Validate() == nil {
		t.Fatal("unknown capability accepted")
	}
}

func TestServeHandshakeAndBoundedFrames(t *testing.T) {
	manifest := Manifest{ID: "example", Version: "1", HostAPI: Version, Capabilities: []string{"tools"}}
	input := strings.NewReader("{\"id\":1,\"method\":\"initialize\",\"params\":{\"version\":\"1.0\"}}\n{\"id\":2,\"method\":\"tool.call\"}\n")
	var output bytes.Buffer
	err := Serve(context.Background(), input, &output, manifest, HandlerFunc(func(context.Context, Request) (json.RawMessage, error) {
		return json.RawMessage(`{"output":"ok"}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id":2,"result":{"output":"ok"}`) {
		t.Fatal(output.String())
	}
	err = Serve(context.Background(), strings.NewReader(strings.Repeat("x", MaxFrameBytes+1)), &output, manifest, HandlerFunc(func(context.Context, Request) (json.RawMessage, error) { return nil, nil }))
	if err == nil {
		t.Fatal("oversize input accepted")
	}
}
