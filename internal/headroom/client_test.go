package headroom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

func TestClientCompressPreservesCanonicalFields(t *testing.T) {
	var seen struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sidecar-token" {
			t.Fatal("missing sidecar token")
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatal(err)
		}
		if len(seen.Messages) != 3 || seen.Messages[0].Role != "user" || seen.Messages[0].Content != "do not rewrite" || seen.Messages[1].Role != "assistant" || seen.Messages[1].Content != "assistant text" || seen.Messages[2].Role != "tool" || seen.Messages[2].ToolCallID != "call-1" || seen.Messages[2].Content != "tool output" {
			t.Fatalf("sidecar request messages = %+v", seen)
		}
		_, _ = w.Write([]byte(`{"messages":[{"role":"user","content":"do not rewrite"},{"role":"assistant","content":"compressed assistant"},{"role":"tool","tool_call_id":"call-1","content":"compressed tool output"}],"tokens_before":10,"tokens_after":6,"tokens_saved":4}`))
	}))
	defer server.Close()

	request := inference.Request{Model: "public-model", Instructions: "keep", Input: []inference.Item{
		{Type: "message", Role: "user", Text: "do not rewrite"},
		{Type: "message", Role: "assistant", Text: "assistant text"},
		{Type: "function_call_output", Output: json.RawMessage(`"tool output"`), CallID: "call-1"},
	}}
	original, _ := json.Marshal(request)
	compressed, metrics, err := NewClient(server.URL, "sidecar-token", nil).Compress(
		context.Background(), domain.Model{ID: "configured", UpstreamID: "upstream-model"}, "conversation", "openai", request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Model != "upstream-model" {
		t.Fatalf("sidecar request = %+v", seen)
	}
	if compressed.Input[0].Text != "do not rewrite" || compressed.Input[1].Text != "compressed assistant" || string(compressed.Input[2].Output) != `"compressed tool output"` {
		t.Fatalf("compressed request = %+v", compressed.Input)
	}
	if metrics.TokensSaved != 4 {
		t.Fatalf("metrics = %+v", metrics)
	}
	unchanged, _ := json.Marshal(request)
	if string(original) != string(unchanged) {
		t.Fatal("Compress mutated the original request")
	}
}

func TestClientRejectsMissingOrInvalidMetrics(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"assistant","content":"text"}]}`,
		`{"messages":[{"role":"assistant","content":"text"}],"tokens_before":10,"tokens_after":6}`,
		`{"messages":[{"role":"assistant","content":"text"}],"tokens_before":10,"tokens_after":6,"tokens_saved":0}`,
		`{"messages":[{"role":"assistant","content":"text"}],"tokens_before":-1,"tokens_after":0,"tokens_saved":0}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			_, _, err := NewClient(server.URL, "token", nil).Compress(context.Background(), domain.Model{UpstreamID: "model"}, "conversation", "provider", inference.Request{Input: []inference.Item{{Type: "message", Role: "assistant", Text: "text"}}})
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestClientReadyRequiresTopLevelMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[],"metrics":{"tokens_before":0,"tokens_after":0,"tokens_saved":0}}`))
	}))
	defer server.Close()
	if err := NewClient(server.URL, "token", nil).Ready(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ready accepted incompatible response: %v", err)
	}
}

func TestClientRejectsUnsafeCompression(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		code int
	}{
		{name: "skipped", body: `{"compression_skipped":true,"messages":[]}`, code: http.StatusOK},
		{name: "malformed", body: `{`, code: http.StatusOK},
		{name: "server error", body: `private response`, code: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, _, err := NewClient(server.URL, "token", nil).Compress(context.Background(), domain.Model{ID: "model"}, "conversation", "provider", inference.Request{Input: []inference.Item{{Type: "message", Role: "assistant", Text: "text"}}})
			if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private response") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSessionIDIncludesProviderAndModel(t *testing.T) {
	first := sessionID("conversation", "openai", "model-a")
	second := sessionID("conversation", "anthropic", "model-a")
	third := sessionID("conversation", "openai", "model-b")
	if first == second || first == third || len(first) != 64 {
		t.Fatalf("session IDs are not isolated: %q %q %q", first, second, third)
	}
}
