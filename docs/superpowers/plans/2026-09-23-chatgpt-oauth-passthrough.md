# ChatGPT OAuth Passthrough Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route eligible Codex Responses requests through the caller's ChatGPT OAuth subscription without requiring an OpenAI API key.

**Architecture:** Codex owns login and refresh, while Mindctl authenticates the client through a separate configurable header. Mindctl captures only the OAuth bearer and ChatGPT account ID in request context, uses them for request-local provider eligibility, and forwards them only from the OpenAI adapter to the ChatGPT Codex backend.

**Tech Stack:** Go 1.26, `net/http`, strict YAML via `gopkg.in/yaml.v3`, existing Responses/SSE adapters, `httptest`, devenv, Nx sync.

**Spec:** `docs/superpowers/specs/2026-09-23-chatgpt-oauth-passthrough-design.md`

## Global Constraints

- Codex remains the only owner of ChatGPT login, token refresh, revocation, and persistent OAuth storage.
- OAuth access tokens and ChatGPT account IDs must never enter inference structs, storage records, logs, telemetry, response bodies, or error text.
- A provider uses exactly one credential mode; OAuth mode never falls back to `OPENAI_API_KEY`.
- Existing API-key defaults, `/v1/responses`, T0-T6 routing, conversation pinning, and Anthropic/Gemini behavior remain unchanged.
- Redirects remain disabled for every request carrying a credential.
- Do not overwrite or stage unrelated dirty work. At plan time this includes
  `README.md`, `internal/conversation/service.go`, and `internal/router/cost.go`;
  re-snapshot status before execution because the shared checkout is active.
- Run project commands through `devenv shell -- ...` because this repository contains `devenv.nix`.
- Follow RED-GREEN-REFACTOR and observe each expected failure before changing production code.

## Review Focus

- Duplicate OAuth header values are ambiguous and must not produce a credential; Task 2 tests this.
- Bearer schemes are case-insensitive, while empty, whitespace-containing tokens and blank account IDs are rejected; Task 2 tests this.
- Requests with and without OAuth must not share credentials or eligibility, including under concurrency; Tasks 2 and 3 test this.
- A trailing slash on the ChatGPT base URL must still produce exactly `/responses`; Task 4 tests this.
- Redirects must not deliver the bearer or account ID to a second origin; Task 4 tests this.

---

## File structure

- `internal/config/types.go`: YAML fields, defaults, auth-mode constants, and cross-field validation.
- `internal/upstreamauth/chatgpt.go`: provider-neutral request-context credential.
- `internal/api/auth.go`: configurable gateway authentication and OAuth capture.
- `internal/api/responses.go`: request-local provider credential eligibility.
- `internal/provider/openai/client.go`: API-key and ChatGPT OAuth request construction.
- `internal/app/app.go`: configuration, middleware, and provider wiring.
- Corresponding `*_test.go` files: focused RED/GREEN regressions.
- `config.example.yaml` and `README.md`: operator and Codex setup.

### Task 1: Add strict provider authentication configuration

**Files:**

- Modify: `internal/config/types.go`
- Test: `internal/config/load_test.go`

**Interfaces:**

- Produces: `type ProviderAuthMode string`
- Produces: `ProviderAuthAPIKey` and `ProviderAuthChatGPTOAuthPassthrough`
- Produces: `func (ClientAuthConfig) HeaderName() string`
- Produces: `func (ProviderConfig) AuthMode() ProviderAuthMode`

- [ ] **Step 1: Write failing configuration tests**

Add tests preserving defaults and covering every cross-field rule:

```go
func TestProviderAuthenticationDefaultsRemainCompatible(t *testing.T) {
	cfg := validConfig()
	if cfg.ClientAuth.HeaderName() != "Authorization" || cfg.Providers[0].AuthMode() != ProviderAuthAPIKey {
		t.Fatalf("header=%q auth=%q", cfg.ClientAuth.HeaderName(), cfg.Providers[0].AuthMode())
	}
	if err := cfg.Validate(testEnv); err != nil { t.Fatal(err) }
}

func TestValidateAcceptsChatGPTOAuthWithoutAPIKey(t *testing.T) {
	cfg := validConfig()
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
	cfg.Providers[0].BaseURL = "https://chatgpt.com/backend-api/codex"
	cfg.Providers[0].APIKeyEnv = ""
	if err := cfg.Validate(testEnv); err != nil { t.Fatal(err) }
}
```

