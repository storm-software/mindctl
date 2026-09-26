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
	var seen compressionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sidecar-token" {
			t.Fatal("missing sidecar token")
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatal(err)
		}
		if len(seen.Messages) != 2 {
			t.Fatalf("sidecar request messages = %+v", seen)
		}
		seen.Messages[0].Text = "compressed assistant"
		seen.Messages[1].Text = "compressed tool output"
		_ = json.NewEncoder(w).Encode(compressionResponse{
			Messages: seen.Messages,
			Metrics:  Metrics{TokensBefore: 10, TokensAfter: 6, TokensSaved: 4},
		})
	}))
	defer server.Close()

	request := inference.Request{Model: "public-model", Instructions: "keep", Input: []inference.Item{
		{Type: "message", Role: "user", Text: "do not rewrite"},
		{Type: "message", Role: "assistant", Text: "assistant text", CallID: "call-1", ProviderData: json.RawMessage(`{"opaque":true}`)},
		{Type: "function_call_output", Text: "tool output", CallID: "call-1"},
	}}
	original, _ := json.Marshal(request)
	compressed, metrics, err := NewClient(server.URL, "sidecar-token", nil).Compress(
		context.Background(), domain.Model{ID: "configured", UpstreamID: "upstream-model"}, "conversation", "openai", request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Model != "upstream-model" || seen.Messages[0].Text != "compressed assistant" || len(seen.Messages) != 2 {
		t.Fatalf("sidecar request = %+v", seen)
	}
	if compressed.Input[0].Text != "do not rewrite" || compressed.Input[1].Text != "compressed assistant" || compressed.Input[2].Text != "compressed tool output" {
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
