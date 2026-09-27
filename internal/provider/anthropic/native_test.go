package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/api"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

// TestNativeMessagesRoundTrip decodes a caller Messages request with the
// gateway decoder and checks the provider request preserves it verbatim.
func TestNativeMessagesRoundTrip(t *testing.T) {
	body := `{"model":"claude-test","max_tokens":64,
		"temperature":1,
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"name":"lookup","description":"find","input_schema":{"type":"object"},"defer_loading":true},{"name":"plain","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"find a tool"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"","signature":"sig"},
				{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_regex","input":{"pattern":"look"}},
				{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"lookup"}]}},
				{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"tool_reference","tool_name":"lookup"},{"type":"text","text":"failed"}],"cache_control":{"type":"ephemeral"}}]},
			{"role":"system","content":[{"type":"text","text":"remember"}],"output_config":{"effort":"low"}},
			{"role":"assistant","content":[{"type":"text","text":"searching"},{"type":"server_tool_use","id":"srvtoolu_2","name":"tool_search_tool_regex","input":{"pattern":"y"}}]}
		]}`
	decoded, err := api.DecodeMessagesRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	request, err := toMessagesRequest(anthropicModel(), decoded.Request, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider request differs:\n got=%s", encoded)
	}
}

func TestAPIKeyForwardsCallerBetaWithoutOAuthFlags(t *testing.T) {
	for _, test := range []struct{ name, beta, want string }{
		{name: "caller beta", beta: "oauth-2025-04-20, advanced-tool-use-2025-11-20,context-management-2025-06-27", want: "advanced-tool-use-2025-11-20,context-management-2025-06-27"},
		{name: "oauth only", beta: "oauth-2025-04-20"},
		{name: "none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Values("anthropic-beta")
				_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
			}))
			defer server.Close()
			ctx := upstreamauth.WithAnthropicBeta(context.Background(), test.beta)
			if _, err := newAnthropic(server.URL).Execute(ctx, anthropicModel(), inference.Request{Model: "m", Input: []inference.Item{{Type: "message", Role: "user", Text: "hi"}}}); err != nil {
				t.Fatal(err)
			}
			if (test.want == "" && len(got) != 0) || (test.want != "" && (len(got) != 1 || got[0] != test.want)) {
				t.Fatalf("anthropic-beta=%q, want %q", got, test.want)
			}
		})
	}
}

func TestNativeOnlyResponseUsesTextlessCarrier(t *testing.T) {
	content := `{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_regex","input":{"pattern":"x"}}`
	result := fromMessagesResponse(messagesResponse{ID: "msg_1", Content: []json.RawMessage{json.RawMessage(content)}, StopReason: json.RawMessage(`"pause_turn"`)}, anthropicModel(), "")
	if len(result.Output) != 1 || result.Output[0].Text != "" || result.Output[0].ContinuationProvider != "anthropic" {
		t.Fatalf("output=%+v", result.Output)
	}
	request, err := toMessagesRequest(anthropicModel(), inference.Request{Model: "m", Input: []inference.Item{{Type: "message", Role: "user", Text: "find"}, result.Output[0]}}, false)
	if err != nil {
		t.Fatal(err)
	}
	replayed, _ := json.Marshal(request.Messages[1])
	if string(replayed) != `{"role":"assistant","content":[`+content+`]}` {
		t.Fatalf("replayed=%s", replayed)
	}
}