Add a table test for: unknown mode; OAuth with `api_key_env`; OAuth on Anthropic; OAuth with gateway header `Authorization`; OAuth with gateway header `ChatGPT-Account-Id`; and invalid header name `Bad Header`. Assert stable sanitized error substrings, not entire joined errors.

- [ ] **Step 2: Run the focused tests and verify RED**

```sh
devenv shell -- go test ./internal/config -run 'TestProviderAuthentication|TestValidateAcceptsChatGPT|TestValidateRejectsInvalidAuthentication' -count=1
```

Expected: compilation fails because `Header`, `Auth`, `ProviderAuthMode`, `HeaderName`, and `AuthMode` do not exist.

- [ ] **Step 3: Implement fields, defaults, and validation**

```go
const DefaultClientAuthHeader = "Authorization"

type ProviderAuthMode string

const (
	ProviderAuthAPIKey ProviderAuthMode = "api_key"
	ProviderAuthChatGPTOAuthPassthrough ProviderAuthMode = "chatgpt_oauth_passthrough"
)

type ClientAuthConfig struct {
	TokenEnv string `yaml:"token_env"`
	Header string `yaml:"header"`
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

func (c ClientAuthConfig) HeaderName() string {
	if c.Header == "" { return DefaultClientAuthHeader }
	return c.Header
}

type ProviderConfig struct {
	ID string `yaml:"id"`
	BaseURL string `yaml:"base_url"`
	Auth string `yaml:"auth"`
	APIKeyEnv string `yaml:"api_key_env"`
}

func (p ProviderConfig) AuthMode() ProviderAuthMode {
	if p.Auth == "" { return ProviderAuthAPIKey }
	return ProviderAuthMode(p.Auth)
}
```

Use `net/textproto.CanonicalMIMEHeaderKey` to validate field names. Require `api_key_env` only for API-key mode; forbid it for OAuth; support OAuth only for provider ID `openai`; reject case-insensitive gateway-header collisions with `Authorization` and `ChatGPT-Account-Id` when OAuth is configured.

- [ ] **Step 4: Run all configuration tests and verify GREEN**

```sh
devenv shell -- go test ./internal/config -count=1
```

Expected: PASS, including legacy defaults and shipped-example decoding.

- [ ] **Step 5: Commit**

```sh
git add internal/config/types.go internal/config/load_test.go
git commit -m "feat(config): add provider authentication modes"
```

### Task 2: Capture ChatGPT OAuth in request context

**Files:**

- Create: `internal/upstreamauth/chatgpt.go`
- Create: `internal/upstreamauth/chatgpt_test.go`
- Modify: `internal/api/auth.go`
- Test: `internal/api/responses_decode_test.go`

**Interfaces:**

- Produces: `type upstreamauth.ChatGPTCredential struct { AccessToken string; AccountID string }`
- Produces: `WithChatGPT(context.Context, ChatGPTCredential) context.Context`
- Produces: `ChatGPT(context.Context) (ChatGPTCredential, bool)`
- Changes: `Authenticate(http.Handler, string, TokenSource) http.Handler`
- Produces: `CaptureChatGPTOAuth(http.Handler) http.Handler`

- [ ] **Step 1: Write failing context tests**

```go
func TestChatGPTCredentialIsRequestScoped(t *testing.T) {
	base := context.Background()
	want := ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"}
	derived := WithChatGPT(base, want)
	if _, ok := ChatGPT(base); ok { t.Fatal("credential leaked into parent") }
	if got, ok := ChatGPT(derived); !ok || got != want { t.Fatalf("got=%+v ok=%v", got, ok) }
}

func TestChatGPTRejectsIncompleteCredential(t *testing.T) {
	for _, value := range []ChatGPTCredential{{AccessToken: "token"}, {AccountID: "account"}, {AccessToken: " ", AccountID: "account"}} {
		if _, ok := ChatGPT(WithChatGPT(context.Background(), value)); ok { t.Fatalf("accepted %+v", value) }
	}
}
```

- [ ] **Step 2: Write failing middleware tests**

