package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
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

func TestResponsesHandlerRejectsWrongMethodBeforeExecution(t *testing.T) {
	runner := &stubExecutor{}
	handler := testResponsesHandlerWith(runner)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodPost || runner.calls != 0 {
		t.Fatalf("status=%d allow=%q calls=%d", rr.Code, rr.Header().Get("Allow"), runner.calls)
	}
}

func testResponsesHandler(output executor.Output, err ...error) http.Handler {
	runner := &stubExecutor{output: output}
	if len(err) != 0 {
		runner.err = err[0]
	}
	return testResponsesHandlerWith(runner)
}

func testResponsesHandlerWith(runner *stubExecutor) http.Handler {
	return Authenticate(NewResponsesHandler(runner, ResponsesConfig{MaxBodyBytes: 1 << 20, Models: []domain.Model{{ID: "claude-test", Tier: domain.T4}}}), staticTokens{{ID: "client_test", Value: "test-token"}})
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
}

func (s *stubExecutor) Execute(context.Context, executor.Input) (executor.Output, error) {
	s.calls++
	return s.output, s.err
}

func (s *stubExecutor) Stream(context.Context, executor.Input, executor.EventWriter) error {
	s.calls++
	return s.err
}
