package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
)

func TestMessagesSSEWriterLifecycle(t *testing.T) {
	response := httptest.NewRecorder()
	writer := &messagesSSEWriter{response: response}
	if err := writer.Start(executor.StreamMetadata{ResponseID: "msg_1", Model: "claude-test", Provider: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []inference.Event{
		{Type: "message_start", Usage: inference.Usage{InputTokens: 4, OutputTokens: 0, Known: true}},
		{Type: "content_block_start", ItemType: "text", ItemID: "0"},
		{Type: "content_block_delta", Delta: "hello", ItemID: "0"},
		{Type: "content_block_stop", ItemID: "0"},
		{Type: "content_block_start", ItemType: "tool_use", ItemID: "toolu_1", CallID: "toolu_1", Name: "run"},
		{Type: "content_block_delta", ArgumentsDelta: `{"q":`, ItemID: "toolu_1"},
		{Type: "content_block_delta", ArgumentsDelta: `"x"}`, ItemID: "toolu_1"},
		{Type: "content_block_stop", ItemID: "toolu_1"},
		{Type: "response.completed", StopReason: "tool_use", Usage: inference.Usage{InputTokens: 4, OutputTokens: 6, Known: true}},
	} {
		if err := writer.WriteEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	stream := response.Body.String()
	if response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type=%q", response.Header().Get("Content-Type"))
	}
	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(stream, "event: "+event+"\ndata: ") {
			t.Fatalf("missing %s in %s", event, stream)
		}
	}
	if !strings.Contains(stream, `"partial_json":"{\"q\":"`) || !strings.Contains(stream, `"stop_reason":"tool_use"`) {
		t.Fatalf("lost tool fragment/stop reason: %s", stream)
	}
}

func TestMessagesSSEWriterCancellationAndTerminalError(t *testing.T) {
	response := httptest.NewRecorder()
	writer := &messagesSSEWriter{response: response}
	_ = writer.Start(executor.StreamMetadata{ResponseID: "msg_1", Model: "gpt-test", Provider: "openai"})
	if err := writer.WriteEvent(context.Background(), inference.Event{Type: "response.output_text.delta", Delta: "partial", Data: []byte(`{"secret":"native"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTerminalError(context.Background(), errors.New("private upstream error")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Body.String(), "native") || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "event: message_stop") || !strings.Contains(response.Body.String(), "event: error") {
		t.Fatalf("unsafe terminal SSE: %s", response.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := response.Body.Len()
	if err := writer.WriteEvent(ctx, inference.Event{Type: "response.output_text.delta", Delta: "later"}); !errors.Is(err, context.Canceled) || response.Body.Len() != before {
		t.Fatal("wrote after cancellation")
	}
}

func TestAnthropicThinkingSignatureIsTyped(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai"} {
		response := httptest.NewRecorder()
		writer := &messagesSSEWriter{response: response}
		_ = writer.Start(executor.StreamMetadata{ResponseID: "msg_1", Model: "test", Provider: provider})
		if err := writer.WriteEvent(context.Background(), inference.Event{Type: "content_block_delta", ItemType: "thinking", Thinking: "reason", Signature: "signed", Data: []byte(`{"secret":"opaque"}`)}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(response.Body.String(), "opaque") || (provider == "openai" && strings.Contains(response.Body.String(), "signed")) || (provider == "anthropic" && !strings.Contains(response.Body.String(), "signed")) {
			t.Fatalf("provider=%s stream=%s", provider, response.Body.String())
		}
	}
}

func TestMessagesSSEWriterRejectsUnsupportedNativeBlockAndIncompleteTool(t *testing.T) {
	for _, test := range []struct {
		name, provider, blockType, callID, toolName string
	}{
		{name: "unknown Anthropic block", provider: "anthropic", blockType: "server_tool_use"},
		{name: "incomplete tool", provider: "openai", blockType: "tool_use", callID: "toolu_1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writer := &messagesSSEWriter{response: response}
			_ = writer.Start(executor.StreamMetadata{ResponseID: "msg", Model: "test", Provider: test.provider})
			if err := writer.WriteEvent(context.Background(), inference.Event{Type: "content_block_start", ItemID: "0", ItemType: test.blockType, CallID: test.callID, Name: test.toolName}); err == nil || strings.Contains(response.Body.String(), "event: content_block_start") {
				t.Fatalf("accepted unsupported block: %s", response.Body.String())
			}
		})
	}
}