Update existing Authenticate calls with `"Authorization"`. Add a dedicated-header test proving `X-Mindctl-Token` accepts the raw configured secret, because Codex `env_http_headers` does not add a Bearer prefix. Preserve the existing test proving the default Authorization header still requires `Bearer <token>`.

Add a table around `CaptureChatGPTOAuth` with valid lowercase `bearer`, missing token, token containing whitespace, blank account, duplicate Authorization, and duplicate account headers. The next handler calls `upstreamauth.ChatGPT(r.Context())`; only the complete single-valued pair is present.

- [ ] **Step 3: Run focused tests and verify RED**

```sh
devenv shell -- go test ./internal/upstreamauth ./internal/api -run 'TestChatGPT|TestAuthenticate|TestCapture' -count=1
```

Expected: compilation fails because the package and new middleware signatures do not exist.

- [ ] **Step 4: Implement immutable context storage**

```go
type ChatGPTCredential struct { AccessToken, AccountID string }
type chatGPTContextKey struct{}

func valid(c ChatGPTCredential) bool {
	return c.AccessToken != "" && strings.TrimSpace(c.AccessToken) == c.AccessToken &&
		!strings.ContainsAny(c.AccessToken, " \t\r\n") && strings.TrimSpace(c.AccountID) != ""
}

func WithChatGPT(ctx context.Context, value ChatGPTCredential) context.Context {
	if !valid(value) { return ctx }
	return context.WithValue(ctx, chatGPTContextKey{}, value)
}

func ChatGPT(ctx context.Context) (ChatGPTCredential, bool) {
	value, ok := ctx.Value(chatGPTContextKey{}).(ChatGPTCredential)
	return value, ok && valid(value)
}
```

- [ ] **Step 5: Implement the two middleware boundaries**

`Authenticate` rejects zero or multiple configured-header values. For `Authorization`, parse a case-insensitive Bearer scheme; for a dedicated header, compare the raw value. Continue hashing presented and configured values before constant-time comparison.

`CaptureChatGPTOAuth` requires exactly one value for each OAuth header, parses exactly two whitespace-delimited Authorization fields, uses `strings.EqualFold` for Bearer, and attaches only a complete credential. It never copies other headers.

- [ ] **Step 6: Run tests and race tests and verify GREEN**

```sh
devenv shell -- go test ./internal/upstreamauth ./internal/api -count=1
devenv shell -- go test -race ./internal/upstreamauth ./internal/api -count=1
```

Expected: both PASS with no race reports or secret-bearing diagnostics.

- [ ] **Step 7: Commit**

```sh
git add internal/upstreamauth/chatgpt.go internal/upstreamauth/chatgpt_test.go internal/api/auth.go internal/api/responses_decode_test.go internal/api/responses_test.go
git commit -m "feat(api): capture request-scoped ChatGPT OAuth"
```

### Task 3: Make OAuth provider eligibility request-local

**Files:**

- Modify: `internal/api/responses.go`
- Test: `internal/api/responses_test.go`

**Interfaces:**

- Produces: `ResponsesConfig.ChatGPTOAuthProviders map[string]bool`
- Changes: `(*responsesHandler).input(context.Context, string, inference.Request, Controls)`
- Consumes: `upstreamauth.ChatGPT(context.Context)` from Task 2.
- Preserves: `executor.Input.ProviderCredentials` contains booleans only, never secrets.

- [ ] **Step 1: Write the failing per-request eligibility test**

Extend `stubExecutor` with `input executor.Input` and record the input in Execute and Stream. Add a table with complete OAuth, missing account, and missing bearer requests:

```go
responses := NewResponsesHandler(runner, ResponsesConfig{
	MaxBodyBytes: 1 << 20,
	Models: []domain.Model{{ID: "gpt-test", Provider: "openai", Tier: domain.T4}},
	ProviderCredentials: map[string]bool{"openai": true, "anthropic": true},
	ChatGPTOAuthProviders: map[string]bool{"openai": true},
})
h := Authenticate(CaptureChatGPTOAuth(responses), "X-Mindctl-Token", staticTokens{{ID: "client", Value: "gateway"}})
```

For each request, assert `runner.input.ProviderCredentials["openai"]` is true only for the complete credential and that the static Anthropic entry remains true. Add parallel complete/incomplete requests to prove maps and contexts do not cross requests.

