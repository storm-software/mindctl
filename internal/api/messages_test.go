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
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

type messagesExecutor struct {
	input      executor.Input
	credential upstreamauth.ClaudeCredential
	hasClaude  bool
}

func (e *messagesExecutor) Execute(ctx context.Context, input executor.Input) (executor.Output, error) {
	e.input = input
	e.credential, e.hasClaude = upstreamauth.Claude(ctx)
	return executor.Output{Result: inference.Result{ID: "msg_1", Model: "gpt-test", Status: "completed", Output: []inference.Item{{Type: "message", Text: "ok"}}}, Decision: router.Decision{Provider: "openai", Tier: domain.T4}}, nil
}

func (e *messagesExecutor) Stream(ctx context.Context, input executor.Input, writer executor.EventWriter) error {
	e.input = input
	if err := writer.Start(executor.StreamMetadata{ResponseID: "msg_2", Model: "gpt-test", Provider: "openai"}); err != nil {
		return err
	}
	for _, event := range []inference.Event{
		{Type: "response.output_text.delta", Delta: "ok"},
		{Type: "response.completed", Status: "completed"},
	} {
		if err := writer.WriteEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func TestResolveMessagesModelAcceptsDatedAnthropicSnapshots(t *testing.T) {
	models := []domain.Model{{ID: "claude-haiku-4-5", Provider: "anthropic"}, {ID: "gpt-test", Provider: "openai"}}
	for _, test := range []struct {
		requested, requiredProvider, want string
		ok                                bool
	}{
		{requested: "claude-haiku-4-5", want: "claude-haiku-4-5", ok: true},
		{requested: "claude-haiku-4-5-20251001", want: "claude-haiku-4-5", ok: true},
		{requested: "claude-haiku-4-5-20251001", requiredProvider: "anthropic", want: "claude-haiku-4-5", ok: true},
		{requested: "claude-haiku-4-5-20251001", requiredProvider: "openai"},
		{requested: "claude-haiku-4-5-2025100"},
		{requested: "claude-haiku-4-5-latest"},
		{requested: "gpt-test-20251001"},
		{requested: "-20251001"},
	} {
		got, ok := resolveMessagesModel(models, test.requested, test.requiredProvider)
		if ok != test.ok || (ok && got != test.want) {
			t.Errorf("resolve(%q, %q) = %q, %v; want %q, %v", test.requested, test.requiredProvider, got, ok, test.want, test.ok)
		}
	}
}

func TestMessagesHandlerCrossProviderAndStream(t *testing.T) {
	runner := &messagesExecutor{}
	cfg := ResponsesConfig{MaxBodyBytes: 1 << 20, ProviderCredentials: map[string]bool{"openai": true}, ClaudeOAuthProviders: map[string]bool{"anthropic": true}}
	handler := AuthenticateWithError(CaptureNativeClaudeOAuth(NewMessagesHandler(runner, cfg)), "X-Mindctl-Token", staticTokens{{ID: "client", Value: "gateway"}}, writeMessagesError)
	for _, test := range []struct {
		name, body string
		stream     bool
	}{
		{name: "text", body: `{"model":"mindctl-auto","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "tool result continuation", body: `{"model":"mindctl-auto","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"run","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`},
		{name: "stream", body: `{"model":"mindctl-auto","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`, stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(test.body))
			request.Header.Set("X-Mindctl-Token", "gateway")
			request.Header.Set("Authorization", "Bearer native-secret")
			request.Header.Set("anthropic-beta", "oauth-2025-04-20,other")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ok") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if runner.input.ProviderCredentials["anthropic"] != true || runner.input.RequiredProvider != "" || runner.input.ClientID != "client" {
				t.Fatalf("routing input=%+v", runner.input)
			}
			if !test.stream && (!runner.hasClaude || runner.credential.Beta != "oauth-2025-04-20,other") {
				t.Fatal("native credential was not request scoped")
			}
			if test.stream && !strings.Contains(response.Body.String(), "event: message_stop") {
				t.Fatalf("missing stream terminal: %s", response.Body.String())
			}
		})
	}
}
