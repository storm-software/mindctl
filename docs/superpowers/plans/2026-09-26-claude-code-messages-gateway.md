# Claude Code Messages Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Claude Code use Mindctl's Anthropic Messages endpoint with its existing subscription login and cross-provider routing, then report Claude's configuration in `mindctl status`.

**Architecture:** Decode and encode the Anthropic wire format at the API edge, keeping the existing router and executor as the only path to providers. Authenticate the gateway separately from Claude's request-scoped OAuth; constrain provider-native features through eligibility rather than silently dropping them. Opt-in route registration and Claude configuration preserve existing Codex and Headroom behavior.

**Tech Stack:** Go 1.26, `net/http`, JSON/SSE, Cobra, existing inference/router/executor/provider packages.

**Spec:** `docs/superpowers/specs/2026-09-26-claude-code-messages-gateway-design.md`

## Global Constraints

- The Messages endpoint is opt-in via `claude_messages.enabled: true`; off preserves current routes and `Authorization` gateway authentication.
- Claude Code keeps its saved subscription credential: set `ANTHROPIC_BASE_URL` without `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, or `apiKeyHelper`; authenticate Mindctl with a separate dedicated header.
- Native Claude OAuth and `anthropic-beta` may reach only the configured Anthropic OAuth provider; never persist or log them, and never send them to other providers or redirects.
- `mindctl-auto` can choose any *eligible* provider with separate credentials; native-only features constrain candidates for that request; explicit model IDs are never silently remapped.
- Unknown mandatory fields and untranslatable required content fail explicitly. No automatic Home Manager edits, releases, commits, resets, or changes to existing unrelated dirty files.
- Run project Go commands with `devenv shell --` per repository instructions; separate unit/full-suite proof from an optional live subscription test.

## Review Focus

1. A missing, duplicate, malformed, or conflicting gateway/Claude auth header must fail without exposing either secret (Tasks 1, 2, 6).
2. A Claude-origin request selecting or falling back to a non-Anthropic provider must never forward native OAuth, beta capabilities, or signed thinking (Tasks 2, 3, 5, 6).
3. Claude Code may send `POST /v1/messages?beta=true`, unfamiliar beta headers, project-local settings, or built-in model IDs; these must respectively route, preserve to Anthropic, remain outside the global-only status claim, and reject unknown IDs clearly (Tasks 2, 3, 6, 7).
4. Split tool-input SSE fragments, stream cancellation, and post-emission errors must retain event order and not imply success or trigger a provider switch (Tasks 5, 6).
5. Missing/malformed Claude settings or unavailable gateway configuration must not misreport live connectivity or break `status codex` (Task 7).

---

### Task 1: Opt-In Configuration Contract

**Files:** Modify `internal/config/types.go`, `internal/config/load_test.go`, `config.example.yaml`.

**Interfaces:** Produce `config.ClaudeMessagesConfig{Enabled bool}` and `Config.ClaudeMessages` (`yaml:"claude_messages"`), consumed by Task 6. The enabled mode requires a non-`Authorization` client header and a configured Anthropic `claude_oauth_passthrough` provider. Disabled mode preserves today's valid configs.

- [ ] Write `TestClaudeMessagesRequiresSeparateClientHeaderAndOAuthProvider`: table cases disabled default, enabled with `X-Mindctl-Token` and Anthropic OAuth, enabled with `Authorization`, enabled without Anthropic OAuth; assert only the intended case validates. Verify strict YAML accepts `claude_messages.enabled` and rejects unknown keys.
- [ ] Run `devenv shell -- go test ./internal/config -run '^TestClaudeMessages' -count=1`; expect a compilation/assertion failure for the new config field.
- [ ] Add the minimal schema, validation, and commented opt-in example. Do not alter existing provider auth validation semantics.
- [ ] Rerun the focused tests; expect PASS.

### Task 2: Separate Native Claude OAuth From Gateway Auth

**Files:** Modify `internal/api/auth.go`, `internal/api/auth_test.go`, `internal/upstreamauth/claude.go`, `internal/upstreamauth/claude_test.go`, `internal/provider/anthropic/client.go`, `internal/provider/anthropic/client_test.go`.

**Interfaces:** Produce `api.CaptureNativeClaudeOAuth(next http.Handler) http.Handler` for the Messages route, distinct from existing `CaptureClaudeOAuth` for Responses. Produce `api.AuthenticateWithError(next http.Handler, header string, tokens TokenSource, writeError func(http.ResponseWriter, error)) http.Handler`, retaining `Authenticate` as its existing-error-format wrapper. Extend `upstreamauth.ClaudeCredential` with `Beta` and `Version` while keeping `AccessToken` and existing callers valid; the Anthropic OAuth client forwards validated, request-scoped header values, falling back to existing defaults only for old callers.

- [ ] Write `TestCaptureNativeClaudeOAuthIsRequestScoped` for one valid bearer and complete `anthropic-beta`, missing/duplicate/malformed headers, and a subsequent request without a credential; require fail-closed errors for malformed inputs. Write `TestAuthenticateWithErrorKeepsExistingResponsesShape` for missing/duplicate gateway tokens and custom Messages errors.
- [ ] Write `TestClaudeOAuthForwardsCallerBetaAndVersionOnlyToAnthropic` with an upstream HTTP fixture plus redirect target; assert exact arbitrary beta values and bearer only at the configured origin, existing Responses credentials keep current defaults, and no sensitive value appears in errors.
- [ ] Run `devenv shell -- go test ./internal/api ./internal/upstreamauth ./internal/provider/anthropic -run 'TestCaptureNativeClaudeOAuth|TestAuthenticateWithError|TestClaudeOAuthForwards' -count=1`; expect RED.
- [ ] Implement the named interfaces, validated single-value headers, and `Authenticate` delegation. Never use an environment credential variable as the Claude subscription credential.
- [ ] Rerun focused tests and existing `Authenticate`/Anthropic OAuth tests; expect PASS.

### Task 3: Messages Decoder and Provider Eligibility

**Files:** Create `internal/api/messages_decode.go`, `internal/api/messages_decode_test.go`; modify `internal/executor/service.go`, `internal/executor/stream.go`, `internal/router/policy.go`, `internal/router/eligibility.go`, `internal/router/eligibility_test.go`, `internal/executor/service_test.go`, `internal/executor/stream_test.go`, `internal/inference/types.go` only where typed native data is needed.

**Interfaces:** Produce `api.DecodedMessages{Request inference.Request; RequiredProvider string}` and `api.DecodeMessagesRequest(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) (DecodedMessages, error)`. Add `RequiredProvider string` to `executor.Input`, `router.DecisionInput`, and `router.EligibilityInput`, passed unchanged across both execute and stream selection. An empty value means no extra restriction; `"anthropic"` excludes all other providers, including fallbacks, for a native-only feature.

- [ ] Write `TestDecodeMessagesRequest` with named cases for text/system blocks, image blocks, ordered assistant `tool_use` and user `tool_result`, tool schemas/choice, required `max_tokens`, `stream`, and `?beta=true`; check portable items and provenance. Include malformed/trailing JSON, body limit, invalid role/model, and unknown mandatory fields.
- [ ] Write `TestMessagesNativeOnlyFeaturesRequireAnthropic` and `TestExecutorRequiredProviderFiltersStreamFallback`: signed thinking or untranslatable cache/beta content admits only Anthropic; an OAuth-only beta capability does **not** force Anthropic; ordinary `mindctl-auto` requests still admit every otherwise eligible provider; an explicit incompatible model fails before provider I/O.
- [ ] Run `devenv shell -- go test ./internal/api ./internal/router ./internal/executor -run 'TestDecodeMessagesRequest|TestMessagesNativeOnlyFeatures|TestExecutorRequiredProvider' -count=1`; expect RED.
- [ ] Implement body-bounded strict decoding and native-only feature classification. Keep provider-only opaque fields marked with source; never turn provider data into public portable text. Thread `RequiredProvider` through routing and *all* fallback/stream paths without modifying default eligibility.
- [ ] Rerun focused tests; expect PASS.

### Task 4: Non-Streaming Responses and Error Envelope

**Files:** Create `internal/api/messages_encode.go`, `internal/api/messages_encode_test.go`; modify `internal/provider/anthropic/types.go` and `internal/provider/anthropic/client_test.go` only to preserve necessary provider-native continuation blocks.

**Interfaces:** Produce `api.messageFromResult(result inference.Result, providerID string) (messageResponseBody, error)` and `api.writeMessagesError(w http.ResponseWriter, err error)`. The response includes the actual routed model, Anthropic `stop_reason`, ordered content, and known usage; `providerID` authorizes Anthropic-native content only when equal to `"anthropic"`.

- [ ] Write `TestMessageFromResult` for text, mixed text/tools, signed thinking continuation from Anthropic, non-Anthropic synthesized text/tools, accurate stop reason and usage, and rejection of untranslatable provider-native blocks.
- [ ] Write `TestWriteMessagesError` for bad request, missing/invalid auth, no eligible model, upstream 429/5xx, and sensitive upstream error text; assert `{ "type": "error", "error": { "type": ..., "message": ... } }`, status, and no credential leakage.
- [ ] Run `devenv shell -- go test ./internal/api ./internal/provider/anthropic -run 'TestMessageFromResult|TestWriteMessagesError' -count=1`; expect RED.
- [ ] Implement the two interfaces, using the existing `errorDetail` classification without exposing provider error bodies. Preserve Anthropic-native blocks only when provenance and `providerID` match; do not change Responses output encoding.
- [ ] Rerun focused tests; expect PASS.

### Task 5: Anthropic-Compatible Streaming Writer

**Files:** Create `internal/api/messages_stream.go`, `internal/api/messages_stream_test.go`; modify `internal/inference/events.go`, `internal/provider/anthropic/client.go`, `internal/provider/anthropic/client_test.go`, `internal/executor/stream.go`, `internal/executor/stream_test.go`.

**Interfaces:** Produce `api.messagesSSEWriter` implementing `executor.EventWriter` (`Start(executor.StreamMetadata)`, `WriteEvent(context.Context, inference.Event)`, `WriteTerminalError(context.Context, error)`). Add `Provider string` to `executor.StreamMetadata` for safe provenance; add typed thinking/signature fields to `inference.Event` only when the Anthropic parser can validate them. Never expose the raw `inference.Event.Data` payload or `ProviderRequestID` directly.

- [ ] Write `TestMessagesSSEWriterLifecycle` for `message_start`, indexed content block starts/text/thinking/tool-input deltas/stops, `message_delta` with stop reason and usage, and `message_stop`; assert block IDs/indexes and `text/event-stream` framing.
- [ ] Write `TestMessagesSSEWriterCancellationAndTerminalError` and `TestAnthropicThinkingSignatureIsTyped`: cancellation stops writes; a post-emission error emits a safe `event: error` without a false success frame; native signature is emitted only with `Provider == "anthropic"`; unknown beta/native raw data is never echoed into a non-Anthropic stream.
- [ ] Run `devenv shell -- go test ./internal/api ./internal/executor ./internal/provider/anthropic -run 'TestMessagesSSEWriter|TestAnthropicThinkingSignature' -count=1`; expect RED.
- [ ] Implement SSE state/index tracking and typed event mapping; mark `StreamMetadata.Provider` from the actually selected candidate. Preserve existing retry-before-first-output semantics and cancellation; do not modify Responses writer's event contract.
- [ ] Rerun focused tests; expect PASS.

### Task 6: Handler Integration and Protocol Regression

**Files:** Create `internal/api/messages.go`, `internal/api/messages_test.go`; modify `internal/app/app.go`, `internal/app/app_test.go`, `internal/api/auth.go`, `internal/api/responses_test.go` only where shared authentication behavior needs a control test.

**Interfaces:** Produce `api.NewMessagesHandler(service api.ResponseExecutor, cfg api.ResponsesConfig) http.Handler`. In `app.New`, register `/v1/messages` only when `Config.ClaudeMessages.Enabled`; compose `AuthenticateWithError` outside `CaptureNativeClaudeOAuth` outside the Messages handler. Reuse `providerCredentials` and existing executor; leave `/v1/responses` composition intact.

- [ ] Write `TestMessagesRouteIsOptInAndAuthenticated` for disabled 404; enabled method 405, missing/bad gateway token 401 in Anthropic error format (even with a valid Claude bearer), authenticated malformed body 400, and valid `?beta=true` routed with both credentials isolated.
- [ ] Write `TestMessagesHandlerCrossProviderAndStream`: choose Anthropic and a separately credentialed alternative from fixtures, verify text then tool-result continuation in non-streaming and SSE modes, and ensure native OAuth/beta never hit the alternative. Unknown explicit models produce a clear error; a failure after visible SSE output cannot trigger another provider attempt.
- [ ] Run `devenv shell -- go test ./internal/api ./internal/app -run 'TestMessagesRoute|TestMessagesHandler' -count=1`; expect RED.
- [ ] Wire handler, decoder, executor, response and stream encoders. Use the shared error writer in the auth middleware so a pre-handler 401 is Anthropic-shaped. Adjust any existing API defaults only with unchanged-path regression coverage.
- [ ] Rerun focused tests and `devenv shell -- go test ./internal/api ./internal/app ./internal/provider/anthropic ./internal/executor ./internal/router -count=1`; expect PASS.

### Task 7: Claude Status and Opt-In Setup

**Files:** Modify `cmd/mindctl/main.go`, `cmd/mindctl/main_test.go`, `README.md`; modify `config.example.yaml` only for additional example annotations.

**Interfaces:** Produce `codexConnected() (bool, error)` unchanged, `claudeConnected() (bool, error)` and the existing `newStatusCommand()` with deterministic `codex`, `claude` table rows. Claude checks only global `~/.claude/settings.json` top-level `env.ANTHROPIC_BASE_URL == "http://127.0.0.1:8080"` (allow one trailing slash); `status claude` prints one status line.

- [ ] Expand `TestStatusCommand` for missing/matching/different/malformed/unreadable Claude settings, `status claude`, multi-harness table, unknown harness, and `status codex` succeeding even when Claude JSON is invalid. Assert a missing/invalid gateway YAML has no effect; do not read the gateway config, contact the network, or claim shell/project override detection.
- [ ] Run `devenv shell -- go test ./cmd/mindctl -run '^TestStatusCommand$' -count=1`; expect RED.
- [ ] Implement `claudeConnected`, dispatch by requested harness, and aggregate table output. Document opt-in `claude_messages.enabled`, non-secret global base URL, gateway-token launcher via `ANTHROPIC_CUSTOM_HEADERS`, native login/no API-key variables, model override guidance, rollback to Headroom, and separate provider billing.
- [ ] Rerun focused tests; expect PASS.

### Final Validation and Handoff

- [ ] Run `devenv shell -- go test ./...` and `devenv shell -- go build ./cmd/mindctl`; report exact results, including any existing devenv startup warning.
- [ ] Run `git diff --check` and inspect `git status --short`; preserve pre-existing dirty work and do not commit unless explicitly requested.
- [ ] If credentials and opt-in approval are available, run a real Claude Code session with a text turn, streamed tool call/response, Anthropic-subscription selection, and separately credentialed non-Anthropic selection. If not, explicitly label live OAuth and cross-provider proof unverified; do not claim deployment, publishing, or a functional live login from fixture tests.