- [ ] **Step 2: Run the regression and verify RED**

```sh
devenv shell -- go test ./internal/api -run TestResponsesHandlerDerivesChatGPTCredentialAvailabilityPerRequest -count=1
```

Expected: compilation fails because `ChatGPTOAuthProviders` and the context-aware input method do not exist.

- [ ] **Step 3: Implement request-local derivation**

Clone the new map in `cloneResponsesConfig`, pass `r.Context()` to `input`, and use:

```go
func providerCredentials(ctx context.Context, cfg ResponsesConfig) map[string]bool {
	credentials := cloneBools(cfg.ProviderCredentials)
	_, hasChatGPT := upstreamauth.ChatGPT(ctx)
	for providerID, enabled := range cfg.ChatGPTOAuthProviders {
		if enabled { credentials[providerID] = hasChatGPT }
	}
	return credentials
}
```

Assign the returned map to `executor.Input.ProviderCredentials`. Do not add credential fields to executor or inference types.

- [ ] **Step 4: Run API tests and race tests and verify GREEN**

```sh
devenv shell -- go test ./internal/api -count=1
devenv shell -- go test -race ./internal/api -count=1
```

Expected: both PASS and the parallel test has no race.

- [ ] **Step 5: Commit**

```sh
git add internal/api/responses.go internal/api/responses_test.go
git commit -m "feat(router): derive OAuth eligibility per request"
```

### Task 4: Add ChatGPT authentication to the OpenAI adapter

**Files:**

- Modify: `internal/provider/openai/client.go`
- Test: `internal/provider/openai/client_test.go`

**Interfaces:**

- Preserves: `NewClient(baseURL, apiKey string, httpClient *http.Client) *Client` for API keys.
- Produces: `NewChatGPTOAuthClient(baseURL string, httpClient *http.Client) *Client`.
- Consumes: `upstreamauth.ChatGPT(context.Context)`.
- Preserves: `provider.Provider` for both modes.

- [ ] **Step 1: Write failing Execute and Stream OAuth tests**

Use a table over Execute and Stream. Build the context and client with:

```go
ctx := upstreamauth.WithChatGPT(context.Background(), upstreamauth.ChatGPTCredential{
	AccessToken: "oauth.jwt", AccountID: "account-1",
})
client := NewChatGPTOAuthClient(server.URL+"/", nil)
```

The test server must assert:

```go
if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer oauth.jwt" ||
	r.Header.Get("ChatGPT-Account-Id") != "account-1" || r.Header.Get("originator") != "mindctl" ||
	!strings.HasPrefix(r.Header.Get("User-Agent"), "mindctl") {
	t.Fatalf("path=%s headers=%v", r.URL.Path, r.Header)
}
```

Return a valid completed JSON response for Execute and a valid `response.completed` SSE frame for Stream.

- [ ] **Step 2: Write failing safety tests**

Add tests proving:

- missing context credentials return `provider.ErrorInvalidRequest` before network I/O;
- a `401` body containing `private-upstream-body` maps to `provider.ErrorAuthentication` without exposing body or token;
- both `401` and `403` map to the same sanitized authentication kind;
- a 302 redirect never calls the target server;
- a base URL ending in `/` produces exactly `/responses`;
- an unrelated context value never becomes an outbound header;
- API-key `NewClient` still sends `Bearer secret` to `/v1/responses` and never adds `ChatGPT-Account-Id`.

- [ ] **Step 3: Run focused tests and verify RED**

```sh
devenv shell -- go test ./internal/provider/openai -run 'TestOpenAIChatGPT|TestOpenAIRedirect' -count=1
```

Expected: compilation fails because `NewChatGPTOAuthClient` does not exist.

- [ ] **Step 4: Implement explicit adapter modes**

```go
type authMode uint8
const (
	authAPIKey authMode = iota
	authChatGPTOAuth
)

type Client struct {
	baseURL string
	apiKey string
	auth authMode
	http *http.Client
}

func NewChatGPTOAuthClient(baseURL string, httpClient *http.Client) *Client {
	return newClient(baseURL, "", authChatGPTOAuth, httpClient)
}
```

