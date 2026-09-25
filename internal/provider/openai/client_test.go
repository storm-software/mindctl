package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		for _, field := range []string{"tool_choice", "parallel_tool_calls", "reasoning", "store", "include"} {
			if _, present := body[field]; present {
				t.Fatalf("legacy API-key request unexpectedly contains %q: %v", field, body)
			}
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

func TestOpenAIResponsePreservesReasoningAndOutputItemIDs(t *testing.T) {
	result := fromResponsesResponse(responsesResponse{
		ID:     "upstream-response",
		Status: "completed",
		Output: []responseOutput{
			{ID: "msg_1", Type: "message", Role: "assistant", Content: []responseContent{{Type: "output_text", Text: "hello"}}},
			{ID: "rs_1", Type: "reasoning", Summary: json.RawMessage(`[{"type":"summary_text","text":"safe summary"}]`), EncryptedContent: json.RawMessage(`"opaque-openai-state"`)},
			{ID: "fc_1", Type: "function_call", CallID: "call_lookup", Name: "lookup", Arguments: json.RawMessage(`{"q":"mindctl"}`)},
		},
	}, openAIModel(), "openai-request")
	if len(result.Output) != 3 || result.Output[0].ID != "msg_1" || result.Output[1].ID != "rs_1" ||
		string(result.Output[1].Summary) != `[{"type":"summary_text","text":"safe summary"}]` ||
		string(result.Output[1].EncryptedContent) != `"opaque-openai-state"` || result.Output[2].ID != "fc_1" {
		t.Fatalf("output=%+v", result.Output)
	}
}

func TestOpenAIStreamEventPreservesReasoningContinuation(t *testing.T) {
	event, err := streamEvent("response.output_item.done", []byte(`{
  "output_index":0,
	  "item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"safe summary"}],"encrypted_content":"opaque-openai-state"}
	}`))
	if err != nil || event.ItemID != "rs_1" || event.ItemType != "reasoning" ||
		string(event.Summary) != `[{"type":"summary_text","text":"safe summary"}]` || string(event.EncryptedContent) != `"opaque-openai-state"` {
		t.Fatalf("event=%+v err=%v", event, err)
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
					r.Header.Get("ChatGPT-Account-Id") != "account-1" || r.Header.Get("originator") != "codex_exec" ||
					r.Header.Get("User-Agent") != "codex_exec/0.156.1" || r.Header.Get("X-Untrusted") != "" {
					t.Fatalf("path=%s headers=%v", r.URL.Path, r.Header)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["tool_choice"] != "auto" || body["parallel_tool_calls"] != false || body["store"] != false || body["stream"] != (tc.name == "stream") {
					t.Fatalf("body=%v", body)
				}
				if include, ok := body["include"].([]any); !ok || len(include) != 0 {
					t.Fatalf("include=%v body=%v", body["include"], body)
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
			ctx = upstreamauth.WithChatGPT(ctx, upstreamauth.ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1", Originator: "codex_exec", UserAgent: "codex_exec/0.156.1"})
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

func TestOpenAIRetainsAllowlistedInvalidRequestDiagnostics(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{
  "error": {
    "code": "unsupported_parameter",
    "param": "input[0].tools[0].description",
    "message": "Unsupported parameter: 'input[0].tools[0].description'."
  },
  "prompt": "private prompt",
  "response": {"output": "private response"},
  "request_headers": {"authorization": "Bearer secret"}
	}`
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"X-Request-Id": []string{"openai-req"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	client := NewClient("https://provider.example", "secret", &http.Client{Transport: transport})

	_, err := client.Execute(context.Background(), openAIModel(), textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || normalized.Status != http.StatusBadRequest || normalized.RequestID != "openai-req" ||
		normalized.UpstreamCode != "unsupported_parameter" || normalized.UpstreamParam != "input[0].tools[0].description" || normalized.UpstreamMessage != "Unsupported parameter: 'input[0].tools[0].description'." {
		t.Fatalf("kind=%q status=%d requestID=%q code=%q param=%q message=%q", normalized.Kind, normalized.Status, normalized.RequestID, normalized.UpstreamCode, normalized.UpstreamParam, normalized.UpstreamMessage)
	}
	if strings.Contains(normalized.Error(), "private") || strings.Contains(normalized.Error(), "secret") {
		t.Fatalf("error leaked upstream body: %v", normalized)
	}
}

func TestOpenAIRedactsUnsafeUpstreamErrorMessages(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{
  "error": {
    "code": "invalid_value",
    "param": "input",
    "message": "Invalid value: private prompt and Bearer secret"
  }
	}`
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	client := NewClient("https://provider.example", "secret", &http.Client{Transport: transport})

	_, err := client.Execute(context.Background(), openAIModel(), textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.UpstreamCode != "invalid_value" || normalized.UpstreamParam != "input" || normalized.UpstreamMessage != "" ||
		strings.Contains(normalized.Error(), "private") || strings.Contains(normalized.Error(), "secret") {
		t.Fatalf("error=%+v", normalized)
	}
}

func TestOpenAIRedactsNonDiagnosticUpstreamErrorMessage(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{
  "error": {
    "code": "invalid_value",
    "param": "input",
    "message": "The request contains an unsupported value."
  }
}`
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	client := NewClient("https://provider.example", "secret", &http.Client{Transport: transport})

	_, err := client.Execute(context.Background(), openAIModel(), textRequest())
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.UpstreamCode != "invalid_value" || normalized.UpstreamParam != "input" || normalized.UpstreamMessage != "" {
		t.Fatalf("error=%+v", normalized)
	}
}

func TestOpenAIResponsesLiteToolCallPayloadSurfacesRejectedField(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("x-openai-internal-codex-responses-lite") != "true" {
			t.Fatalf("responses-lite header=%q", request.Header.Get("x-openai-internal-codex-responses-lite"))
		}
		var body struct {
			Input []struct {
				Type  string `json:"type"`
				Tools []struct {
					Type        string  `json:"type"`
					Description *string `json:"description"`
					Tools       []struct {
						Type string `json:"type"`
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"tools"`
			} `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Input) != 1 || body.Input[0].Type != "additional_tools" || len(body.Input[0].Tools) != 1 ||
			body.Input[0].Tools[0].Type != "namespace" || body.Input[0].Tools[0].Description == nil || *body.Input[0].Tools[0].Description != "" ||
			len(body.Input[0].Tools[0].Tools) != 2 || body.Input[0].Tools[0].Tools[0].Type != "function" || body.Input[0].Tools[0].Tools[1].Name != "apply_patch" {
			t.Fatalf("body=%+v", body)
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body: io.NopCloser(strings.NewReader(`{
  "error": {
    "code": "unsupported_parameter",
    "param": "input[0].tools[0].description",
    "message": "Unsupported parameter: 'input[0].tools[0].description'."
  }
}`)),
		}, nil
	})
	client := NewChatGPTOAuthClient("https://provider.example", &http.Client{Transport: transport})
	request := inference.Request{
		Model: "gateway-model",
		Input: []inference.Item{{
			Type: "additional_tools", Role: "developer",
			Tools: []inference.Tool{{
				Type: "namespace", Name: "functions", Description: "",
				Tools: []inference.Tool{
					{Type: "function", Name: "read", Description: "read files", Parameters: json.RawMessage(`{"type":"object"}`)},
					{Type: "custom", Name: "apply_patch", Description: "apply a patch", Format: &inference.ToolFormat{Type: "grammar", Syntax: "lark", Definition: "start: PATCH"}},
				},
			}},
		}},
	}
	ctx := upstreamauth.WithChatGPT(context.Background(), upstreamauth.ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"})
	_, err := client.Execute(ctx, openAIModel(), request)
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.UpstreamCode != "unsupported_parameter" || normalized.UpstreamParam != "input[0].tools[0].description" ||
		normalized.UpstreamMessage != "Unsupported parameter: 'input[0].tools[0].description'." {
		t.Fatalf("err=%v normalized=%+v", err, normalized)
	}
}

func TestOpenAICodexCustomToolContinuationUsesInputAndPreservesSafeDiagnostic(t *testing.T) {
	const toolOutput = "private tool output sentinel"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Input) != 3 {
			t.Fatalf("input=%s", body.Input)
		}
		var customOutput map[string]any
		if err := json.Unmarshal(body.Input[2], &customOutput); err != nil {
			t.Fatal(err)
		}
		if customOutput["type"] != "custom_tool_call_output" || customOutput["call_id"] != "call_patch" || customOutput["input"] != toolOutput {
			t.Fatalf("custom tool continuation=%v", customOutput)
		}
		if _, present := customOutput["output"]; present {
			t.Fatalf("custom tool continuation sent rejected output field: %v", customOutput)
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body: io.NopCloser(strings.NewReader(`{
  "error": {
    "code": "unknown_parameter",
    "param": "input[2].output",
    "message": "Unknown parameter: 'input[2].output'."
  }
}`)),
		}, nil
	})
	client := NewChatGPTOAuthClient("https://provider.example", &http.Client{Transport: transport})
	request := inference.Request{
		Model:        "gateway-model",
		Instructions: "private prompt sentinel",
		Input: []inference.Item{
			{Type: "message", Role: "user", Text: "continue"},
			{Type: "custom_tool_call", CallID: "call_patch", Name: "apply_patch", Input: "*** Begin Patch"},
			{Type: "custom_tool_call_output", CallID: "call_patch", Output: json.RawMessage(`"private tool output sentinel"`)},
		},
	}
	ctx := upstreamauth.WithChatGPT(context.Background(), upstreamauth.ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"})
	_, err := client.Execute(ctx, openAIModel(), request)
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Kind != provider.ErrorInvalidRequest || normalized.UpstreamCode != "unknown_parameter" ||
		normalized.UpstreamParam != "input[2].output" || normalized.UpstreamMessage != "Unknown parameter: 'input[2].output'." {
		t.Fatalf("err=%v normalized=%+v", err, normalized)
	}
	for _, sensitive := range []string{toolOutput, request.Instructions, "oauth.jwt"} {
		if strings.Contains(normalized.Error(), sensitive) {
			t.Fatalf("error leaked sensitive value %q: %v", sensitive, normalized)
		}
	}
}

func TestOpenAIContinuationCallOutputsUseAuthSpecificFields(t *testing.T) {
	tests := []struct {
		name           string
		client         *Client
		item           inference.Item
		wantInput      bool
		wantInputValue string
		wantOutput     bool
	}{
		{
			name:           "Codex custom tool output",
			client:         NewChatGPTOAuthClient("https://provider.example", nil),
			item:           inference.Item{Type: "custom_tool_call_output", CallID: "call_custom", Output: json.RawMessage(`{"status":"complete"}`)},
			wantInput:      true,
			wantInputValue: `{"status":"complete"}`,
			wantOutput:     false,
		},
		{
			name:       "Codex function output",
			client:     NewChatGPTOAuthClient("https://provider.example", nil),
			item:       inference.Item{Type: "function_call_output", CallID: "call_function", Output: json.RawMessage(`{"status":"complete"}`)},
			wantInput:  false,
			wantOutput: true,
		},
		{
			name:       "Codex computer output",
			client:     NewChatGPTOAuthClient("https://provider.example", nil),
			item:       inference.Item{Type: "computer_call_output", CallID: "call_computer", Output: json.RawMessage(`{"status":"complete"}`)},
			wantInput:  false,
			wantOutput: true,
		},
		{
			name:       "Codex empty string function output",
			client:     NewChatGPTOAuthClient("https://provider.example", nil),
			item:       inference.Item{Type: "function_call_output", CallID: "call_empty", Output: json.RawMessage(`""`)},
			wantInput:  false,
			wantOutput: true,
		},
		{
			name:       "Codex function call omits null output",
			client:     NewChatGPTOAuthClient("https://provider.example", nil),
			item:       inference.Item{Type: "function_call", CallID: "call_shell", Name: "exec", Arguments: json.RawMessage(`{"cmd":"pwd"}`), Output: json.RawMessage(`null`)},
			wantInput:  false,
			wantOutput: false,
		},
		{
			name:       "API key function output",
			client:     NewClient("https://provider.example", "api-key", nil),
			item:       inference.Item{Type: "function_call_output", CallID: "call_function", Output: json.RawMessage(`{"status":"complete"}`)},
			wantInput:  false,
			wantOutput: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := tt.client.toResponsesRequest(openAIModel(), inference.Request{
				Model: "gateway-model",
				Input: []inference.Item{
					{Type: "message", Role: "user", Text: "continue"},
					tt.item,
				},
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			var encoded map[string]any
			if err := json.Unmarshal(body.Input[1], &encoded); err != nil {
				t.Fatal(err)
			}
			_, hasInput := encoded["input"]
			_, hasOutput := encoded["output"]
			if hasInput != tt.wantInput || hasOutput != tt.wantOutput {
				t.Fatalf("encoded output=%v, want input=%t output=%t", encoded, tt.wantInput, tt.wantOutput)
			}
			if tt.wantInput && encoded["input"] != tt.wantInputValue {
				t.Fatalf("input=%#v, want %q", encoded["input"], tt.wantInputValue)
			}
		})
	}
}

func TestOpenAIChatGPTOAuthTwoTurnContinuationPreservesReasoningAndToolOutputFields(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/responses" || request.Header.Get("Authorization") != "Bearer oauth.jwt" || request.Header.Get("ChatGPT-Account-Id") != "account-1" {
			t.Fatal("OAuth request contract was not preserved")
		}
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
  "id":"upstream-1","status":"completed","model":"gpt-test","output":[
    {"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"safe summary"}],"encrypted_content":"opaque-openai-state"},
    {"id":"fc_1","type":"function_call","call_id":"call_function","name":"lookup","arguments":"{}"}
  ]
}`))}, nil
		}
		if calls != 2 || len(body.Input) != 5 {
			t.Fatal("unexpected continuation shape")
		}
		var reasoning, functionCall, functionOutput, customOutput, computerOutput map[string]any
		if err := json.Unmarshal(body.Input[0], &reasoning); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body.Input[1], &functionCall); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body.Input[2], &functionOutput); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body.Input[3], &customOutput); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body.Input[4], &computerOutput); err != nil {
			t.Fatal(err)
		}
		if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_1" || reasoning["encrypted_content"] != "opaque-openai-state" ||
			!reflect.DeepEqual(reasoning["summary"], []any{map[string]any{"type": "summary_text", "text": "safe summary"}}) ||
			functionCall["type"] != "function_call" || functionCall["id"] != "fc_1" || functionCall["call_id"] != "call_function" ||
			functionOutput["type"] != "function_call_output" || functionOutput["output"] != "function result" ||
			customOutput["type"] != "custom_tool_call_output" || customOutput["input"] != "custom result" ||
			computerOutput["type"] != "computer_call_output" || computerOutput["output"] != "computer result" {
			t.Fatal("continuation field contract was not preserved")
		}
		for _, item := range []map[string]any{functionOutput, computerOutput} {
			if _, present := item["input"]; present {
				t.Fatal("function or computer output used input")
			}
		}
		if _, present := customOutput["output"]; present {
			t.Fatal("custom tool output used output")
		}
		if _, present := functionCall["arguments"]; present {
			t.Fatal("Codex function-call continuation sent rejected arguments field")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"upstream-2","status":"completed","model":"gpt-test","output":[]}`))}, nil
	})
	client := NewChatGPTOAuthClient("https://provider.example", &http.Client{Transport: transport})
	ctx := upstreamauth.WithChatGPT(context.Background(), upstreamauth.ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"})
	first, err := client.Execute(ctx, openAIModel(), inference.Request{Model: "gateway-model", Input: []inference.Item{{Type: "message", Role: "user", Text: "continue"}}})
	if err != nil {
		t.Fatal(err)
	}
	continuation := append([]inference.Item(nil), first.Output...)
	continuation = append(continuation,
		inference.Item{Type: "function_call_output", CallID: "call_function", Output: json.RawMessage(`"function result"`)},
		inference.Item{Type: "custom_tool_call_output", CallID: "call_custom", Output: json.RawMessage(`"custom result"`)},
		inference.Item{Type: "computer_call_output", CallID: "call_computer", Output: json.RawMessage(`"computer result"`)},
	)
	if _, err := client.Execute(ctx, openAIModel(), inference.Request{Model: "gateway-model", Input: continuation}); err != nil || calls != 2 {
		t.Fatalf("continuation failed: calls=%d err=%v", calls, err)
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

func TestOpenAIForwardsCodexWebSearchTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Type              string `json:"type"`
				ExternalWebAccess *bool  `json:"external_web_access"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 1 || body.Tools[0].Type != "web_search" || body.Tools[0].ExternalWebAccess == nil || !*body.Tools[0].ExternalWebAccess {
			t.Fatalf("tools=%+v", body.Tools)
		}
		_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"gpt-test","output":[]}`)
	}))
	defer server.Close()

	external := true
	request := textRequest()
	request.Tools = []inference.Tool{{Type: "web_search", ExternalWebAccess: &external}}
	if _, err := newClient(server.URL).Execute(context.Background(), openAIModel(), request); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIPreservesMultipartMessageContent(t *testing.T) {
	input := toResponseInput(inference.Item{
		ID: "msg_1", Type: "message", Role: "user", Text: "combined",
		Content: []inference.ContentPart{
			{Type: "input_text", Text: "first", ImageURL: json.RawMessage("null")},
			{Type: "input_text", Text: "second"},
		},
	})
	if input.ID != "msg_1" || len(input.Content) != 2 || input.Content[0].Text != "first" || input.Content[1].Text != "second" {
		t.Fatalf("input=%+v", input)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	content := wire["content"].([]any)
	if _, present := content[0].(map[string]any)["image_url"]; present {
		t.Fatalf("content=%v", content[0])
	}
}

func TestOpenAIPreservesEmptyDescriptionForNestedCodexTools(t *testing.T) {
	request := inference.Request{
		Model: "gateway-model",
		Input: []inference.Item{{
			ID: "tools_1", Type: "additional_tools", Role: "developer",
			Tools: []inference.Tool{{
				Type: "namespace", Name: "functions", Description: "", Parameters: json.RawMessage("null"),
				Tools: []inference.Tool{{Type: "function", Name: "read"}},
			}},
		}},
	}
	body, err := toResponsesRequest(openAIModel(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	tools := input[0].(map[string]any)["tools"].([]any)
	namespace := tools[0].(map[string]any)
	if description, present := namespace["description"]; !present || description != "" {
		t.Fatalf("tool=%v", tools[0])
	}
	if _, present := namespace["parameters"]; present {
		t.Fatalf("tool=%v", tools[0])
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }
