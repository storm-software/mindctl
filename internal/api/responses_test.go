package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

func TestResponsesHandlerReturnsActualModelAndRoutingHeaders(t *testing.T) {
	handler := testResponsesHandler(executor.Output{Result: inference.Result{ID: "resp_gateway", Model: "claude-test", Status: "completed", Output: []inference.Item{{Type: "message", Role: "assistant", Text: "hello"}}}, Decision: router.Decision{Tier: domain.T4}, AttemptID: "att_gateway"})
	req := authenticatedRequest(`{"model":"mindctl-auto","input":"hello"}`)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || body.Model != "claude-test" || rr.Header().Get("X-Mindctl-Tier") != "T4" || rr.Header().Get("X-Mindctl-Decision-ID") != "att_gateway" || rr.Header().Get("X-Mindctl-Attempts") != "1" {
		t.Fatalf("status=%d headers=%v body=%s", rr.Code, rr.Header(), rr.Body.String())
	}
}

func TestResponsesHandlerMapsSafeGatewayErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"no model", &router.NoEligibleModelError{}, http.StatusBadRequest},
		{"unsupported", &provider.UnsupportedFeatureError{Feature: "private"}, http.StatusBadRequest},
		{"rate limited", &provider.Error{Kind: provider.ErrorRateLimit, Err: errors.New("private")}, http.StatusTooManyRequests},
		{"overloaded", &provider.Error{Kind: provider.ErrorOverloaded, Err: errors.New("private")}, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := testResponsesHandler(executor.Output{}, tc.err)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, authenticatedRequest(`{"model":"mindctl-auto","input":"hello"}`))
			if rr.Code != tc.want || strings.Contains(rr.Body.String(), "private") {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestResponsesHandlerMapsProviderAuthenticationAsOperationalFailure(t *testing.T) {
	handler := testResponsesHandler(executor.Output{}, &provider.Error{Kind: provider.ErrorAuthentication, Err: errors.New("private upstream authentication failure")})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, authenticatedRequest(`{"model":"mindctl-auto","input":"hello"}`))
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable || body.Error.Type != "server_error" || body.Error.Code != "provider_authentication_unavailable" || strings.Contains(rr.Body.String(), "private") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestResponsesHandlerMapsSafetyRefusalForStreamingAndNonStreaming(t *testing.T) {
	for _, bodyText := range []string{
		`{"model":"mindctl-auto","input":"hello"}`,
		`{"model":"mindctl-auto","input":"hello","stream":true}`,
	} {
		handler := testResponsesHandler(executor.Output{}, &provider.Error{Kind: provider.ErrorKind("safety_refusal"), Err: errors.New("private provider refusal")})
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, authenticatedRequest(bodyText))
		var body ErrorBody
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rr.Code != http.StatusBadRequest || body.Error.Type != "invalid_request_error" || body.Error.Code != "safety_refusal" || strings.Contains(rr.Body.String(), "private") {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}

func TestResponsesHandlerStreamsCanonicalTerminalErrorAfterVisibleOutput(t *testing.T) {
	handler := testResponsesHandlerWith(visibleThenFailExecutor{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, authenticatedRequest(`{"model":"mindctl-auto","input":"hello","stream":true}`))
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "text/event-stream" || rr.Header().Get("Cache-Control") != "no-cache" || rr.Header().Get("X-Mindctl-Tier") != "T4" || rr.Header().Get("X-Mindctl-Decision-ID") != "att_gateway" || rr.Header().Get("X-Mindctl-Attempts") != "1" {
		t.Fatalf("status=%d headers=%v body=%s", rr.Code, rr.Header(), rr.Body.String())
	}
	want := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp_gateway\",\"item_id\":\"item_gateway\",\"delta\":\"partial\"}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"provider is temporarily unavailable\",\"type\":\"server_error\",\"code\":\"provider_unavailable\"}}\n\n"
	if rr.Body.String() != want {
		t.Fatalf("body=%q", rr.Body.String())
	}
}

func TestResponsesHandlerRejectsWrongMethodBeforeExecution(t *testing.T) {
	runner := &stubExecutor{}
	handler := testResponsesHandlerWith(runner)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	handler.ServeHTTP(rr, req)
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodPost || rr.Header().Get("Content-Type") != "application/json" || body.Error.Message != "method not allowed" || body.Error.Type != "invalid_request_error" || body.Error.Code != "method_not_allowed" || runner.calls != 0 {
		t.Fatalf("status=%d allow=%q contentType=%q body=%+v calls=%d", rr.Code, rr.Header().Get("Allow"), rr.Header().Get("Content-Type"), body, runner.calls)
	}
}

func TestResponsesHandlerDerivesChatGPTCredentialAvailabilityPerRequest(t *testing.T) {
	cases := []struct {
		label, authorization, account string
		wantOAuth                     bool
	}{
		{"first", "Bearer oauth.first", "account-1", true},
		{"second", "Bearer oauth.second", "account-2", true},
		{"missing", "Bearer oauth.missing", "", false},
	}
	release := make(chan struct{})
	runner := &concurrentCredentialRecorder{
		started:      make(chan struct{}, len(cases)),
		release:      release,
		observations: make(map[string]credentialObservation),
	}
	responses := NewResponsesHandler(runner, ResponsesConfig{
		MaxBodyBytes:          1 << 20,
		Models:                []domain.Model{{ID: "gpt-test", Provider: "openai", Tier: domain.T4}},
		ProviderCredentials:   map[string]bool{"openai": true, "anthropic": true},
		ChatGPTOAuthProviders: map[string]bool{"openai": true},
	})
	h := Authenticate(CaptureChatGPTOAuth(responses), "X-Mindctl-Token", staticTokens{{ID: "client", Value: "gateway"}})

	statuses := make(chan int, len(cases))
	var requests sync.WaitGroup
	for _, tc := range cases {
		requests.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"mindctl-auto","instructions":"`+tc.label+`","input":"hello"}`))
			req.Header.Set("X-Mindctl-Token", "gateway")
			req.Header.Set("Authorization", tc.authorization)
			if tc.account != "" {
				req.Header.Set("ChatGPT-Account-Id", tc.account)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			statuses <- rr.Code
		})
	}
	for range cases {
		<-runner.started
	}
	close(release)
	requests.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
	}

	for _, tc := range cases {
		got, ok := runner.observation(tc.label)
		if !ok || got.oauthAvailable != tc.wantOAuth || !got.anthropicAvailable {
			t.Fatalf("label=%s observation=%+v present=%v", tc.label, got, ok)
		}
		if tc.wantOAuth && (got.credential.AccessToken != strings.TrimPrefix(tc.authorization, "Bearer ") || got.credential.AccountID != tc.account) {
			t.Fatalf("label=%s credential=%+v", tc.label, got.credential)
		}
		if !tc.wantOAuth && got.hasCredential {
			t.Fatalf("label=%s unexpectedly captured credential=%+v", tc.label, got.credential)
		}
	}
}

type credentialObservation struct {
	credential                    upstreamauth.ChatGPTCredential
	hasCredential, oauthAvailable bool
	anthropicAvailable            bool
}

type concurrentCredentialRecorder struct {
	mu           sync.Mutex
	started      chan struct{}
	release      <-chan struct{}
	observations map[string]credentialObservation
}

func (r *concurrentCredentialRecorder) Execute(ctx context.Context, input executor.Input) (executor.Output, error) {
	r.started <- struct{}{}
	<-r.release
	credential, ok := upstreamauth.ChatGPT(ctx)
	r.mu.Lock()
	r.observations[input.Request.Instructions] = credentialObservation{
		credential: credential, hasCredential: ok,
		oauthAvailable: input.ProviderCredentials["openai"], anthropicAvailable: input.ProviderCredentials["anthropic"],
	}
	r.mu.Unlock()
	return executor.Output{Result: inference.Result{Status: "completed"}}, nil
}

func (r *concurrentCredentialRecorder) Stream(context.Context, executor.Input, executor.EventWriter) error {
	return errors.New("unexpected streaming execution")
}

func (r *concurrentCredentialRecorder) observation(label string) (credentialObservation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.observations[label]
	return value, ok
}

func testResponsesHandler(output executor.Output, err ...error) http.Handler {
	runner := &stubExecutor{output: output}
	if len(err) != 0 {
		runner.err = err[0]
	}
	return testResponsesHandlerWith(runner)
}

func testResponsesHandlerWith(runner ResponseExecutor) http.Handler {
	return Authenticate(NewResponsesHandler(runner, ResponsesConfig{MaxBodyBytes: 1 << 20, Models: []domain.Model{{ID: "claude-test", Tier: domain.T4}}}), "Authorization", staticTokens{{ID: "client_test", Value: "test-token"}})
}

func authenticatedRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

type stubExecutor struct {
	output executor.Output
	err    error
	calls  int
	input  executor.Input
}

type visibleThenFailExecutor struct{}

func (visibleThenFailExecutor) Execute(context.Context, executor.Input) (executor.Output, error) {
	return executor.Output{}, errors.New("unexpected non-streaming execution")
}

func (visibleThenFailExecutor) Stream(ctx context.Context, _ executor.Input, writer executor.EventWriter) error {
	if err := writer.Start(executor.StreamMetadata{ResponseID: "resp_gateway", Model: "gateway-model", DecisionID: "att_gateway", Tier: domain.T4, Attempts: 1}); err != nil {
		return err
	}
	if err := writer.WriteEvent(ctx, inference.Event{Type: "response.output_text.delta", ResponseID: "resp_gateway", ItemID: "item_gateway", Delta: "partial"}); err != nil {
		return err
	}
	err := &provider.Error{Kind: provider.ErrorRetryable, Err: errors.New("private provider failure")}
	if writeErr := writer.WriteTerminalError(ctx, err); writeErr != nil {
		return writeErr
	}
	return err
}

func (s *stubExecutor) Execute(_ context.Context, input executor.Input) (executor.Output, error) {
	s.calls++
	s.input = input
	return s.output, s.err
}

func (s *stubExecutor) Stream(_ context.Context, input executor.Input, _ executor.EventWriter) error {
	s.calls++
	s.input = input
	return s.err
}
