package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeMessagesRequest(t *testing.T) {
	for _, test := range []struct {
		name, body, required string
		wantItems            int
		wantError            bool
	}{
		{name: "text and tools", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"run","input":{"arg":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}],"tools":[{"name":"run","input_schema":{"type":"object"}}],"stream":true}`, wantItems: 3},
		{name: "signed thinking", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"signature"},{"type":"text","text":"hi"}]}]}`, required: "anthropic", wantItems: 1},
		{name: "omitted signed thinking", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signature"},{"type":"text","text":"hi"}]}]}`, required: "anthropic", wantItems: 1},
		{name: "missing max tokens", body: `{"model":"mindctl-auto","messages":[{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "null system", body: `{"model":"mindctl-auto","max_tokens":42,"system":null,"messages":[{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "null message content", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":null},{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "empty message content", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":[]},{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "invalid role", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"system","content":"hi"}]}`, wantError: true},
		{name: "unknown parameter", body: `{"model":"mindctl-auto","max_tokens":1,"messages":[],"temperature":0.7}`, wantError: true},
		{name: "trailing JSON", body: `{} {}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(test.body))
			got, err := DecodeMessagesRequest(httptest.NewRecorder(), request, 1<<20)
			if (err != nil) != test.wantError {
				t.Fatalf("err=%v", err)
			}
			if !test.wantError && (len(got.Request.Input) != test.wantItems || got.RequiredProvider != test.required || got.Request.MaxOutputTokens != 42) {
				t.Fatalf("decoded=%+v", got)
			}
		})
	}
}

func TestDecodeMessagesRequestEnforcesBodyLimit(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":"hi"}]}`))
	_, err := DecodeMessagesRequest(httptest.NewRecorder(), request, 10)
	if err == nil || !strings.Contains(err.Error(), "body too large") {
		t.Fatalf("body limit error=%v", err)
	}
}

func TestDecodeMessagesNativeControlsRetainProvenance(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"mindctl-auto","max_tokens":1000,
		"thinking":{"type":"enabled","budget_tokens":500},
		"system":[{"type":"text","text":"instructions","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"question"}],
		"tools":[{"name":"search","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"tool_choice":{"type":"tool","name":"search"}
	}`))
	got, err := DecodeMessagesRequest(httptest.NewRecorder(), request, 1<<20)
	if err != nil || got.RequiredProvider != "anthropic" || got.Request.Thinking == nil || got.Request.Thinking.BudgetTokens != 500 || len(got.Request.AnthropicSystem) != 1 || len(got.Request.Tools) != 1 || len(got.Request.Tools[0].CacheControl) == 0 || got.Request.AnthropicToolChoice == nil || got.Request.AnthropicToolChoice.Name != "search" {
		t.Fatalf("decoded=%+v err=%v", got, err)
	}
}
