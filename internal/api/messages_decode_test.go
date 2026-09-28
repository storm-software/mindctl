package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeMessagesRequest(t *testing.T) {
	for _, test := range []struct {
		name, body, required string
		wantItems            int
		wantError, native    bool
	}{
		{name: "text and tools", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"run","input":{"arg":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}],"tools":[{"name":"run","input_schema":{"type":"object"}}],"stream":true}`, wantItems: 3},
		{name: "signed thinking", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"signature"},{"type":"text","text":"hi"}]}]}`, native: true, wantItems: 1},
		{name: "omitted signed thinking", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signature"},{"type":"text","text":"hi"}]}]}`, native: true, wantItems: 1},
		{name: "missing max tokens", body: `{"model":"mindctl-auto","messages":[{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "null system", body: `{"model":"mindctl-auto","max_tokens":42,"system":null,"messages":[{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "null message content", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":null},{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "empty message content", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":[]},{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "invalid role", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"developer","content":"hi"}]}`, wantError: true},
		{name: "system block extra field", body: `{"model":"mindctl-auto","max_tokens":42,"system":[{"type":"text","text":"instructions","citations":null}],"messages":[{"role":"user","content":"hi"}]}`, native: true, wantItems: 1},
		{name: "system non-text block", body: `{"model":"mindctl-auto","max_tokens":42,"system":[{"type":"image","source":{}}],"messages":[{"role":"user","content":"hi"}]}`, wantError: true},
		{name: "system role", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":"hi"},{"role":"system","content":"mid-conversation"}]}`, native: true, wantItems: 2},
		{name: "unknown parameter", body: `{"model":"mindctl-auto","max_tokens":42,"messages":[{"role":"user","content":"hi"}],"temperature":0.7}`, native: true, wantItems: 1},
		{name: "no messages", body: `{"model":"mindctl-auto","max_tokens":1,"messages":[],"temperature":0.7}`, wantError: true},
		{name: "trailing JSON", body: `{} {}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(test.body))
			got, err := DecodeMessagesRequest(httptest.NewRecorder(), request, 1<<20)
			if (err != nil) != test.wantError {
				t.Fatalf("err=%v", err)
			}
			if !test.wantError && (len(got.Request.Input) != test.wantItems || got.RequiredProvider != test.required || got.Native != test.native || got.Request.MaxOutputTokens != 42) {
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

func TestDecodeMessagesSystemKeepsUnknownBlockFields(t *testing.T) {
	got, err := decodeMessagesBody(t, `{"model":"mindctl-auto","max_tokens":42,"system":[{"type":"text","text":"a"},{"type":"text","text":"b","citations":null,"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	if err != nil || got.Request.Instructions != "a\nb" || len(got.Request.AnthropicSystem) != 2 {
		t.Fatalf("decoded=%+v err=%v", got, err)
	}
	encoded, err := json.Marshal(got.Request.AnthropicSystem)
	if err != nil || string(encoded) != `[{"type":"text","text":"a"},{"type":"text","text":"b","citations":null,"cache_control":{"type":"ephemeral"}}]` {
		t.Fatalf("encoded=%s err=%v", encoded, err)
	}
}

func decodeMessagesBody(t *testing.T, body string) (DecodedMessages, error) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	return DecodeMessagesRequest(httptest.NewRecorder(), request, 1<<20)
}

func TestDecodeMessagesRecordedClaudeCodeRequest(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "claude_code_2_1_283_tool_search.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeMessagesBody(t, string(body))
	if err != nil {
		t.Fatal(err)
	}
	request := got.Request
	if got.RequiredProvider != "" || !got.Native || request.Thinking == nil || request.Thinking.Type != "adaptive" || request.Thinking.Display != "omitted" {
		t.Fatalf("thinking=%+v required=%q", request.Thinking, got.RequiredProvider)
	}
	for _, name := range []string{"metadata", "context_management", "output_config"} {
		if len(request.AnthropicExtra[name]) == 0 {
			t.Fatalf("top-level %s was not retained: %v", name, request.AnthropicExtra)
		}
	}
	if len(request.Tools) != 3 || request.Tools[0].AnthropicNative != nil || !strings.Contains(string(request.Tools[2].AnthropicNative), `"defer_loading"`) {
		t.Fatalf("tools=%+v", request.Tools)
	}
	var systemTurns int
	for _, item := range request.Input {
		if item.Role == "system" {
			systemTurns++
			if item.ContinuationProvider != "anthropic" || len(item.ProviderData) == 0 {
				t.Fatalf("system turn is not native: %+v", item)
			}
		}
	}
	if systemTurns != 2 {
		t.Fatalf("system turns=%d", systemTurns)
	}
}

func TestDecodeMessagesForwardsNativeToolSearchContent(t *testing.T) {
	got, err := decodeMessagesBody(t, `{"model":"mindctl-auto","max_tokens":42,
		"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"name":"lookup","input_schema":{"type":"object"},"defer_loading":true}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true},
		"messages":[
			{"role":"user","content":"find a tool"},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_regex","input":{"pattern":"look"}},
				{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"lookup"}]}},
				{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"tool_reference","tool_name":"lookup"},{"type":"text","text":"failed"}]}]},
			{"role":"assistant","content":[{"type":"text","text":"searching"},{"type":"server_tool_use","id":"srvtoolu_2","name":"tool_search_tool_regex","input":{"pattern":"x"}}]}
		]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequiredProvider != "" || !got.Native || got.Request.AnthropicToolChoice == nil || got.Request.ParallelToolCalls == nil || *got.Request.ParallelToolCalls || len(got.Request.Tools) != 2 || got.Request.Tools[0].AnthropicNative == nil || got.Request.Tools[1].AnthropicNative == nil {
		t.Fatalf("decoded=%+v", got)
	}
	input := got.Request.Input
	if len(input) != 4 {
		t.Fatalf("items=%+v", input)
	}
	for index, want := range []string{"", `"anthropic_prefix":[{"type":"server_tool_use"`, `"is_error":true`, `"anthropic_suffix":[{"type":"server_tool_use"`} {
		if !strings.Contains(string(input[index].ProviderData), want) {
			t.Fatalf("item %d provider data=%s, want %s", index, input[index].ProviderData, want)
		}
	}
	if string(input[2].Output) != `"failed"` {
		t.Fatalf("portable tool output=%s", input[2].Output)
	}
}

func TestDecodeMessagesReportsSpecificReasons(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{name: "role", body: `{"model":"m","max_tokens":1,"messages":[{"role":"developer","content":"hi"}]}`, want: `messages[0].role: unsupported message role "developer"`},
		{name: "thinking type", body: `{"model":"m","max_tokens":1,"thinking":{"type":"sometimes"},"messages":[{"role":"user","content":"hi"}]}`, want: `thinking.type: unsupported thinking type "sometimes"`},
		{name: "tool schema", body: `{"model":"m","max_tokens":1,"tools":[{"name":"run"}],"messages":[{"role":"user","content":"hi"}]}`, want: "tools[0].input_schema: is required"},
		{name: "only native blocks", body: `{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"n","input":{}}]}]}`, want: "messages[0].content: must include a text, image, tool_use, or tool_result block"},
		{name: "system non-text", body: `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":[{"type":"image","source":{}}]}]}`, want: `messages[0].content[0]: system messages support only text blocks, got "image"`},
		{name: "field type", body: `{"model":"m","max_tokens":"many","messages":[]}`, want: "max_tokens: has an invalid JSON type string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeMessagesBody(t, test.body)
			if err == nil || err.Error() != test.want {
				t.Fatalf("err=%v, want %s", err, test.want)
			}
		})
	}
}

func TestMessagesHandlerReturnsAndLogsValidationReason(t *testing.T) {
	var logs bytes.Buffer
	cfg := ResponsesConfig{MaxBodyBytes: 1 << 20, Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	handler := NewMessagesHandler(&messagesExecutor{}, cfg)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"mindctl-auto","max_tokens":8,"messages":[{"role":"developer","content":"private prompt"}]}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), clientIDContextKey{}, "client")))
	want := `messages[0].role: unsupported message role \"developer\"`
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), want) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(logs.String(), `"msg":"messages.request.rejected"`) || !strings.Contains(logs.String(), want) || strings.Contains(logs.String(), "private prompt") {
		t.Fatalf("logs=%s", logs.String())
	}
}
