package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

func TestAnthropicTranslatesInstructionsFunctionsAndToolResults(t *testing.T) {
	server := anthropicFixture(t, func(body messagesRequest) {
		if body.System != "be concise" || body.Model != "claude-test" || len(body.Tools) != 1 {
			t.Fatalf("body=%+v", body)
		}
		if len(body.Messages) != 2 || body.Messages[0].Content[0].Type != "tool_use" || body.Messages[1].Content[0].Type != "tool_result" {
			t.Fatalf("messages=%+v", body.Messages)
		}
		var result string
		if err := json.Unmarshal(body.Messages[1].Content[0].Content, &result); err != nil || result != `{"temperature":70}` {
			t.Fatalf("tool result content=%s err=%v", body.Messages[1].Content[0].Content, err)
		}
	}, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":2}}`)
	defer server.Close()

	got, err := newAnthropic(server.URL).Execute(context.Background(), anthropicModel(), functionResultRequest())
	if err != nil || len(got.Output) != 1 || got.Output[0].Text != "done" || got.Usage.InputTokens != 8 || got.Status != "completed" || got.ProviderRequestID != "anthropic-req" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestAnthropicClaudeOAuthUsesRequestCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer oauth-token" {
			t.Fatalf("path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != anthropicVersion ||
			r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Fatalf("headers=%v", r.Header)
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()

	ctx := upstreamauth.WithClaude(context.Background(), upstreamauth.ClaudeCredential{AccessToken: "oauth-token"})
	if _, err := NewClaudeOAuthClient(server.URL, nil).Execute(ctx, anthropicModel(), textRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicClaudeOAuthRejectsMissingRequestCredential(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()

	_, err := NewClaudeOAuthClient(server.URL, nil).Execute(context.Background(), anthropicModel(), textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestAnthropicClaudeOAuthDoesNotFollowRedirects(t *testing.T) {
	var redirectedCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	ctx := upstreamauth.WithClaude(context.Background(), upstreamauth.ClaudeCredential{AccessToken: "oauth-token"})
	_, err := NewClaudeOAuthClient(server.URL, nil).Execute(ctx, anthropicModel(), textRequest())
	if err == nil || redirectedCalls.Load() != 0 {
		t.Fatalf("err=%v redirected calls=%d", err, redirectedCalls.Load())
	}
}

func TestAnthropicTranslatesImagesAndJSONSchema(t *testing.T) {
	server := anthropicFixture(t, func(body messagesRequest) {
		if len(body.Messages) != 1 || body.Messages[0].Content[0].Type != "image" || body.Messages[0].Content[0].Source.Type != "url" {
			t.Fatalf("messages=%+v", body.Messages)
		}
		if body.OutputConfig == nil || body.OutputConfig.Format.Type != "json_schema" || string(body.OutputConfig.Format.Schema) != `{"type":"object"}` {
			t.Fatalf("output_config=%+v", body.OutputConfig)
		}
	}, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	defer server.Close()

	request := imageRequest()
	request.TextFormat = &inference.JSONSchemaFormat{Type: "json_schema", Name: "answer", Schema: json.RawMessage(`{"type":"object"}`)}
	got, err := newAnthropic(server.URL).Execute(context.Background(), anthropicModel(), request)
	if err != nil || got.Usage.Known {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestAnthropicCoalescesAdjacentContentForNativeMessages(t *testing.T) {
	server := anthropicFixture(t, func(body messagesRequest) {
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" || len(body.Messages[0].Content) != 2 {
			t.Fatalf("messages=%+v", body.Messages)
		}
	}, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	defer server.Close()

	request := textRequest()
	request.Input = append(request.Input, imageRequest().Input[0])
	if _, err := newAnthropic(server.URL).Execute(context.Background(), anthropicModel(), request); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicCapabilityValidation(t *testing.T) {
	client := newAnthropic("unused")
	if _, err := client.Execute(context.Background(), anthropicModel(), hostedToolRequest("file_search")); err == nil {
		t.Fatal("expected unsupported feature")
	}
	if _, err := client.Execute(context.Background(), textOnlyAnthropicModel(), imageRequest()); err == nil {
		t.Fatal("expected image rejection")
	}
}

func TestAnthropicRejectsMissingUpstreamModelBeforeNetworkCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	model := anthropicModel()
	model.UpstreamID = ""

	_, err := newAnthropic(server.URL).Execute(context.Background(), model, textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestAnthropicUsesConfiguredOutputLimitWhenRequestOmitsOne(t *testing.T) {
	server := anthropicFixture(t, func(body messagesRequest) {
		if body.MaxTokens != 77 {
			t.Fatalf("max_tokens=%d", body.MaxTokens)
		}
	}, `{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`)
	defer server.Close()
	model := anthropicModel()
	model.MaxOutputTokens = 77
	if _, err := newAnthropic(server.URL).Execute(context.Background(), model, textRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicStreamNormalizesLifecycleAndCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("accept=%q", r.Header.Get("Accept"))
		}
		w.Header().Set("request-id", "anthropic-stream-req")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
	}))
	defer server.Close()

	stream, err := newAnthropic(server.URL).Stream(context.Background(), anthropicModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	start, err := stream.Next(context.Background())
	if err != nil || start.Type != "message_start" || start.ResponseID != "msg_1" || start.Status != "in_progress" || !start.Usage.Known || start.Usage.InputTokens != 3 || start.ProviderRequestID != "anthropic-stream-req" {
		t.Fatalf("event=%+v err=%v", start, err)
	}
	_, _ = stream.Next(context.Background())
	delta, err := stream.Next(context.Background())
	if err != nil || delta.Type != "content_block_delta" || delta.Delta != "hi" || delta.ItemID != "0" {
		t.Fatalf("event=%+v err=%v", delta, err)
	}
	completed, err := stream.Next(context.Background())
	if err != nil || completed.Status != "completed" || !completed.Usage.Known || completed.Usage.OutputTokens != 1 {
		t.Fatalf("event=%+v err=%v", completed, err)
	}
}

func TestAnthropicStreamCorrelatesToolStartAndArgumentDelta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n")
	}))
	defer server.Close()

	stream, err := newAnthropic(server.URL).Stream(context.Background(), anthropicModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	start, err := stream.Next(context.Background())
	if err != nil || start.ItemID != "toolu_1" || start.CallID != "toolu_1" || start.Name != "lookup" {
		t.Fatalf("start=%+v err=%v", start, err)
	}
	delta, err := stream.Next(context.Background())
	if err != nil || delta.ItemID != start.ItemID || delta.CallID != start.CallID || delta.Name != start.Name || delta.ArgumentsDelta != `{"city":` {
		t.Fatalf("delta=%+v start=%+v err=%v", delta, start, err)
	}
}

func TestAnthropicStreamPreservesRequestIDOnMalformedFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "anthropic-stream-req")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\n\n")
	}))
	defer server.Close()
	stream, err := newAnthropic(server.URL).Stream(context.Background(), anthropicModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next(context.Background())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorRetryable || normalized.RequestID != "anthropic-stream-req" {
		t.Fatalf("err=%v", err)
	}
}

func TestAnthropicStreamNormalizesProviderErrorWithoutBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "anthropic-stream-req")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"secret upstream body\"}}\n\n")
	}))
	defer server.Close()
	stream, err := newAnthropic(server.URL).Stream(context.Background(), anthropicModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next(context.Background())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorRateLimit || normalized.RequestID != "anthropic-stream-req" || strings.Contains(normalized.Error(), "secret") {
		t.Fatalf("err=%v", err)
	}
}

