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
)

const testSessionID = "0f5e7c1a-3b2d-4e6f-8a9b-1c2d3e4f5a6b"

func userItems(texts ...string) []inference.Item {
	items := make([]inference.Item, 0, len(texts))
	for _, text := range texts {
		items = append(items, inference.Item{Type: "message", Role: "user", Text: text})
	}
	return items
}

func TestSessionKeyDerivation(t *testing.T) {
	first := append(userItems("<system-reminder>context</system-reminder>", "fix the parser"),
		inference.Item{Type: "message", Role: "assistant", Text: "done"},
		inference.Item{Type: "message", Role: "user", Text: "now add tests"},
	)
	key := sessionKey("client", testSessionID, first)
	if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
		t.Fatalf("sessionKey() = %q; want a hex SHA-256 digest", key)
	}
	if strings.Contains(key, testSessionID) {
		t.Fatal("session key contains the raw client session ID")
	}
	if got := sessionKey("client", testSessionID, first[:2]); got != key {
		t.Fatal("later turns changed the session key")
	}
	for _, tc := range []struct {
		name            string
		client, session string
		input           []inference.Item
	}{
		{"sub-agent first message", "client", testSessionID, userItems("<system-reminder>context</system-reminder>", "explore the repo")},
		{"shared preamble only", "client", testSessionID, userItems("<system-reminder>context</system-reminder>")},
		{"other client", "other", testSessionID, first},
		{"other session", "client", "1f5e7c1a-3b2d-4e6f-8a9b-1c2d3e4f5a6b", first},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionKey(tc.client, tc.session, tc.input); got == "" || got == key {
				t.Fatalf("sessionKey() = %q; want a distinct key", got)
			}
		})
	}
	contentParts := []inference.Item{{Type: "message", Role: "user", Content: []inference.ContentPart{{Type: "input_text", Text: "fix the parser"}}}}
	if sessionKey("client", testSessionID, contentParts) == "" {
		t.Fatal("content-part user text did not produce a session key")
	}
	for name, input := range map[string][]inference.Item{
		"no user text":        {{Type: "input_image", Role: "user", ImageURL: []byte(`"data:image/png;base64,AA=="`)}},
		"assistant first":     {{Type: "message", Role: "assistant", Text: "hello"}},
		"empty input":         nil,
		"developer then none": {{Type: "message", Role: "developer", Text: "rules"}},
	} {
		if got := sessionKey("client", testSessionID, input); got != "" {
			t.Fatalf("%s: sessionKey() = %q; want no key", name, got)
		}
	}
	if sessionKey("", testSessionID, first) != "" || sessionKey("client", "", first) != "" {
		t.Fatal("missing client or session ID produced a key")
	}
}

