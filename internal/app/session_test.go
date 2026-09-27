package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/config"
)

// upstreamCall is what the fake Anthropic upstream observed for one request.
type upstreamCall struct {
	model    string
	messages int
}

type sessionUpstream struct {
	mu              sync.Mutex
	classifierCalls int
	calls           []upstreamCall
}

func (u *sessionUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch r.URL.Path {
	case "/v1/classify":
		// An unavailable classifier makes routing use the safe fallback
		// tier, so each call is observable without a Laya response fixture.
		u.classifierCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	case "/v1/messages":
		var body struct {
			Model    string            `json:"model"`
			Messages []json.RawMessage `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		u.calls = append(u.calls, upstreamCall{model: body.Model, messages: len(body.Messages)})
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":5}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (u *sessionUpstream) snapshot() (int, []upstreamCall) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.classifierCalls, append([]upstreamCall(nil), u.calls...)
}

func TestClaudeCodeSessionAffinityEndToEnd(t *testing.T) {
	upstream := &sessionUpstream{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	cfg, env := fixture(t)
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.ClaudeMessages.Enabled = true
	cfg.Classifier.Endpoint = server.URL
	cfg.Providers[0] = config.ProviderConfig{ID: "anthropic", BaseURL: server.URL, Auth: string(config.ProviderAuthClaudeOAuthPassthrough)}
	delete(env, "TEST_PROVIDER_KEY")
	model := func(id, tier string, price float64) config.ModelConfig {
		return config.ModelConfig{ID: id, Provider: "anthropic", Tier: tier, Available: true, Capabilities: []string{"chat", "tools"},
			ContextWindow: 200_000, MaxOutputTokens: 1_024, InputPrice: price, OutputPrice: 5 * price, SuccessPrior: 1}
	}
	cfg.Models = []config.ModelConfig{model("claude-haiku-4-5", "T1", 1), model("claude-opus-5", "T4", 5), model("claude-fable-5-1", "T6", 10)}
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	const sessionID = "0f5e7c1a-3b2d-4e6f-8a9b-1c2d3e4f5a6b"
	send := func(modelID string, messages ...string) {
		t.Helper()
		body := `{"model":"` + modelID + `","max_tokens":32,"messages":[` + strings.Join(messages, ",") + `]}`
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("X-Mindctl-Token", env["TEST_GATEWAY_TOKEN"])
		request.Header.Set("Authorization", "Bearer native-secret")
		request.Header.Set("X-Claude-Code-Session-Id", sessionID)
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	expect := func(step string, classifierCalls int, lastModel string, lastMessages int) {
		t.Helper()
		calls, upstreamCalls := upstream.snapshot()
		last := upstreamCalls[len(upstreamCalls)-1]
		if calls != classifierCalls || last.model != lastModel || last.messages != lastMessages {
			t.Fatalf("%s: classifier calls=%d last upstream=%+v; want %d calls, %s with %d messages",
				step, calls, last, classifierCalls, lastModel, lastMessages)
		}
	}
	sessions := func() int {
		t.Helper()
		var count int
		if err := a.store.SQL().QueryRow("SELECT count(*) FROM routing_sessions").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	user := func(text string) string { return `{"role":"user","content":"` + text + `"}` }
	toolUse := func(id string) string {
		return `{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"read","input":{}}]}`
	}
	toolResult := func(id string) string {
		return `{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"contents"},` +
			`{"type":"text","text":"<system-reminder>files changed</system-reminder>"}]}`
	}
	reply := `{"role":"assistant","content":"done"}`

	// The classifier is down, so turn 1 routes at the safe fallback tier.
	turn1 := []string{user("fix the parser")}
	send("mindctl-auto", turn1...)
	expect("first turn", 1, "claude-opus-5", 1)

	turn2 := append(append([]string(nil), turn1...), toolUse("toolu_1"), toolResult("toolu_1"))
	send("mindctl-auto", turn2...)
	expect("tool continuation", 1, "claude-opus-5", 3)

	turn3 := append(append([]string(nil), turn2...), reply, user("now add tests"))
	send("mindctl-auto", turn3...)
	expect("new user turn", 2, "claude-opus-5", 5)

	send("mindctl-auto", user("explore the repo"))
	expect("sub-agent session", 3, "claude-opus-5", 1)
	if got := sessions(); got != 2 {
		t.Fatalf("routing sessions=%d; want main thread and sub-agent", got)
	}

	var before int64
	if err := a.store.SQL().QueryRow("SELECT sum(updated_at) FROM routing_sessions").Scan(&before); err != nil {
		t.Fatal(err)
	}
	send("claude-haiku-4-5-20251001", user("summarize the title"))
	expect("explicit background call", 3, "claude-haiku-4-5", 1)
	var after int64
	if err := a.store.SQL().QueryRow("SELECT sum(updated_at) FROM routing_sessions").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if sessions() != 2 || after != before {
		t.Fatal("an explicit-model request read or wrote session state")
	}

	turn4 := append(append([]string(nil), turn3...), toolUse("toolu_2"), toolResult("toolu_2"))
	send("mindctl-auto", turn4...)
	expect("continuation after background call", 3, "claude-opus-5", 7)

	idle := (2 * time.Hour).Nanoseconds()
	if _, err := a.store.SQL().Exec("UPDATE routing_sessions SET updated_at = updated_at - ?", idle); err != nil {
		t.Fatal(err)
	}
	turn5 := append(append([]string(nil), turn4...), toolUse("toolu_3"), toolResult("toolu_3"))
	send("mindctl-auto", turn5...)
	expect("continuation after idle expiry", 4, "claude-opus-5", 9)
}