Keep `NewClient` delegating to `newClient` with `authAPIKey`. In `post`, API-key mode retains `<baseURL>/v1/responses`. OAuth mode requires `upstreamauth.ChatGPT(ctx)`, uses `<baseURL>/responses`, and sets only the bearer, account ID, `originator: mindctl`, Mindctl User-Agent, content type, and optional SSE accept header. Preserve `CheckRedirect = http.ErrUseLastResponse`.

- [ ] **Step 5: Run all adapter tests and verify GREEN**

```sh
devenv shell -- go test ./internal/provider/openai -count=1
```

Expected: PASS for API-key, OAuth, stream, non-stream, error, and redirect paths.

- [ ] **Step 6: Commit**

```sh
git add internal/provider/openai/client.go internal/provider/openai/client_test.go
git commit -m "feat(provider): support ChatGPT OAuth requests"
```

### Task 5: Wire OAuth through application bootstrap

**Files:**

- Modify: `internal/app/app.go`
- Test: `internal/app/app_test.go`
- Modify only if compilation requires: `cmd/mindctl/main_test.go`

**Interfaces:**

- Consumes: configuration helpers from Task 1, API middleware and metadata from Tasks 2-3, and the OAuth constructor from Task 4.
- Produces: `/v1/responses` starts and executes in OAuth mode without `OPENAI_API_KEY`.

- [ ] **Step 1: Write the failing application regression**

Create one `httptest.Server` for both dependencies. Return a valid Jev judgment for `/v1/systemone`. For `/responses`, assert the OAuth headers and return:

```json
{ "id": "upstream", "status": "completed", "model": "first", "output": [] }
```

Configure the fixture with `Header: "X-Mindctl-Token"`, OAuth provider mode, ChatGPT base URL, and empty `APIKeyEnv`; delete `TEST_PROVIDER_KEY`. Send a request with raw `X-Mindctl-Token`, bearer Authorization, and `ChatGPT-Account-Id`. Assert startup succeeds, response status is 200, and the OAuth upstream is called once. A second request missing account ID must not increase the upstream call count.

- [ ] **Step 2: Run the regression and verify RED**

```sh
devenv shell -- go test ./internal/app -run TestChatGPTOAuthRequestSucceedsWithoutOpenAIAPIKey -count=1
```

Expected: FAIL because bootstrap still requires and constructs API-key authentication.

- [ ] **Step 3: Implement bootstrap mode mapping**

```go
oauthProviders := make(map[string]bool)
for _, providerConfig := range cfg.Providers {
	switch providerConfig.AuthMode() {
	case config.ProviderAuthAPIKey:
		value, exists := getenv(providerConfig.APIKeyEnv)
		a.providerCredentials[providerConfig.ID] = exists && value != ""
	case config.ProviderAuthChatGPTOAuthPassthrough:
		a.providerCredentials[providerConfig.ID] = true
		oauthProviders[providerConfig.ID] = true
	}
	a.providerAvailability[providerConfig.ID] = validEndpoint(providerConfig.BaseURL)
}
```

In `configuredProviders`, call `openai.NewChatGPTOAuthClient` only for OAuth mode and never call `getenv("")`. Pass `oauthProviders` into `ResponsesConfig`.

Compose middleware so gateway authentication is outermost and rejects unauthorized calls before OAuth capture:

```go
responses = api.CaptureChatGPTOAuth(responses)
mux.Handle("/v1/responses", api.Authenticate(
	responses, cfg.ClientAuth.HeaderName(),
	appTokens{{ID: "configured-client", Value: clientToken}},
))
```

- [ ] **Step 4: Run app and command tests and verify GREEN**

```sh
devenv shell -- go test ./internal/app ./cmd/mindctl -count=1
```

Expected: PASS, including existing API-key startup tests.

- [ ] **Step 5: Run the full Go suite for caller compatibility**

```sh
devenv shell -- go test ./... -count=1
```

Expected: PASS. Change only callers broken by the intentional explicit-authentication signatures.

- [ ] **Step 6: Commit**

```sh
git add internal/app/app.go internal/app/app_test.go
git commit -m "feat(app): wire ChatGPT OAuth passthrough"
```

Include `cmd/mindctl/main_test.go` in the commit only if it changed.

### Task 6: Document the operator and Codex setup