func TestClientSessionIDPrecedence(t *testing.T) {
	metadataID := "aaaaaaaa-3b2d-4e6f-8a9b-1c2d3e4f5a6b"
	mindctlID := "bbbbbbbb-3b2d-4e6f-8a9b-1c2d3e4f5a6b"
	claudeID := "cccccccc-3b2d-4e6f-8a9b-1c2d3e4f5a6b"
	codexID := "dddddddd-3b2d-4e6f-8a9b-1c2d3e4f5a6b"
	all := map[string]string{
		"X-Mindctl-Session-Id":     mindctlID,
		"X-Claude-Code-Session-Id": claudeID,
		"Session-Id":               codexID,
	}
	for _, tc := range []struct {
		name     string
		metadata string
		drop     []string
		want     string
	}{
		{"metadata session_id", `{"session_id":"` + metadataID + `"}`, nil, metadataID},
		{"metadata sessionId", `{"sessionId":"` + metadataID + `"}`, nil, metadataID},
		{"metadata conversation_id", `{"conversation_id":"` + metadataID + `"}`, nil, metadataID},
		{"non-json metadata ignored", "user_abc_account__session_" + metadataID, nil, mindctlID},
		{"mindctl header", "", nil, mindctlID},
		{"claude code header", "", []string{"X-Mindctl-Session-Id"}, claudeID},
		{"codex header", "", []string{"X-Mindctl-Session-Id", "X-Claude-Code-Session-Id"}, codexID},
		{"none", "", []string{"X-Mindctl-Session-Id", "X-Claude-Code-Session-Id", "Session-Id"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", nil)
			for name, value := range all {
				request.Header.Set(name, value)
			}
			for _, name := range tc.drop {
				request.Header.Del(name)
			}
			if got := clientSessionID(request, tc.metadata); got != tc.want {
				t.Fatalf("clientSessionID() = %q; want %q", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"too short", []string{"123456789012345"}},
		{"too long", []string{strings.Repeat("a", 129)}},
		{"padded", []string{" " + codexID}},
		{"inner space", []string{"aaaaaaaa bbbbbbbbbbbb"}},
		{"conflicting repeats", []string{codexID, claudeID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", nil)
			for _, value := range tc.values {
				request.Header.Add("Session-Id", value)
			}
			if got := clientSessionID(request, ""); got != "" {
				t.Fatalf("clientSessionID() = %q; want none", got)
			}
		})
	}
	repeated := httptest.NewRequest(http.MethodPost, "/", nil)
	repeated.Header.Add("Session-Id", codexID)
	repeated.Header.Add("Session-Id", codexID)
	if got := clientSessionID(repeated, ""); got != codexID {
		t.Fatalf("identical repeated header = %q; want %q", got, codexID)
	}
}

func TestHandlersSetSessionKeyOnlyForAutomaticStatelessRequests(t *testing.T) {
	models := []domain.Model{{ID: "claude-haiku-4-5", Provider: "anthropic", Tier: domain.T1}}
	settings := executor.SessionSettings{HorizonTurns: 3, DefaultCacheHitRatio: .8}
	messagesCfg := ResponsesConfig{MaxBodyBytes: 1 << 20, Models: models, SessionEnabled: true, ProviderCredentials: map[string]bool{},
		ExpectedOutputTokens: 2_000, Session: settings, ClaudeOAuthProviders: map[string]bool{"anthropic": true}}
	serveMessages := func(t *testing.T, cfg ResponsesConfig, body string) *messagesExecutor {
		t.Helper()
		runner := &messagesExecutor{}
		handler := AuthenticateWithError(CaptureNativeClaudeOAuth(NewMessagesHandler(runner, cfg)), "X-Mindctl-Token", staticTokens{{ID: "client", Value: "gateway"}}, writeMessagesError)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("X-Mindctl-Token", "gateway")
		request.Header.Set("X-Claude-Code-Session-Id", testSessionID)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return runner
	}
	automatic := `{"model":"mindctl-auto","max_tokens":32,"metadata":{"user_id":"opaque"},"messages":[{"role":"user","content":"hi"}]}`

	runner := serveMessages(t, messagesCfg, automatic)
	want := sessionKey("client", testSessionID, userItems("hi"))
	if runner.input.Request.SessionKey != want {
		t.Fatalf("automatic Messages SessionKey = %q; want %q", runner.input.Request.SessionKey, want)
	}
	if runner.input.ExpectedOutputTokens != 2_000 || runner.input.Session != settings {
		t.Fatalf("cost estimate settings were not passed through: %+v %+v", runner.input.ExpectedOutputTokens, runner.input.Session)
	}
	if string(runner.input.Request.AnthropicExtra["metadata"]) != `{"user_id":"opaque"}` {
		t.Fatalf("metadata was not forwarded: %s", runner.input.Request.AnthropicExtra["metadata"])
	}
	explicit := strings.Replace(automatic, "mindctl-auto", "claude-haiku-4-5-20251001", 1)
	if got := serveMessages(t, messagesCfg, explicit).input.Request.SessionKey; got != "" {
		t.Fatalf("explicit Messages SessionKey = %q; want none", got)
	}
	disabled := messagesCfg
	disabled.SessionEnabled = false
	if got := serveMessages(t, disabled, automatic).input.Request.SessionKey; got != "" {
		t.Fatalf("disabled Messages SessionKey = %q; want none", got)
	}

	serveResponses := func(t *testing.T, body string) inference.Request {
		t.Helper()
		runner := &stubExecutor{}
		handler := NewResponsesHandler(runner, ResponsesConfig{MaxBodyBytes: 1 << 20, SessionEnabled: true})
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		request.Header.Set("Session-Id", testSessionID)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), clientIDContextKey{}, "client")))
		if runner.calls != 1 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return runner.input.Request
	}
	if got := serveResponses(t, `{"model":"mindctl-auto","store":false,"input":"hi"}`).SessionKey; got == "" {
		t.Fatal("automatic stateless Responses request has no SessionKey")
	}
	if got := serveResponses(t, `{"model":"mindctl-auto","store":false,"previous_response_id":"resp_abcdefghijklmnopqrstuvwx","input":"hi"}`).SessionKey; got != "" {
		t.Fatalf("previous_response_id Responses SessionKey = %q; want none", got)
	}
}
