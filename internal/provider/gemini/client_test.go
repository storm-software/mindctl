package gemini

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
)

func TestGeminiTranslatesContentFunctionsAndSchema(t *testing.T) {
	server := geminiFixture(t, func(path string, body generateRequest) {
		if !strings.Contains(path, "models/gemini-test:generateContent") {
			t.Fatalf("path=%s", path)
		}
		if len(body.Tools) != 1 || body.GenerationConfig == nil || body.GenerationConfig.ResponseSchema == nil {
			t.Fatalf("body=%+v", body)
		}
		if body.SystemInstruction == nil || body.SystemInstruction.Parts[0].Text != "be concise" {
			t.Fatalf("system=%+v", body.SystemInstruction)
		}
	}, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	defer server.Close()

	got, err := newGemini(server.URL).Execute(context.Background(), geminiModel(), schemaFunctionRequest())
	if err != nil || len(got.Output) != 1 || got.Output[0].Text != "done" || got.Usage.OutputTokens != 1 || got.Status != "completed" || got.ProviderRequestID != "gemini-req" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestGeminiTranslatesInlineAndFileImages(t *testing.T) {
	server := geminiFixture(t, func(_ string, body generateRequest) {
		if len(body.Contents) != 1 || len(body.Contents[0].Parts) != 2 {
			t.Fatalf("contents=%+v", body.Contents)
		}
		if body.Contents[0].Parts[0].InlineData == nil || body.Contents[0].Parts[0].InlineData.MimeType != "image/png" || body.Contents[0].Parts[1].FileData == nil || body.Contents[0].Parts[1].FileData.FileURI != "https://files.example.test/cat.png" {
			t.Fatalf("parts=%+v", body.Contents[0].Parts)
		}
	}, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	defer server.Close()
	request := textRequest()
	request.Input = []inference.Item{
		{Type: "input_image", Role: "user", ImageURL: json.RawMessage(`{"url":"data:image/png;base64,aGVsbG8="}`)},
		{Type: "input_image", Role: "user", ImageURL: json.RawMessage(`{"url":"https://files.example.test/cat.png"}`)},
	}
	if _, err := newGemini(server.URL).Execute(context.Background(), geminiModel(), request); err != nil {
		t.Fatal(err)
	}
}

func TestGeminiRejectsUnsupportedHostedTool(t *testing.T) {
	_, err := newGemini("unused").Execute(context.Background(), geminiModel(), hostedToolRequest("code_interpreter"))
	var unsupported *provider.UnsupportedFeatureError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiEmptyCandidatesReturnsNormalizedError(t *testing.T) {
	err := executeGeminiErrorFixture(t, `{"candidates":[],"usageMetadata":{}}`)
	var providerErr *provider.Error
	if !errors.As(err, &providerErr) || providerErr.Kind != provider.ErrorRetryable {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiThoughtSignatureSurvivesRoundTrip(t *testing.T) {
	got := executeGeminiFixture(t, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"opaque-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	next, err := toGenerateRequest(geminiModel(), requestWithPriorItem(got.Output[0]))
	if err != nil || len(next.Contents) != 1 || len(next.Contents[0].Parts) != 1 || next.Contents[0].Parts[0].ThoughtSignature != "opaque-signature" {
		t.Fatalf("next=%+v err=%v", next, err)
	}
}

func TestGeminiRejectsMissingUpstreamModelBeforeNetworkCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	model := geminiModel()
	model.UpstreamID = ""
	_, err := newGemini(server.URL).Execute(context.Background(), model, textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestGeminiSafetyAndMalformedUsageReturnNormalizedError(t *testing.T) {
	for _, response := range []string{
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":-1,"candidatesTokenCount":1}}`,
	} {
		err := executeGeminiErrorFixture(t, response)
		var normalized *provider.Error
		if !errors.As(err, &normalized) {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestGeminiStreamNormalizesTextFunctionAndCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("url=%s accept=%q", r.URL.String(), r.Header.Get("Accept"))
		}
		w.Header().Set("x-goog-request-id", "gemini-stream-req")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"responseId\":\"resp_1\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]}}]}\n\ndata: {\"responseId\":\"resp_1\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"city\":\"Boston\"}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2}}\n\n")
	}))
	defer server.Close()
	stream, err := newGemini(server.URL).Stream(context.Background(), geminiModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	text, err := stream.Next(context.Background())
	if err != nil || text.ResponseID != "resp_1" || text.Delta != "hi" || text.Status != "in_progress" || text.ProviderRequestID != "gemini-stream-req" {
		t.Fatalf("event=%+v err=%v", text, err)
	}
	call, err := stream.Next(context.Background())
	if err != nil || call.Name != "lookup" || call.ArgumentsDelta != `{"city":"Boston"}` || call.Status != "completed" || !call.Usage.Known || call.Usage.OutputTokens != 2 {
		t.Fatalf("event=%+v err=%v", call, err)
	}
}

func TestStreamEventMapsTextFrame(t *testing.T) {
	event, _, _, err := streamEvent([]byte(`{"responseId":"resp_1","candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`), "")
	if err != nil || event.Delta != "hi" {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}

func TestGeminiStreamNormalizesMalformedFramesAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", "gemini-stream-req")
		_, _ = io.WriteString(w, "data: {\n\n")
	}))
	defer server.Close()
	stream, err := newGemini(server.URL).Stream(context.Background(), geminiModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next(context.Background())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorRetryable || normalized.RequestID != "gemini-stream-req" {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiStreamClassifiesProviderErrorWithoutBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-goog-request-id", "gemini-stream-req")
		_, _ = io.WriteString(w, "data: {\"error\":{\"code\":429,\"message\":\"secret upstream body\"}}\n\n")
	}))
	defer server.Close()
	stream, err := newGemini(server.URL).Stream(context.Background(), geminiModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next(context.Background())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorRateLimit || normalized.RequestID != "gemini-stream-req" || strings.Contains(normalized.Error(), "secret") {
		t.Fatalf("err=%v", err)
	}
}

func geminiFixture(t *testing.T, check func(string, generateRequest), response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("x-goog-api-key") != "secret" {
			t.Fatalf("method=%s api-key=%q", r.Method, r.Header.Get("x-goog-api-key"))
		}
		var body generateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		check(r.URL.String(), body)
		w.Header().Set("x-goog-request-id", "gemini-req")
		_, _ = io.WriteString(w, response)
	}))
}

