package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
)

func TestMessageFromResult(t *testing.T) {
	for _, test := range []struct {
		name, provider, stop string
		result               inference.Result
		wantBlocks           int
		wantError            bool
	}{
		{name: "portable text", provider: "openai", stop: "end_turn", wantBlocks: 1, result: inference.Result{ID: "msg_1", Model: "gpt-test", Status: "completed", Output: []inference.Item{{Type: "message", Text: "hi"}}}},
		{name: "portable tool", provider: "gemini", stop: "tool_use", wantBlocks: 2, result: inference.Result{Model: "gemini-test", Output: []inference.Item{{Type: "message", Text: "looking"}, {Type: "function_call", CallID: "call_1", Name: "search", Arguments: []byte(`{"q":"hi"}`)}}}},
		{name: "native signature", provider: "anthropic", stop: "end_turn", wantBlocks: 2, result: inference.Result{Model: "claude-test", Status: "completed", Output: []inference.Item{{Type: "message", Text: "hi", ProviderData: []byte(`{"anthropic_content_block":{"type":"text","text":"hi"},"anthropic_prefix":[{"type":"thinking","thinking":"secret","signature":"signed"}]}`)}}}},
		{name: "omitted thinking retains empty text", provider: "anthropic", stop: "end_turn", wantBlocks: 2, result: inference.Result{Model: "claude-test", Status: "completed", Output: []inference.Item{{Type: "message", Text: "hi", ProviderData: []byte(`{"anthropic_content_block":{"type":"text","text":"hi"},"anthropic_prefix":[{"type":"thinking","thinking":"","signature":"signed"}]}`)}}}},
		{name: "native block is not portable", provider: "openai", stop: "end_turn", wantBlocks: 1, result: inference.Result{Status: "completed", Output: []inference.Item{{Type: "message", Text: "hi", ProviderData: []byte(`{"anthropic_prefix":[{"type":"thinking","signature":"signed"}]}`)}}}},
		{name: "unknown native output", provider: "anthropic", result: inference.Result{Output: []inference.Item{{Type: "message", Text: "hi", ProviderData: []byte(`{"anthropic_prefix":[{"type":"unexpected"}],"anthropic_content_block":{"type":"text","text":"hi"}}`)}}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := messageFromResult(test.result, test.provider)
			if (err != nil) != test.wantError {
				t.Fatalf("err=%v", err)
			}
			if test.wantError {
				return
			}
			if body.StopReason != test.stop || len(body.Content) != test.wantBlocks {
				t.Fatalf("body=%+v", body)
			}
			if test.name == "omitted thinking retains empty text" {
				encoded, _ := json.Marshal(body)
				if !strings.Contains(string(encoded), `"thinking":""`) {
					t.Fatalf("missing empty signed thinking: %s", encoded)
				}
			}
		})
	}
}

func TestWriteMessagesError(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "invalid", err: ErrInvalidRequest, want: http.StatusBadRequest},
		{name: "auth", err: ErrUnauthorized, want: http.StatusUnauthorized},
		{name: "no model", err: &router.NoEligibleModelError{}, want: http.StatusBadRequest},
		{name: "rate", err: &provider.Error{Kind: provider.ErrorRateLimit, Err: errors.New("private oauth")}, want: http.StatusTooManyRequests},
		{name: "server", err: &provider.Error{Kind: provider.ErrorRetryable, Err: errors.New("private oauth")}, want: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeMessagesError(response, test.err)
			var body struct {
				Type  string `json:"type"`
				Error struct {
					Type, Message string
				} `json:"error"`
			}
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != test.want || body.Type != "error" || body.Error.Type == "" || strings.Contains(response.Body.String(), "private") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
