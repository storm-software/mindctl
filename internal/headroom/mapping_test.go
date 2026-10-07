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

func TestEligibleMessagesMapsTextAndProtectsOpaqueContent(t *testing.T) {
	request := inference.Request{Input: []inference.Item{
		{Type: "message", Role: "user", Content: []inference.ContentPart{{Type: "input_text", Text: "first"}, {Type: "input_text", Text: "second"}}},
		{Type: "message", Role: "assistant", Content: []inference.ContentPart{{Type: "output_text", Text: "assistant report"}}},
		{Type: "message", Role: "assistant", Text: "native", ProviderData: json.RawMessage(`{"opaque":true}`)},
		{Type: "message", Role: "user", Content: []inference.ContentPart{{Type: "input_text", Text: "safe"}, {Type: "input_image", ImageURL: json.RawMessage(`{"url":"secret"}`)}}},
		{Type: "reasoning", EncryptedContent: json.RawMessage(`"opaque"`)},
		{Type: "function_call_output", CallID: "call-1", Output: json.RawMessage(`"verbose tool output"`)},
		{Type: "function_call_output", CallID: "call-2", Output: json.RawMessage(`{"structured":"opaque"}`)},
		{Type: "custom_tool_call_output", CallID: "call-3", Output: json.RawMessage(`"custom output"`)},
	}}
	messages, ok := eligibleMessages(request)
	if !ok || len(messages) != 5 || messages[0].Role != "user" || messages[0].Content != "first" || messages[1].Content != "second" || messages[2].Role != "assistant" || messages[2].Content != "assistant report" || messages[3].Role != "tool" || messages[3].Content != "verbose tool output" || messages[3].ToolCallID != "call-1" || messages[4].Content != "custom output" {
		t.Fatalf("messages = %+v, eligible = %v", messages, ok)
	}
	messages[2].Content = "short report"
	messages[3].Content = "short result"
	messages[4].Content = "short custom"
	if err := applyMessages(&request, messages); err != nil {
		t.Fatal(err)
	}
	if request.Input[0].Content[0].Text != "first" || request.Input[0].Content[1].Text != "second" || request.Input[1].Content[0].Text != "short report" || string(request.Input[5].Output) != `"short result"` || string(request.Input[7].Output) != `"short custom"` || request.Input[2].Text != "native" || request.Input[3].Content[0].Text != "safe" {
		t.Fatalf("request changed unsafely: %+v", request.Input)
	}
}

func TestApplyMessagesRejectsReorderedOrChangedIdentity(t *testing.T) {
	request := inference.Request{Input: []inference.Item{{Type: "message", Role: "assistant", Text: "one"}, {Type: "function_call_output", CallID: "call-1", Output: json.RawMessage(`"two"`)}}}
	messages, _ := eligibleMessages(request)
	for _, changed := range [][]compressionMessage{
		{messages[1], messages[0]},
		{{Role: "user", Content: "one"}, messages[1]},
		{messages[0], {Role: "tool", ToolCallID: "other", Content: "two"}},
	} {
		if err := applyMessages(&request, changed); err == nil {
			t.Fatalf("accepted changed messages: %+v", changed)
		}
	}
}

func TestApplyMessagesPreservesUserText(t *testing.T) {
	request := inference.Request{Input: []inference.Item{{Type: "message", Role: "user", Text: "protected instructions"}}}
	if err := applyMessages(&request, []compressionMessage{{Role: "user", Content: "altered instructions"}}); err == nil || request.Input[0].Text != "protected instructions" {
		t.Fatalf("user text changed: %+v, err=%v", request.Input, err)
	}
}
