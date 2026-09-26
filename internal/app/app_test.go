package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage"
)

func fixture(t *testing.T) (config.Config, map[string]string) {
	t.Helper()
	return config.Config{
			Listen: "127.0.0.1:0", ClientAuth: config.ClientAuthConfig{TokenEnv: "TEST_GATEWAY_TOKEN"},
			Classifier: config.ClassifierConfig{Endpoint: "https://laya.example.com", TokenEnv: "TEST_LAYA_TOKEN"},
			SQLite:     config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "gateway.db")},
			Encryption: config.EncryptionConfig{ActiveKeyID: "active", Keys: map[string]string{"active": "TEST_ENCRYPTION_KEY", "old": "TEST_OLD_KEY"}},
			Routing:    config.RoutingConfig{MinTier: "T0", MaxTier: "T6"},
			Providers:  []config.ProviderConfig{{ID: "openai", BaseURL: "https://provider.example.com", APIKeyEnv: "TEST_PROVIDER_KEY"}},
			Models:     []config.ModelConfig{{ID: "first", Provider: "openai", Tier: "T4", Available: true, Capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, ContextWindow: 32000, InputPrice: 2, OutputPrice: 8, SuccessPrior: .9, TaskSuccessPriors: map[string]float64{"coding": .95}}},
		}, map[string]string{
			"TEST_GATEWAY_TOKEN": "private-gateway-token", "TEST_LAYA_TOKEN": "private-laya-token", "TEST_PROVIDER_KEY": "private-provider-token",
			"TEST_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
			"TEST_OLD_KEY":        base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		}
}

func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := env[name]; return value, ok }
}

func status(a *App, path string) int {
	r := httptest.NewRecorder()
	a.Handler().ServeHTTP(r, httptest.NewRequest("GET", path, nil))
	return r.Code
}

