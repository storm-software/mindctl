package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
)

func TestDecodeChatRequestKeepsToolsAndIgnoresSamplingFields(t *testing.T) {
	body := `{
		"model":"mindctl-auto",
		"temperature":0,
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":[{"type":"text","text":"look up the weather"}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Boston\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"70F"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"weather","parameters":{"type":"object"}}}],
		"tool_choice":"auto"
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	got, err := DecodeChatRequest(httptest.NewRecorder(), request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "mindctl-auto" || got.ToolChoice != "auto" || len(got.Tools) != 1 || got.Tools[0].Name != "lookup" {
		t.Fatalf("request = %+v", got)
	}
	if len(got.Input) != 4 || got.Input[0].Role != "system" || got.Input[0].Text != "be brief" || got.Input[1].Text != "look up the weather" {
		t.Fatalf("input = %+v", got.Input)
	}
	call, output := got.Input[2], got.Input[3]
	if call.Type != "function_call" || call.CallID != "call_1" || call.Name != "lookup" || string(call.Arguments) != `{"city":"Boston"}` {
		t.Fatalf("call = %+v", call)
	}
	if output.Type != "function_call_output" || output.CallID != "call_1" || string(output.Output) != `"70F"` {
		t.Fatalf("output = %+v", output)
	}
}

func TestChatHandlerAuthenticatesBearerAndStreamsTools(t *testing.T) {
	runner := &chatExecutor{}
	handler := Authenticate(NewChatHandler(runner, ResponsesConfig{MaxBodyBytes: 1 << 20}), ChatAuthorizationHeader, StaticTokens{{ID: "client", Value: "gateway"}})
	rejected := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mindctl-auto","messages":[{"role":"user","content":"hi"}]}`))
	rejected.Header.Set("X-Mindctl-Token", "gateway")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, rejected)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("custom header status = %d, want 401", response.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mindctl-auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer gateway")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"content":"ok"`, `"name":"lookup"`, `"arguments":"{"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if runner.input.ClientID != "client" || !runner.input.Request.Stream {
		t.Fatalf("input = %+v", runner.input)
	}
}

func TestChatHandlerReturnsToolCalls(t *testing.T) {
	runner := &chatExecutor{result: inference.Result{
		ID: "chatcmpl_1", Model: "gpt-test", Status: "completed",
		Output: []inference.Item{{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: []byte(`{"q":"x"}`)}},
		Usage:  inference.Usage{InputTokens: 3, OutputTokens: 2, Known: true},
	}}
	handler := NewChatHandler(runner, ResponsesConfig{MaxBodyBytes: 1 << 20})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	request = request.WithContext(context.WithValue(request.Context(), clientIDContextKey{}, "client"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(response.Body.String(), `"prompt_tokens":3`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

type chatExecutor struct {
	input  executor.Input
	result inference.Result
}

func (e *chatExecutor) Execute(context.Context, executor.Input) (executor.Output, error) {
	result := e.result
	if result.ID == "" {
		result = inference.Result{ID: "chatcmpl_1", Model: "gpt-test", Status: "completed", Output: []inference.Item{{Type: "message", Text: "ok"}}}
	}
	return executor.Output{Result: result, Decision: router.Decision{Provider: "openai", Tier: domain.T4}}, nil
}

func (e *chatExecutor) Stream(ctx context.Context, input executor.Input, writer executor.EventWriter) error {
	e.input = input
	if err := writer.Start(executor.StreamMetadata{ResponseID: "chatcmpl_2", Model: "gpt-test", Provider: "openai"}); err != nil {
		return err
	}
	for _, event := range []inference.Event{
		{Type: "response.output_text.delta", Delta: "ok"},
		{Type: "response.function_call_arguments.delta", CallID: "call_1", Name: "lookup", ArgumentsDelta: "{"},
		{Type: "response.completed", Status: "completed", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1, Known: true}},
	} {
		if err := writer.WriteEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}
