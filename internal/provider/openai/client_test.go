package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
)

func TestExecuteTranslatesOpenAIRequestAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer secret" {
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

func TestOpenAIRejectsUnsupportedExplicitFeature(t *testing.T) {
	_, err := newClient("unused").Execute(context.Background(), modelWithoutHostedTools(), hostedToolRequest("computer_use"))
	var unsupported *provider.UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != "computer_use" {
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