func TestMessagesRouteIsOptInAndAuthenticated(t *testing.T) {
	cfg, env := fixture(t)
	app, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if got := status(app, "/v1/messages"); got != http.StatusNotFound {
		t.Fatalf("disabled Messages status=%d", got)
	}
	_ = app.Close()
	cfg.ClaudeMessages.Enabled = true
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "anthropic", BaseURL: "https://api.anthropic.com", Auth: string(config.ProviderAuthClaudeOAuthPassthrough)})
	app, err = newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"mindctl-auto","max_tokens":4,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer claude-native")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"type":"error"`) {
		t.Fatalf("missing gateway auth: %d %s", response.Code, response.Body.String())
	}
	request.Header.Set("X-Mindctl-Token", env["TEST_GATEWAY_TOKEN"])
	response = httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"type":"error"`) {
		t.Fatalf("invalid request: %d %s", response.Code, response.Body.String())
	}
	for _, test := range []struct {
		name         string
		method       string
		gatewayToken string
		duplicate    bool
		want         int
	}{
		{name: "invalid gateway token", method: http.MethodPost, gatewayToken: "wrong", want: http.StatusUnauthorized},
		{name: "ambiguous gateway token", method: http.MethodPost, gatewayToken: env["TEST_GATEWAY_TOKEN"], duplicate: true, want: http.StatusUnauthorized},
		{name: "GET not allowed after auth", method: http.MethodGet, gatewayToken: env["TEST_GATEWAY_TOKEN"], want: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/v1/messages", nil)
			request.Header.Set("Authorization", "Bearer claude-native")
			request.Header.Add("X-Mindctl-Token", test.gatewayToken)
			if test.duplicate {
				request.Header.Add("X-Mindctl-Token", test.gatewayToken)
			}
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != test.want || !strings.Contains(response.Body.String(), `"type":"error"`) || strings.Contains(response.Body.String(), "claude-native") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestMessagesHandlerCrossProviderCredentialsWithRealAdapters(t *testing.T) {
	var openAICalls, anthropicCalls atomic.Int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		openAICalls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer private-provider-token" || request.Header.Get("anthropic-beta") != "" || request.Header.Get("X-Mindctl-Token") != "" || strings.Contains(string(body), "native-secret") {
			t.Error("Claude credential or header crossed to separately credentialed provider")
		}
		_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"first","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"openai-ok"}]}],"usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer openAIServer.Close()
	anthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		anthropicCalls.Add(1)
		if request.URL.Path != "/v1/messages" || request.Header.Get("Authorization") != "Bearer native-secret" || request.Header.Get("anthropic-beta") != "oauth-2025-04-20,feature-x" || request.Header.Get("X-Mindctl-Token") != "" {
			t.Error("Anthropic did not receive native request-scoped credential")
		}
		_, _ = io.WriteString(w, `{"id":"upstream","role":"assistant","content":[{"type":"text","text":"anthropic-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer anthropicServer.Close()
	cfg, env := fixture(t)
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.ClaudeMessages.Enabled = true
	cfg.Providers[0].BaseURL = openAIServer.URL
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "anthropic", BaseURL: anthropicServer.URL, Auth: string(config.ProviderAuthClaudeOAuthPassthrough)})
	cfg.Models[0].MaxOutputTokens = 256
	claude := cfg.Models[0]
	claude.ID, claude.Provider = "claude", "anthropic"
	cfg.Models = append(cfg.Models, claude)
	app, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, test := range []struct {
		name, model, want string
		status            int
	}{
		{name: "separate API key", model: "first", want: "openai-ok", status: http.StatusOK},
		{name: "subscription", model: "claude", want: "anthropic-ok", status: http.StatusOK},
		{name: "unknown concrete model", model: "unknown", want: "unknown", status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"model":"` + test.model + `","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
			request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer native-secret")
			request.Header.Set("anthropic-beta", "oauth-2025-04-20,feature-x")
			request.Header.Set("X-Mindctl-Token", env["TEST_GATEWAY_TOKEN"])
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if openAICalls.Load() != 1 || anthropicCalls.Load() != 1 {
		t.Fatalf("provider calls openai=%d anthropic=%d", openAICalls.Load(), anthropicCalls.Load())
	}
}

func TestMessagesHandlerStreamsToolFragmentsWithoutLeakingClaudeHeaders(t *testing.T) {
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer private-provider-token" || request.Header.Get("anthropic-beta") != "" {
			t.Error("Claude headers crossed into OpenAI stream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\n"+
			"event: response.function_call_arguments.delta\ndata: {\"item_id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"delta\":\"{\\\"q\\\":\"}\n\n"+
			"event: response.function_call_arguments.delta\ndata: {\"item_id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"delta\":\"\\\"x\\\"}\"}\n\n"+
			"event: response.completed\ndata: {\"response\":{\"id\":\"upstream\",\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":3}}}\n\n")
	}))
	defer openAIServer.Close()
	cfg, env := fixture(t)
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.ClaudeMessages.Enabled = true
	cfg.Providers[0].BaseURL = openAIServer.URL
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "anthropic", BaseURL: "https://api.anthropic.com", Auth: string(config.ProviderAuthClaudeOAuthPassthrough)})
	cfg.Models[0].MaxOutputTokens = 256
	app, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"first","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`))
	request.Header.Set("X-Mindctl-Token", env["TEST_GATEWAY_TOKEN"])
	request.Header.Set("Authorization", "Bearer native-secret")
	request.Header.Set("anthropic-beta", "feature-x")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"call_1"`) || !strings.Contains(response.Body.String(), `"partial_json":"{\"q\":"`) || !strings.Contains(response.Body.String(), `"partial_json":"\"x\"}"`) || !strings.Contains(response.Body.String(), "event: message_stop") || strings.Contains(response.Body.String(), "native-secret") {
		t.Fatalf("status=%d stream=%s", response.Code, response.Body.String())
	}
}

func TestNewInitializesStorageKeyringAndStableHandler(t *testing.T) {
	cfg, env := fixture(t)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Handler() != a.Handler() || status(a, "/healthz") != 200 || status(a, "/readyz") != 200 {
		t.Fatal("operational handler is not stable and ready")
	}
	if !a.catalog[0].Capabilities.HostedTools["web_search"] {
		t.Fatal("web_search capability was not mapped")
	}
	var migrations int
	if err := a.store.SQL().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil || migrations != 3 {
		t.Fatalf("migrations=%d err=%v", migrations, err)
	}
	old, err := contentcrypto.New("old", map[string][]byte{"old": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := old.Encrypt([]byte("rotation proof"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := a.keyring.Decrypt(envelope)
	if err != nil || string(plaintext) != "rotation proof" {
		t.Fatalf("old key decrypt: %v", err)
	}
	envelope, err = a.keyring.Encrypt([]byte("active proof"))
	if err != nil || envelope.KeyID != "active" {
		t.Fatalf("active encryption: %v", err)
	}
}

func TestNewCreatesDebugTraceInUserCacheWhenEnabled(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	cfg, env := fixture(t)
	cfg.Debug = true

	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if a.debugTrace == nil {
		t.Fatal("debug trace was not initialized")
	}
	path := a.debugTrace.Path()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(cacheHome, "mindctl", "logs") {
		t.Fatalf("trace path = %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("trace file: %v", err)
	}
}

func TestNewUsesTheDefaultResponseBodyLimit(t *testing.T) {
	cfg, env := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"id":"upstream","status":"completed","model":"first","output":[]}`))
	}))
	t.Cleanup(server.Close)
	cfg.Classifier.Endpoint = server.URL
	cfg.Providers[0].BaseURL = server.URL
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"mindctl-auto","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+env["TEST_GATEWAY_TOKEN"])
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestConfiguredProvidersUsesDeepSeekResponsesEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer deepseek-token" {
			t.Fatalf("method=%s path=%s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"deepseek-flash","output":[]}`)
	}))
	defer server.Close()

	providers, err := configuredProviders([]config.ProviderConfig{{
		ID: "deepseek", BaseURL: server.URL, APIKeyEnv: "DEEPSEEK_API_TOKEN",
	}}, func(name string) (string, bool) {
		return "deepseek-token", name == "DEEPSEEK_API_TOKEN"
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = providers["deepseek"].Execute(context.Background(), domain.Model{
		ID: "deepseek-flash", UpstreamID: "deepseek-flash", Capabilities: domain.Capabilities{Text: true},
	}, inference.Request{Model: "deepseek-flash", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredProvidersUsesMetaResponsesEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer meta-token" {
			t.Fatalf("method=%s path=%s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"muse-spark-1.3","output":[]}`)
	}))
	defer server.Close()

	providers, err := configuredProviders([]config.ProviderConfig{{
		ID: "meta", BaseURL: server.URL, APIKeyEnv: "MUSE_API_KEY",
	}}, func(name string) (string, bool) {
		return "meta-token", name == "MUSE_API_KEY"
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = providers["meta"].Execute(context.Background(), domain.Model{
		ID: "muse-spark-1.3", UpstreamID: "muse-spark-1.3", Capabilities: domain.Capabilities{Text: true},
	}, inference.Request{Model: "muse-spark-1.3", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnavailableLayaPreservesT4Fallback(t *testing.T) {
	cfg, env := fixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"first","output":[]}`)
	}))
	defer provider.Close()
	cfg.Classifier = config.ClassifierConfig{Endpoint: "http://127.0.0.1:1", TokenEnv: "TEST_LAYA_TOKEN"}
	cfg.Providers[0].BaseURL = provider.URL
	env["TEST_LAYA_TOKEN"] = "private-laya-token"
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	output, err := a.executor.Execute(context.Background(), executor.Input{
		ClientID: "classifier-fallback", Request: inference.Request{Model: "mindctl-auto", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}},
		Models: a.catalog, Features: domain.RequestFeatures{NeedsText: true}, MinTier: &a.minTier, MaxTier: &a.maxTier, SafeFallbackTier: a.safeFallbackTier,
		ProviderCredentials: a.providerCredentials, ProviderAvailability: a.providerAvailability,
	})
	if err != nil || output.Decision.Tier < domain.T4 {
		t.Fatalf("decision=%+v err=%v", output.Decision, err)
	}
}

func TestChatGPTOAuthRequestSucceedsWithoutOpenAIAPIKey(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/classify":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/responses":
			call := providerCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer oauth.jwt" || r.Header.Get("ChatGPT-Account-Id") != "account-1" {
				t.Fatalf("headers=%v", r.Header)
			}
			if got := r.Header.Get("x-openai-internal-codex-responses-lite"); (call == 3 && got != "true") || (call != 3 && got != "") {
				t.Fatalf("call=%d responses-lite header=%q", call, got)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			reasoning, _ := body["reasoning"].(map[string]any)
			include, _ := body["include"].([]any)
			streamOptions, _ := body["stream_options"].(map[string]any)
			metadata, _ := body["client_metadata"].(map[string]any)
			textControls, _ := body["text"].(map[string]any)
			tools, _ := body["tools"].([]any)
			input, _ := body["input"].([]any)
			if body["model"] != "first" || body["tool_choice"] != "auto" || body["parallel_tool_calls"] != true ||
				body["store"] != false || body["stream"] != true || reasoning["effort"] != "high" || reasoning["summary"] != "auto" ||
				len(include) != 1 || include[0] != "reasoning.encrypted_content" || body["prompt_cache_key"] != "cache-key" ||
				body["service_tier"] != "priority" || streamOptions["reasoning_summary_delivery"] != "sequential_cutoff" ||
				metadata["thread_id"] != "thread-1" || textControls["verbosity"] != "high" {
				t.Fatalf("body=%v", body)
			}
			if call <= 2 {
				if len(tools) != 2 {
					t.Fatalf("body=%v", body)
				}
				customTool, _ := tools[1].(map[string]any)
				customFormat, _ := customTool["format"].(map[string]any)
				if customTool["type"] != "custom" || customTool["name"] != "apply_patch" || customFormat["type"] != "grammar" {
					t.Fatalf("body=%v", body)
				}
			}
			if call == 1 && len(input) != 2 {
				t.Fatalf("initial input=%v", input)
			}
			if call == 2 {
				if len(input) != 4 {
					t.Fatalf("continuation input=%v", input)
				}
				customCall, _ := input[2].(map[string]any)
				customOutput, _ := input[3].(map[string]any)
				if customCall["type"] != "custom_tool_call" || customCall["input"] != "*** Begin Patch" ||
					customOutput["type"] != "custom_tool_call_output" || customOutput["output"] != "Done!" {
					t.Fatalf("continuation input=%v", input)
				}
				if _, present := customOutput["input"]; present {
					t.Fatalf("Codex custom-tool continuation sent input field: %v", customOutput)
				}
			}
			if call == 3 {
				if len(tools) != 0 || len(input) != 3 {
					t.Fatalf("responses-lite body=%v", body)
				}
				additional, _ := input[0].(map[string]any)
				additionalTools, _ := additional["tools"].([]any)
				if len(additionalTools) != 1 {
					t.Fatalf("responses-lite input=%v", input)
				}
				namespace, _ := additionalTools[0].(map[string]any)
				namespaceTools, _ := namespace["tools"].([]any)
				developer, _ := input[1].(map[string]any)
				if additional["id"] != "at_tools" || additional["type"] != "additional_tools" || additional["role"] != "developer" ||
					namespace["type"] != "namespace" || namespace["name"] != "functions" ||
					len(namespaceTools) != 2 || developer["id"] != "msg_dev" || developer["role"] != "developer" {
					t.Fatalf("responses-lite input=%v", input)
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if call == 1 {
				_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n"))
				_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_lookup\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n"))
				_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"ctc_1\",\"type\":\"custom_tool_call\",\"call_id\":\"call_patch\",\"name\":\"apply_patch\",\"input\":\"*** Begin Patch\"}}\n\n"))
			}
			_, _ = w.Write([]byte("event: response.completed\ndata: {\"response\":{\"id\":\"upstream\",\"status\":\"completed\",\"model\":\"first\",\"output\":[]}}\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cfg, env := fixture(t)
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.Classifier.Endpoint = server.URL
	cfg.Providers[0].BaseURL = server.URL
	cfg.Providers[0].Auth = string(config.ProviderAuthChatGPTOAuthPassthrough)
	cfg.Providers[0].APIKeyEnv = ""
	delete(env, "TEST_PROVIDER_KEY")
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	initialBody := `{
  "model":"mindctl-auto",
  "instructions":"be concise",
  "input":[
    {"type":"message","role":"developer","content":[{"type":"input_text","text":"follow repository instructions"}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
  ],
  "tools":[
    {"type":"function","name":"lookup","description":"find","parameters":{"type":"object"},"strict":true},
    {"type":"custom","name":"apply_patch","description":"apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: PATCH"}}
  ],
  "tool_choice":"auto",
  "parallel_tool_calls":true,
  "reasoning":{"effort":"high","summary":"auto"},
  "store":false,
  "stream":true,
  "stream_options":{"reasoning_summary_delivery":"sequential_cutoff"},
  "include":["reasoning.encrypted_content"],
  "service_tier":"priority",
  "prompt_cache_key":"cache-key",
  "text":{"verbosity":"high"},
  "client_metadata":{"thread_id":"thread-1"}
}`
	continuationBody := strings.Replace(initialBody,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
    {"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch"},
    {"type":"custom_tool_call_output","call_id":"call_patch","output":"Done!"}`,
		1,
	)
	liteBody := `{
  "model":"mindctl-auto",
  "input":[
    {"id":"at_tools","type":"additional_tools","role":"developer","tools":[
      {"type":"namespace","name":"functions","description":"","tools":[
        {"type":"function","name":"lookup","description":"find","parameters":{"type":"object"},"strict":true},
        {"type":"custom","name":"apply_patch","description":"apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: PATCH"}}
      ]}
    ]},
    {"id":"msg_dev","type":"message","role":"developer","content":[{"type":"input_text","text":"follow repository instructions"}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
  ],
  "tool_choice":"auto",
  "parallel_tool_calls":true,
  "reasoning":{"effort":"high","summary":"auto","context":"all_turns"},
  "store":false,
  "stream":true,
  "stream_options":{"reasoning_summary_delivery":"sequential_cutoff"},
  "include":["reasoning.encrypted_content"],
  "service_tier":"priority",
  "prompt_cache_key":"cache-key",
  "text":{"verbosity":"high"},
  "client_metadata":{"thread_id":"thread-1"}
}`
	request := func(accountID, body string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("X-Mindctl-Token", env["TEST_GATEWAY_TOKEN"])
		req.Header.Set("Authorization", "Bearer oauth.jwt")
		if accountID != "" {
			req.Header.Set("ChatGPT-Account-Id", accountID)
		}
		return req
	}

	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, request("account-1", initialBody))
	if rr.Code != http.StatusOK || providerCalls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, providerCalls.Load(), rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"custom_tool_call"`) || !strings.Contains(rr.Body.String(), `"input":"*** Begin Patch"`) {
		t.Fatalf("custom tool event body=%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"message"`) || !strings.Contains(rr.Body.String(), `"role":"assistant"`) ||
		!strings.Contains(rr.Body.String(), `"content":[{"text":"hello","type":"output_text"}]`) ||
		!strings.Contains(rr.Body.String(), `"type":"function_call"`) || !strings.Contains(rr.Body.String(), `"arguments":"{\"q\":\"x\"}"`) {
		t.Fatalf("native output item body=%s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, request("account-1", continuationBody))
	if rr.Code != http.StatusOK || providerCalls.Load() != 2 {
		t.Fatalf("continuation status=%d calls=%d body=%s", rr.Code, providerCalls.Load(), rr.Body.String())
	}

	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, request("account-1", liteBody))
	if rr.Code != http.StatusOK || providerCalls.Load() != 3 {
		t.Fatalf("responses-lite status=%d calls=%d body=%s", rr.Code, providerCalls.Load(), rr.Body.String())
	}

	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, request("", initialBody))
	if rr.Code == http.StatusOK || providerCalls.Load() != 3 {
		t.Fatalf("missing account status=%d calls=%d body=%s", rr.Code, providerCalls.Load(), rr.Body.String())
	}
}

func TestClaudeOAuthRequestSucceedsWithoutAnthropicAPIKey(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/classify":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/v1/messages":
			providerCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer claude.oauth" || r.Header.Get("x-api-key") != "" ||
				r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
				t.Fatalf("headers=%v", r.Header)
			}
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cfg, env := fixture(t)
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.Classifier.Endpoint = server.URL
	cfg.Providers[0] = config.ProviderConfig{
		ID:      "anthropic",
		BaseURL: server.URL,
		Auth:    string(config.ProviderAuthClaudeOAuthPassthrough),
	}
	cfg.Models[0].Provider = "anthropic"
	delete(env, "TEST_PROVIDER_KEY")
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	request := func(token string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"mindctl-auto","input":"hello"}`))
		req.Header.Set("X-Mindctl-Token", env["TEST_GATEWAY_TOKEN"])
		if token != "" {
			req.Header.Set("X-Mindctl-Claude-Token", token)
		}
		return req
	}

	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, request("claude.oauth"))
	if rr.Code != http.StatusOK || providerCalls.Load() != 1 || !strings.Contains(rr.Body.String(), `"text":"hello"`) {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, providerCalls.Load(), rr.Body.String())
	}

	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, request(""))
	if rr.Code == http.StatusOK || providerCalls.Load() != 1 {
		t.Fatalf("missing credential status=%d calls=%d body=%s", rr.Code, providerCalls.Load(), rr.Body.String())
	}
}

func TestChatGPTStreamReplaysEncryptedReasoningBeforeFunctionResult(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/classify":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/responses":
			call := providerCalls.Add(1)
			var body struct {
				Input []json.RawMessage `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if call == 1 {
				if len(body.Input) != 1 {
					t.Fatalf("initial input count=%d", len(body.Input))
				}
			} else if call == 2 {
				if len(body.Input) != 6 {
					t.Fatalf("continuation input count=%d", len(body.Input))
				}
				var reasoning, functionCall, functionOutput, customOutput, computerOutput map[string]any
				if err := json.Unmarshal(body.Input[1], &reasoning); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body.Input[2], &functionCall); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body.Input[3], &functionOutput); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body.Input[4], &customOutput); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body.Input[5], &computerOutput); err != nil {
					t.Fatal(err)
				}
				if reasoning["id"] != "rs_1" || reasoning["type"] != "reasoning" || reasoning["encrypted_content"] != "opaque-openai-state" ||
					!reflect.DeepEqual(reasoning["summary"], []any{map[string]any{"type": "summary_text", "text": "safe summary"}}) ||
					functionCall["id"] != "fc_1" || functionCall["type"] != "function_call" || functionCall["call_id"] != "call_lookup" ||
					functionOutput["type"] != "function_call_output" || functionOutput["call_id"] != "call_lookup" || functionOutput["output"] != `{"found":true}` ||
					customOutput["type"] != "custom_tool_call_output" || customOutput["call_id"] != "call_custom" || customOutput["output"] != `{"done":true}` ||
					computerOutput["type"] != "computer_call_output" || computerOutput["call_id"] != "call_computer" || computerOutput["output"] != `{"done":true}` {
					t.Fatal("continuation ordering or opaque state was lost")
				}
				for _, item := range []map[string]any{functionOutput, customOutput, computerOutput} {
					if _, present := item["input"]; present {
						t.Fatal("unexpected input field")
					}
				}
			} else {
				t.Fatalf("unexpected provider call %d", call)
			}

			w.Header().Set("Content-Type", "text/event-stream")
			if call == 1 {
				_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"safe summary\"}],\"encrypted_content\":\"opaque-openai-state\"}}\n\n")
				_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_lookup\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"mindctl\\\"}\"}}\n\n")
			}
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"id\":\"upstream\",\"status\":\"completed\",\"model\":\"first\",\"output\":[]}}\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cfg, env := fixture(t)
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.Classifier.Endpoint = server.URL
	cfg.Providers[0].BaseURL = server.URL
	cfg.Providers[0].Auth = string(config.ProviderAuthChatGPTOAuthPassthrough)
	cfg.Providers[0].APIKeyEnv = ""
	delete(env, "TEST_PROVIDER_KEY")
	app, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })

	request := func(body string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("X-Mindctl-Token", env["TEST_GATEWAY_TOKEN"])
		req.Header.Set("Authorization", "Bearer oauth.jwt")
		req.Header.Set("ChatGPT-Account-Id", "account-1")
		return req
	}
	initial := httptest.NewRecorder()
	app.Handler().ServeHTTP(initial, request(`{"model":"mindctl-auto","stream":true,"input":"find Mindctl"}`))
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), `"type":"reasoning"`) || !strings.Contains(initial.Body.String(), `"summary":[{"type":"summary_text","text":"safe summary"}]`) || !strings.Contains(initial.Body.String(), `"encrypted_content":"opaque-openai-state"`) {
		t.Fatalf("initial response did not preserve reasoning: status=%d", initial.Code)
	}
	const responseIDKey = `"response_id":"`
	responseIDStart := strings.Index(initial.Body.String(), responseIDKey)
	if responseIDStart < 0 {
		t.Fatal("initial response has no gateway response ID")
	}
	responseID := strings.SplitN(initial.Body.String()[responseIDStart+len(responseIDKey):], `"`, 2)[0]

	continuation := httptest.NewRecorder()
	app.Handler().ServeHTTP(continuation, request(`{"model":"mindctl-auto","stream":true,"previous_response_id":"`+responseID+`","input":[{"type":"function_call_output","call_id":"call_lookup","output":"{\"found\":true}"},{"type":"custom_tool_call_output","call_id":"call_custom","output":"{\"done\":true}"},{"type":"computer_call_output","call_id":"call_computer","output":"{\"done\":true}"}]}`))
	if continuation.Code != http.StatusOK || providerCalls.Load() != 2 {
		t.Fatalf("continuation status=%d calls=%d", continuation.Code, providerCalls.Load())
	}
}