func anthropicFixture(t *testing.T, check func(messagesRequest), response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") == "" {
			t.Fatalf("method=%s path=%s x-api-key=%q version=%q", r.Method, r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		var body messagesRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		check(body)
		w.Header().Set("request-id", "anthropic-req")
		_, _ = io.WriteString(w, response)
	}))
}

func anthropicModel() domain.Model {
	return domain.Model{ID: "gateway-model", UpstreamID: "claude-test", Capabilities: domain.Capabilities{Text: true, Images: true, Functions: true, JSONSchema: true}}
}

func textOnlyAnthropicModel() domain.Model {
	return domain.Model{ID: "gateway-model", UpstreamID: "claude-test", Capabilities: domain.Capabilities{Text: true}}
}

func functionResultRequest() inference.Request {
	return inference.Request{
		Model: "gateway-model", Instructions: "be concise",
		Input: []inference.Item{
			{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"city":"Boston"}`)},
			{Type: "function_call_output", CallID: "call_1", Output: json.RawMessage(`{"temperature":70}`)},
		},
		Tools: []inference.Tool{{Type: "function", Name: "lookup", Description: "look up weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
}

func textRequest() inference.Request {
	return inference.Request{Model: "gateway-model", Instructions: "be concise", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}}
}

func imageRequest() inference.Request {
	return inference.Request{Model: "gateway-model", Input: []inference.Item{{Type: "input_image", Role: "user", ImageURL: json.RawMessage(`{"url":"https://example.test/image.png"}`)}}}
}

func hostedToolRequest(kind string) inference.Request {
	request := textRequest()
	request.Tools = []inference.Tool{{Type: kind}}
	return request
}

func newAnthropic(baseURL string) *Client { return NewClient(baseURL, "secret", nil) }
