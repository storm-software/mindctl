package openai

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

func TestExecuteTranslatesOpenAIRequestAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("ChatGPT-Account-Id") != "" {
			t.Fatalf("method=%s path=%s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "gpt-test" || body["input"] == nil || body["instructions"] != "be concise" {
			t.Fatalf("body=%v", body)
		}
		w.Header().Set("x-request-id", "openai-req")
		_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":3,"output_tokens":1,"input_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer server.Close()

	got, err := newClient(server.URL).Execute(context.Background(), openAIModel(), textRequest())
	if err != nil || got.ProviderRequestID != "openai-req" || got.Usage.CachedInputTokens != 2 || len(got.Output) != 1 || got.Output[0].Text != "hi" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestOpenAIChatGPTOAuthExecuteAndStreamUseRequestCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		invoke func(context.Context, *Client) error
	}{
		{"execute", func(ctx context.Context, client *Client) error {
			_, err := client.Execute(ctx, openAIModel(), textRequest())
			return err
		}},
		{"stream", func(ctx context.Context, client *Client) error {
			stream, err := client.Stream(ctx, openAIModel(), textRequest())
			if err != nil {
				return err
			}
			return stream.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer oauth.jwt" ||
					r.Header.Get("ChatGPT-Account-Id") != "account-1" || r.Header.Get("originator") != "mindctl" ||
					!strings.HasPrefix(r.Header.Get("User-Agent"), "mindctl") || r.Header.Get("X-Untrusted") != "" {
					t.Fatalf("path=%s headers=%v", r.URL.Path, r.Header)
				}
				if tc.name == "stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"id\":\"upstream\",\"status\":\"completed\"}}\n\n")
					return
				}
				_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"gpt-test","output":[]}`)
			}))
			defer server.Close()

			type untrustedContextKey struct{}
			ctx := context.WithValue(context.Background(), untrustedContextKey{}, "X-Untrusted: private")
			ctx = upstreamauth.WithChatGPT(ctx, upstreamauth.ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"})
			if err := tc.invoke(ctx, NewChatGPTOAuthClient(server.URL+"/", nil)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenAIChatGPTOAuthRequiresRequestCredentialBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	_, err := NewChatGPTOAuthClient(server.URL, nil).Execute(context.Background(), openAIModel(), textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || calls.Load() != 0 {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}

func TestOpenAIChatGPTOAuthSanitizesAuthenticationFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "private-upstream-body")
			}))
			defer server.Close()
			ctx := upstreamauth.WithChatGPT(context.Background(), upstreamauth.ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"})
			_, err := NewChatGPTOAuthClient(server.URL, nil).Execute(ctx, openAIModel(), textRequest())
			var normalized *provider.Error
			if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorAuthentication || strings.Contains(normalized.Error(), "private") || strings.Contains(normalized.Error(), "oauth.jwt") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestOpenAIRedirectDoesNotForwardChatGPTOAuth(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	ctx := upstreamauth.WithChatGPT(context.Background(), upstreamauth.ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"})
	_, err := NewChatGPTOAuthClient(origin.URL, nil).Execute(ctx, openAIModel(), textRequest())
	if err == nil || targetCalls.Load() != 0 {
		t.Fatalf("error=%v targetCalls=%d", err, targetCalls.Load())
	}
}

func TestOpenAIRejectsUnsupportedExplicitFeature(t *testing.T) {
	_, err := newClient("unused").Execute(context.Background(), modelWithoutHostedTools(), hostedToolRequest("computer_use"))
	var unsupported *provider.UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != "computer_use" {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenAIRejectsConfiguredHostedToolWithoutNetworkCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	model := openAIModel()
	model.Capabilities.HostedTools = map[string]bool{"computer_use": true}
	_, err := newClient(server.URL).Execute(context.Background(), model, hostedToolRequest("computer_use"))
	var unsupported *provider.UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != "computer_use" || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestOpenAIRejectsModelWithoutUpstreamIDBeforeNetworkCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	model := openAIModel()
	model.UpstreamID = ""
	_, err := newClient(server.URL).Execute(context.Background(), model, textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestOpenAISanitizesItemEncodingFailure(t *testing.T) {
	model := openAIModel()
	model.Capabilities.Images = true
	request := textRequest()
	request.Input = []inference.Item{{Type: "input_image", Role: "user", ImageURL: json.RawMessage("{")}}
	_, err := newClient("unused").Execute(context.Background(), model, request)
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || normalized.RequestID != "" || normalized.Error() != "provider request failed (invalid_request)" {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenAIMissingUsageDoesNotPanic(t *testing.T) {
	got := executeFixture(t, `{"id":"x","status":"completed","output":[]}`)
	if got.Usage.Known {
		t.Fatalf("usage=%+v", got.Usage)
	}
}

func TestOpenAIStreamNormalizesTextAndToolEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&map[string]any{}); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"response_id\":\"resp_1\",\"item_id\":\"msg_1\",\"delta\":\"hi\"}\n\nevent: response.function_call_arguments.delta\ndata: {\"response_id\":\"resp_1\",\"item_id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"delta\":\"{\"}\n\n")
	}))
	defer server.Close()

	stream, err := newClient(server.URL).Stream(context.Background(), openAIModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	text, err := stream.Next(context.Background())
	if err != nil || text.Type != "response.output_text.delta" || text.ResponseID != "resp_1" || text.ItemID != "msg_1" || text.Delta != "hi" {
		t.Fatalf("event=%+v err=%v", text, err)
	}
	tool, err := stream.Next(context.Background())
	if err != nil || tool.Type != "response.function_call_arguments.delta" || tool.CallID != "call_1" || tool.Name != "lookup" || tool.ArgumentsDelta != "{" {
		t.Fatalf("event=%+v err=%v", tool, err)
	}
}

func TestOpenAIStreamPreservesRequestIDOnEventsAndMalformedFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "openai-stream-req")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\n\n")
	}))
	defer server.Close()
	stream, err := newClient(server.URL).Stream(context.Background(), openAIModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next(context.Background())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorRetryable || normalized.RequestID != "openai-stream-req" {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenAIStreamNormalizesCompletionStatusAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "openai-stream-req")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"id\":\"upstream\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":2}}}}\n\n")
	}))
	defer server.Close()
	stream, err := newClient(server.URL).Stream(context.Background(), openAIModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next(context.Background())
	if err != nil || event.ProviderRequestID != "openai-stream-req" || event.ResponseID != "upstream" || event.Status != "completed" || !event.Usage.Known || event.Usage.InputTokens != 3 || event.Usage.CachedInputTokens != 2 {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}

func TestOpenAIStreamRejectsUnsuccessfulTerminalFrames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		data  string
	}{
		{"error", "error", `{"error":{"message":"private upstream body"}}`},
		{"failed", "response.failed", `{"response":{"id":"upstream","status":"failed"}}`},
		{"cancelled", "response.cancelled", `{"response":{"id":"upstream","status":"cancelled"}}`},
		{"incomplete", "response.incomplete", `{"response":{"id":"upstream","status":"incomplete"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", "openai-stream-req")
				_, _ = io.WriteString(w, "event: "+tc.event+"\ndata: "+tc.data+"\n\n")
			}))
			defer server.Close()

			stream, err := newClient(server.URL).Stream(context.Background(), openAIModel(), textRequest())
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			_, err = stream.Next(context.Background())
			var normalized *provider.Error
			if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorRetryable || normalized.RequestID != "openai-stream-req" || strings.Contains(normalized.Error(), "private") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func openAIModel() domain.Model {
	return domain.Model{ID: "gateway-model", UpstreamID: "gpt-test", Capabilities: domain.Capabilities{Text: true, Functions: true, JSONSchema: true, HostedTools: map[string]bool{"web_search": true}}}
}

func modelWithoutHostedTools() domain.Model {
	model := openAIModel()
	model.Capabilities.HostedTools = nil
	return model
}

func textRequest() inference.Request {
	return inference.Request{Model: "gateway-model", Instructions: "be concise", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}}
}

func hostedToolRequest(kind string) inference.Request {
	request := textRequest()
	request.Tools = []inference.Tool{{Type: kind}}
	return request
}

func newClient(baseURL string) *Client { return NewClient(baseURL, "secret", nil) }

func executeFixture(t *testing.T, response string) inference.Result {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, response)
	}))
	defer server.Close()
	got, err := newClient(server.URL).Execute(context.Background(), openAIModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	return got
}