func TestSQLiteFailureChangesReadinessButNotHealth(t *testing.T) {
	cfg, env := fixture(t)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if _, err := a.store.SQL().Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatal(err)
	}
	if status(a, "/readyz") != 503 || status(a, "/healthz") != 200 {
		t.Fatal("SQLite failure did not affect readiness independently")
	}
}

func TestCloseIsConcurrentIdempotentAndReleasesSQLite(t *testing.T) {
	cfg, env := fixture(t)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := a.store.SQL().Ping(); err == nil {
		t.Fatal("database remains open")
	}
	if status(a, "/readyz") != 503 || status(a, "/healthz") != 200 {
		t.Fatal("closed app operational state is incorrect")
	}
}

func TestFiniteRetentionMaintenanceDeletesOnlyEncryptedContent(t *testing.T) {
	cfg, env := fixture(t)
	cfg.SQLite.Retention = time.Hour
	cfg.SQLite.RetentionMaintenanceInterval = time.Minute
	clock := newManualRetentionClock(time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC))
	completed := make(chan error, 1)
	var a *App
	var err error
	a, err = newWithLookupWithMaintenance(context.Background(), cfg, lookup(env), retentionMaintenanceOptions{
		Clock:            clock,
		OperationTimeout: time.Second,
		DeleteExpiredContent: func(ctx context.Context, retention time.Duration, now time.Time) (int64, error) {
			if _, ok := ctx.Deadline(); !ok {
				return 0, errors.New("retention operation has no deadline")
			}
			return a.store.DeleteExpiredContent(ctx, retention, now)
		},
		OnCycleComplete: func(err error) { completed <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	record := storage.RequestRecord{
		ID:         "expired-content",
		CreatedAt:  clock.Now().Add(-time.Hour - time.Nanosecond),
		Prompt:     []byte("expired prompt"),
		Answer:     []byte("expired answer"),
		RawContent: []byte("expired raw content"),
		RejectedOutputs: [][]byte{
			[]byte("expired rejection"),
		},
		Decision: router.Decision{Tier: domain.T4, ModelID: "first", Provider: "provider", Reasons: []string{"retained telemetry"}},
	}
	if err := a.store.WithTx(context.Background(), func(tx storage.Tx) error { return tx.InsertRequest(record) }); err != nil {
		t.Fatal(err)
	}
	clock.Tick()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	got, err := a.store.GetRequest(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != nil || got.Answer != nil || got.RawContent != nil || len(got.RejectedOutputs) != 0 || !reflect.DeepEqual(got.Decision, record.Decision) {
		t.Fatalf("retention removed telemetry or left encrypted content: %+v", got)
	}
}

func TestZeroRetentionDoesNotStartMaintenance(t *testing.T) {
	cfg, env := fixture(t)
	clock := newManualRetentionClock(time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC))
	a, err := newWithLookupWithMaintenance(context.Background(), cfg, lookup(env), retentionMaintenanceOptions{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.maintenance != nil || clock.newTickerCalls != 0 {
		t.Fatalf("unlimited retention started maintenance: maintenance=%v ticks=%d", a.maintenance, clock.newTickerCalls)
	}
}

func TestRetentionMaintenanceFailureChangesReadiness(t *testing.T) {
	cfg, env := fixture(t)
	cfg.SQLite.Retention = time.Hour
	clock := newManualRetentionClock(time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC))
	completed := make(chan error, 1)
	a, err := newWithLookupWithMaintenance(context.Background(), cfg, lookup(env), retentionMaintenanceOptions{
		Clock: clock,
		DeleteExpiredContent: func(context.Context, time.Duration, time.Time) (int64, error) {
			return 0, errors.New("retention delete failed")
		},
		OnCycleComplete: func(err error) { completed <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	clock.Tick()
	if err := <-completed; err == nil {
		t.Fatal("maintenance failure was lost")
	}
	if status(a, "/healthz") != http.StatusOK || status(a, "/readyz") != http.StatusServiceUnavailable {
		t.Fatal("maintenance failure did not change readiness independently of health")
	}
}

func TestCloseWaitsForRetentionMaintenanceBeforeClosingSQLite(t *testing.T) {
	cfg, env := fixture(t)
	cfg.SQLite.Retention = time.Hour
	clock := newManualRetentionClock(time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC))
	started := make(chan struct{})
	pingWhileStopping := make(chan error, 1)
	var a *App
	var err error
	a, err = newWithLookupWithMaintenance(context.Background(), cfg, lookup(env), retentionMaintenanceOptions{
		Clock: clock,
		DeleteExpiredContent: func(ctx context.Context, _ time.Duration, _ time.Time) (int64, error) {
			close(started)
			<-ctx.Done()
			pingWhileStopping <- a.store.SQL().Ping()
			return 0, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	clock.Tick()
	<-started
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-pingWhileStopping; err != nil {
		t.Fatalf("SQLite closed before retention maintenance stopped: %v", err)
	}
	if err := a.store.SQL().Ping(); err == nil {
		t.Fatal("SQLite remains open after shutdown")
	}
}

type manualRetentionTicker struct {
	ch      chan time.Time
	stop    sync.Once
	stopped chan struct{}
}

func (t *manualRetentionTicker) C() <-chan time.Time { return t.ch }
func (t *manualRetentionTicker) Stop()               { t.stop.Do(func() { close(t.stopped) }) }

type manualRetentionClock struct {
	now            time.Time
	ticker         *manualRetentionTicker
	newTickerCalls int
}

func newManualRetentionClock(now time.Time) *manualRetentionClock {
	return &manualRetentionClock{now: now, ticker: &manualRetentionTicker{ch: make(chan time.Time, 1), stopped: make(chan struct{})}}
}

func (c *manualRetentionClock) Now() time.Time { return c.now }

func (c *manualRetentionClock) NewTicker(time.Duration) retentionTicker {
	c.newTickerCalls++
	return c.ticker
}

func (c *manualRetentionClock) Tick() { c.ticker.ch <- c.now }

func TestNewFailsWhenEncryptionKeyIsMissing(t *testing.T) {
	cfg, env := fixture(t)
	for name, value := range env {
		t.Setenv(name, value)
	}
	t.Setenv("TEST_ENCRYPTION_KEY", "")
	a, err := New(context.Background(), cfg)
	if a != nil || err == nil || !strings.Contains(err.Error(), "encryption") || !strings.Contains(err.Error(), "TEST_ENCRYPTION_KEY") {
		t.Fatalf("app=%v err=%v", a, err)
	}
}

func TestNewRejectsInvalidRuntimeConfigWithoutSecretValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config, map[string]string)
	}{
		{"missing client token", func(_ *config.Config, env map[string]string) { delete(env, "TEST_GATEWAY_TOKEN") }},
		{"missing classifier token", func(_ *config.Config, env map[string]string) { delete(env, "TEST_LAYA_TOKEN") }},
		{"missing provider token", func(_ *config.Config, env map[string]string) { delete(env, "TEST_PROVIDER_KEY") }},
		{"malformed active key", func(_ *config.Config, env map[string]string) {
			env["TEST_ENCRYPTION_KEY"] = "not-base64-private-secret"
		}},
		{"malformed old key", func(_ *config.Config, env map[string]string) { env["TEST_OLD_KEY"] = "not-base64-private-secret" }},
		{"unknown active key", func(cfg *config.Config, _ map[string]string) { cfg.Encryption.ActiveKeyID = "absent" }},
		{"bad bounds", func(cfg *config.Config, _ map[string]string) { cfg.Routing.MinTier = "T9" }},
		{"no models", func(cfg *config.Config, _ map[string]string) { cfg.Models = nil }},
		{"all models disabled", func(cfg *config.Config, _ map[string]string) { cfg.Models[0].Available = false }},
		{"unusable provider", func(cfg *config.Config, _ map[string]string) { cfg.Providers[0].BaseURL = "not-a-url" }},
		{"invalid classifier endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Classifier.Endpoint = "not-a-url" }},
		{"query classifier endpoint", func(cfg *config.Config, _ map[string]string) {
			cfg.Classifier.Endpoint = "https://laya.example.com?x=1"
		}},
		{"empty query classifier endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Classifier.Endpoint = "https://laya.example.com?" }},
		{"fragment classifier endpoint", func(cfg *config.Config, _ map[string]string) {
			cfg.Classifier.Endpoint = "https://laya.example.com#section"
		}},
		{"empty fragment classifier endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Classifier.Endpoint = "https://laya.example.com#" }},
		{"invalid classifier retry count", func(cfg *config.Config, _ map[string]string) { cfg.Classifier.MaxRetries = -1 }},
		{"SQLite cannot open", func(cfg *config.Config, _ map[string]string) {
			cfg.SQLite.Path = filepath.Join(t.TempDir(), "missing", "db")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, env := fixture(t)
			tc.mutate(&cfg, env)
			a, err := newWithLookup(context.Background(), cfg, lookup(env))
			if a != nil {
				_ = a.Close()
				t.Fatal("returned app on failure")
			}
			if err == nil {
				t.Fatal("invalid bootstrap succeeded")
			}
			for _, value := range env {
				if strings.Contains(err.Error(), value) {
					t.Fatal("error exposed a resolved secret")
				}
			}
		})
	}
}

func TestNewSnapshotsEachResolvedSecretOnce(t *testing.T) {
	cfg, env := fixture(t)
	reads := map[string]int{}
	a, err := newWithLookup(context.Background(), cfg, func(name string) (string, bool) {
		reads[name]++
		if reads[name] > 1 {
			return "changed-private-secret", true
		}
		value, ok := env[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if cfg.Classifier.TokenEnv != "TEST_LAYA_TOKEN" || cfg.Encryption.Keys["active"] != "TEST_ENCRYPTION_KEY" {
		t.Fatal("config mutated to resolved secrets")
	}
}

func TestBootstrapCatalogFeedsDeterministicPolicy(t *testing.T) {
	cfg, env := fixture(t)
	second := cfg.Models[0]
	second.ID = "second"
	cfg.Models = append(cfg.Models, second)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	cfg.Models[0].TaskSuccessPriors["coding"] = 0
	cfg.Models[0].Capabilities[0] = "mutated"
	decision, err := a.policy.Decide(router.DecisionInput{
		Models: a.catalog, Floor: domain.T4, TaskType: domain.TaskCoding,
		Features: domain.RequestFeatures{NeedsText: true, NeedsImages: true, NeedsFunctions: true, NeedsJSONSchema: true, InputTokens: 1000, ContextTokens: 32000},
		MinTier:  &a.minTier, MaxTier: &a.maxTier, ProviderCredentials: a.providerCredentials, ProviderAvailability: a.providerAvailability,
	})
	if err != nil || decision.ModelID != "first" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if len(decision.Candidates) != 2 || decision.Candidates[0].SuccessProbability != .95 || decision.Candidates[0].DirectCost != .002 {
		t.Fatalf("candidate wiring = %+v", decision.Candidates)
	}
}

func TestClassifierUsesResolvedSecretAndCloseReleasesItsConnections(t *testing.T) {
	cfg, env := fixture(t)
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-laya-token" || r.URL.Path != "/v1/classify" {
			t.Error("classifier was not wired to its endpoint and resolved credential")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	cfg.Classifier.Endpoint = server.URL
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	_, err = a.classifier.Classify(context.Background(), classifier.Input{Prompt: "test"})
	if !errors.Is(err, classifier.ErrUnavailable) {
		t.Fatalf("classification error = %v", err)
	}
	if status(a, "/readyz") != 200 {
		t.Fatal("remote classification failure changed local readiness")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("App.Close left its classifier connection open")
	}
}