**Files:**

- Modify: `config.example.yaml`
- Modify without staging unrelated hunks: `README.md`

**Interfaces:**

- Consumes: exact YAML and TOML fields implemented in Tasks 1 and 5.
- Produces: a copyable OAuth-first Mindctl example and Codex custom-provider configuration.

- [ ] **Step 1: Update the shipped configuration example**

Use the approved OAuth-first configuration:

```yaml
client_auth:
  token_env: MINDCTL_GATEWAY_TOKEN
  header: X-Mindctl-Token
  max_body_bytes: 16777216

providers:
  - id: openai
    base_url: https://chatgpt.com/backend-api/codex
    auth: chatgpt_oauth_passthrough
```

Add adjacent comments showing that API-key mode uses `auth: api_key`, `base_url: https://api.openai.com`, and `api_key_env: OPENAI_API_KEY`. State that the modes are exclusive.

- [ ] **Step 2: Verify strict example decoding**

```sh
devenv shell -- go test ./internal/config -run TestShippedExampleReachesSecretValidation -count=1
```

Expected: PASS; the example reaches runtime-secret validation without YAML/schema errors and does not demand `OPENAI_API_KEY`.

- [ ] **Step 3: Add the Codex setup to the current README**

Add a “ChatGPT subscription routing” section with:

```toml
model_provider = "mindctl"

[model_providers.mindctl]
name = "Mindctl"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
requires_openai_auth = true
env_http_headers = { "X-Mindctl-Token" = "MINDCTL_GATEWAY_TOKEN" }
```

Explain that the user signs into Codex with ChatGPT, exports only `MINDCTL_GATEWAY_TOKEN` for Mindctl client authentication, and does not set `OPENAI_API_KEY` for this provider. State that upstream model access and usage limits remain account-dependent.

Inspect `git diff -- README.md` before and after editing. Preserve every pre-existing hunk. Do not stage or commit README because it already contains unrelated user changes; report the new documentation hunk at handoff.

- [ ] **Step 4: Format and inspect documentation**

```sh
devenv shell -- pnpm prettier --check config.example.yaml docs/superpowers/specs/2026-09-23-chatgpt-oauth-passthrough-design.md docs/superpowers/plans/2026-09-23-chatgpt-oauth-passthrough.md
git diff --check -- config.example.yaml README.md
```

Expected: formatter and whitespace checks PASS, and original README edits remain intact.

- [ ] **Step 5: Commit only owned documentation**

```sh
git add config.example.yaml
git commit -m "docs(router): document ChatGPT OAuth configuration"
```

Leave `README.md` unstaged because the file has mixed ownership.

### Task 7: Full verification and security audit

**Files:**

- Inspect: every file changed by Tasks 1-6.
- Do not modify unrelated files generated by devenv or Nx.

**Interfaces:**

- Consumes: complete implementation and documentation.
- Produces: fresh correctness, race, static-analysis, workspace, and secret-flow evidence.

- [ ] **Step 1: Run the full Go suite**

```sh
devenv shell -- go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 2: Run the full race suite**

```sh
devenv shell -- go test -race ./... -count=1
```

Expected: PASS with no race reports.

- [ ] **Step 3: Run static analysis**

```sh
devenv shell -- go vet ./...
```

Expected: PASS.

- [ ] **Step 4: Verify Nx synchronization**

```sh
NX_SOCKET_DIR=/tmp/mindctl-nx devenv shell -- pnpm nx sync
```

Expected: “The workspace is already up to date.” Restore only generated churn unrelated to the feature; never restore the user's README edits.

- [ ] **Step 5: Audit credential flow and repository state**

```sh
rg -n "AccessToken|AccountID|ChatGPT-Account-Id|Authorization" internal
git diff --check
git status --short
git log --oneline --decorate -8
```

From the matches, confirm credentials exist only in request context and outbound header construction, not inference, executor, storage, telemetry, or errors. Compare final status with the execution-start snapshot and confirm every unrelated dirty file remains unstaged and semantically unchanged.

- [ ] **Step 6: Review against the approved spec**

Check every Goals, Non-goals, Compatibility, Error handling, and Testing item in the spec against code or fresh command evidence. Report any failing command by exact name and summary; do not claim completion until every required check passes.
