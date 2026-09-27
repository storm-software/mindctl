package headroom

import (
	"encoding/json"
	"testing"

	"github.com/storm-software/mindctl/internal/inference"
)

// A nil json.RawMessage encodes as null, which decodes to a non-empty literal.
// Adapters treat non-empty raw fields as present, so an Anthropic tool without
// native fields was forwarded as a null tool.
func TestCloneRequestKeepsEmptyRawFieldsEmpty(t *testing.T) {
	request := inference.Request{
		Input: []inference.Item{{Type: "message", Role: "user", Text: "hi", Content: []inference.ContentPart{{Type: "input_text", Text: "hi"}}}},
		Tools: []inference.Tool{
			{Type: "function", Name: "Bash", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Type: "function", Name: "Mcp", Parameters: json.RawMessage(`{"type":"object"}`), AnthropicNative: json.RawMessage(`{"name":"Mcp","defer_loading":true}`)},
		},
	}
	cloned, err := cloneRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	plain := cloned.Tools[0]
	if plain.AnthropicNative != nil || plain.CacheControl != nil {
		t.Errorf("plain tool gained raw fields: native=%q cache_control=%q", plain.AnthropicNative, plain.CacheControl)
	}
	if string(cloned.Tools[1].AnthropicNative) != `{"name":"Mcp","defer_loading":true}` {
		t.Errorf("native tool = %q", cloned.Tools[1].AnthropicNative)
	}
	item := cloned.Input[0]
	for name, raw := range map[string]json.RawMessage{
		"ImageURL": item.ImageURL, "Arguments": item.Arguments, "Output": item.Output,
		"Summary": item.Summary, "EncryptedContent": item.EncryptedContent,
		"ProviderData": item.ProviderData, "Content.ImageURL": item.Content[0].ImageURL,
	} {
		if raw != nil {
			t.Errorf("item %s = %q, want nil", name, raw)
		}
	}
}