func geminiModel() domain.Model {
	return domain.Model{ID: "gateway-model", UpstreamID: "gemini-test", Capabilities: domain.Capabilities{Text: true, Images: true, Functions: true, JSONSchema: true}}
}

func textRequest() inference.Request {
	return inference.Request{Model: "gateway-model", Instructions: "be concise", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}}
}

func schemaFunctionRequest() inference.Request {
	request := textRequest()
	request.Tools = []inference.Tool{{Type: "function", Name: "lookup", Description: "look up weather", Parameters: json.RawMessage(`{"type":"object"}`)}}
	request.TextFormat = &inference.JSONSchemaFormat{Type: "json_schema", Name: "answer", Schema: json.RawMessage(`{"type":"object"}`)}
	return request
}

func hostedToolRequest(kind string) inference.Request {
	request := textRequest()
	request.Tools = []inference.Tool{{Type: kind}}
	return request
}

func requestWithPriorItem(item inference.Item) inference.Request {
	return inference.Request{Model: "gateway-model", Input: []inference.Item{item}}
}

func newGemini(baseURL string) *Client { return NewClient(baseURL, "secret", nil) }

func executeGeminiFixture(t *testing.T, response string) inference.Result {
	t.Helper()
	server := geminiFixture(t, func(string, generateRequest) {}, response)
	defer server.Close()
	got, err := newGemini(server.URL).Execute(context.Background(), geminiModel(), textRequest())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func executeGeminiErrorFixture(t *testing.T, response string) error {
	t.Helper()
	server := geminiFixture(t, func(string, generateRequest) {}, response)
	defer server.Close()
	_, err := newGemini(server.URL).Execute(context.Background(), geminiModel(), textRequest())
	return err
}
