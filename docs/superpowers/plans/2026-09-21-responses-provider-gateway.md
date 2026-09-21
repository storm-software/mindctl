# Responses Provider Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the routing core into an OpenAI Responses-compatible gateway that executes pinned conversations through native OpenAI, Anthropic, and Gemini adapters.

**Architecture:** Requests are decoded into a provider-neutral item model, routed through the pure policy from the core plan, and executed by native adapters behind one interface. The gateway owns response IDs and canonical transcripts so `previous_response_id` works across provider protocols; streaming retries stop permanently after the first client-visible event.

**Tech Stack:** Go 1.26.7, `net/http`, `encoding/json`, provider REST/SSE protocols, SQLite repositories from the core plan.

**Spec:** `docs/superpowers/specs/2026-09-21-jev-model-router-design.md`

## Prerequisite

Complete `docs/superpowers/plans/2026-09-21-jev-router-core.md` first. This plan consumes its `domain`, `router`, `config`, `contentcrypto`, `storage`, and `app` interfaces.

## Global Constraints

- Expose `POST /v1/responses` as the only initial generation endpoint.
- `model: "mindctl-auto"` enables routing; a concrete model bypasses selection.
- Automatic escalation defaults on; concrete-model escalation defaults off unless `X-Mindctl-Allow-Escalation` is true.
- Support text/images, custom functions/results, JSON Schema output, non-streaming responses, and SSE text/tool events.
- Treat unsupported provider-hosted tools as capability constraints; never silently approximate them.
- Store a canonical transcript and pin the first selected model/provider; never automatically downgrade it.
- Retry streaming only before the first client-visible event.
- Return the selected concrete model in the standard response `model` field.
- Persist encrypted prompt/output content and every attempt before reporting success.
- Run repository commands through `devenv shell --`.

## Review Focus

- Empty, malformed, unknown-field, and over-limit request bodies must return stable 400/413 Responses-style errors without provider calls; Task 1 tests each case.
- A hosted tool unsupported by a provider must exclude that provider for automatic routing and reject an explicit incompatible model; Tasks 2-4 test both paths.
- A malformed, unknown, or foreign-client `previous_response_id` must be rejected without revealing whether another client owns it; Tasks 1 and 5 cover these cases.
- Client disconnects and SSE events larger than `bufio.Scanner`'s default token size must cancel upstream work without truncation or goroutine leaks; Task 7 uses `bufio.Reader` and tests both.
- Missing/malformed provider usage and finish metadata must not panic, and provider-specific opaque continuation metadata such as Gemini thought signatures must survive same-provider turns; Tasks 2-5 test this.

---

### Task 1: Canonical Responses model, authentication, and decoding

**Files:**

- Create: `internal/inference/types.go`
- Create: `internal/inference/events.go`
- Create: `internal/inference/validation.go`
- Create: `internal/api/auth.go`
- Create: `internal/api/errors.go`
- Create: `internal/api/responses_decode.go`
- Create: `internal/api/responses_decode_test.go`
- Modify: `internal/config/types.go`
- Modify: `internal/config/load_test.go`

**Interfaces:**

- Consumes: gateway client-auth environment reference and model catalog from the core plan.
- Produces: `api.DecodeResponseRequest(http.ResponseWriter, *http.Request, int64) (inference.Request, api.Controls, error)`, `api.Authenticate(next http.Handler, tokens TokenSource) http.Handler`, and canonical inference types used by all adapters.

- [ ] **Step 1: Write failing request-decoding tests**

```go
func TestDecodeResponseRequest(t *testing.T) {
    req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
      "model":"mindctl-auto","instructions":"be concise","input":"hello","stream":false
    }`))
    got, controls, err := DecodeResponseRequest(httptest.NewRecorder(), req, 1<<20)
    if err != nil || got.Model != "mindctl-auto" || got.Input[0].Text != "hello" || controls.AllowEscalation != nil {
        t.Fatalf("request=%+v controls=%+v err=%v", got, controls, err)
    }
}

func TestDecodeRejectsUnknownMalformedAndOversizedBodies(t *testing.T) {
    for _, body := range []string{`{"model":"mindctl-auto","wat":1}`, `{`, ``} {
        _, _, err := decode(t, body, 1<<20)
        if err == nil { t.Fatalf("body %q unexpectedly passed", body) }
    }
    _, _, err := decode(t, `{"model":"mindctl-auto","input":"0123456789"}`, 8)
    if !errors.Is(err, api.ErrBodyTooLarge) { t.Fatalf("err=%v", err) }
}
```

- [ ] **Step 2: Write failing auth and header-control tests**

```go
func TestAuthenticateUsesConstantTimeTokenMatch(t *testing.T) {
    called := false
    h := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), staticTokens("client-a"))
    req := httptest.NewRequest("POST", "/v1/responses", nil)
    req.Header.Set("Authorization", "Bearer client-a")
    rr := httptest.NewRecorder()
    h.ServeHTTP(rr, req)
    if rr.Code != 200 || !called { t.Fatalf("status=%d called=%v", rr.Code, called) }
}

func TestDecodeControlsValidatesTierAndEscalationHeaders(t *testing.T) {
    req := validRequest()
    req.Header.Set("X-Mindctl-Min-Tier", "T5")
    req.Header.Set("X-Mindctl-Max-Tier", "T3")
    if _, _, err := DecodeResponseRequest(httptest.NewRecorder(), req, 1<<20); err == nil { t.Fatal("expected invalid bounds") }
}
```

- [ ] **Step 3: Run the tests and verify failure**

Run: `devenv shell -- go test ./internal/api ./internal/inference -run 'Test(Decode|Authenticate)' -v`

Expected: FAIL because canonical inference and API decoding types do not exist.

- [ ] **Step 4: Implement canonical types and strict decoding**

```go
type Request struct {
    ID, Model, Instructions, PreviousResponseID string
    Input      []Item
    Tools      []Tool
    TextFormat *JSONSchemaFormat
    Stream     bool
    MaxOutputTokens int64
}

type Item struct {
    Type, Role, Text, CallID, Name string
    ImageURL json.RawMessage
    Arguments, Output json.RawMessage
    ProviderData json.RawMessage
}

type Result struct {
    ID, Model, ProviderRequestID, Status string
    Output []Item
    Usage Usage
}

type Usage struct {
    InputTokens, OutputTokens, CachedInputTokens int64
    Known bool
}

type Event struct {
    Type string
    ResponseID, ItemID, CallID, Name, Delta string
    ArgumentsDelta string
    Data json.RawMessage
}
```

Use `http.MaxBytesReader`, `json.Decoder.DisallowUnknownFields`, a second decode expecting `io.EOF`, and typed validation errors. Parse tier and escalation headers without mutating the canonical body. Add `ClientAuth.MaxBodyBytes` with a 16 MiB example default.

- [ ] **Step 5: Implement stable API errors**

```go
type ErrorBody struct { Error ErrorDetail `json:"error"` }
type ErrorDetail struct {
    Message string `json:"message"`
    Type string `json:"type"`
    Param string `json:"param,omitempty"`
    Code string `json:"code,omitempty"`
}
```

Map syntax/validation errors to 400, missing authentication to 401, body limits to 413, and unexpected errors to 500. Do not return raw provider or SQLite error strings to clients.

- [ ] **Step 6: Run tests and commit**

Run: `devenv shell -- go test ./internal/api ./internal/inference ./internal/config -v`

Expected: PASS.

```bash
git add internal/inference internal/api internal/config
git commit -m "feat(api): decode authenticated Responses requests"
```

### Task 2: Provider contract and OpenAI native adapter

**Files:**

- Create: `internal/provider/provider.go`
- Create: `internal/provider/registry.go`
- Create: `internal/provider/openai/client.go`
- Create: `internal/provider/openai/types.go`
- Create: `internal/provider/openai/client_test.go`

**Interfaces:**

- Consumes: canonical `inference.Request`/`Result` and `domain.Model`.
- Produces: `provider.Provider`, `provider.Stream`, `provider.Registry`, normalized `provider.Error`, and the OpenAI adapter.

- [ ] **Step 1: Write the provider interface and failing OpenAI translation test**

```go
type Provider interface {
    Execute(context.Context, domain.Model, inference.Request) (inference.Result, error)
    Stream(context.Context, domain.Model, inference.Request) (Stream, error)
}

type Stream interface {
    Next(context.Context) (inference.Event, error)
    Close() error
}

func TestExecuteTranslatesOpenAIRequestAndUsage(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body map[string]any
        json.NewDecoder(r.Body).Decode(&body)
        if body["model"] != "gpt-test" || body["input"] == nil { t.Fatalf("body=%v", body) }
        w.Header().Set("x-request-id", "openai-req")
        io.WriteString(w, `{"id":"upstream","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":3,"output_tokens":1,"input_tokens_details":{"cached_tokens":2}}}`)
    }))
    got, err := newClient(server.URL).Execute(context.Background(), openAIModel(), textRequest())
    if err != nil || got.ProviderRequestID != "openai-req" || got.Usage.CachedInputTokens != 2 { t.Fatalf("got=%+v err=%v", got, err) }
}
```

- [ ] **Step 2: Add explicit unsupported-tool and malformed-usage tests**

```go
func TestOpenAIRejectsUnsupportedExplicitFeature(t *testing.T) {
    _, err := newClient("unused").Execute(context.Background(), modelWithoutHostedTools(), hostedToolRequest("computer_use"))
    var unsupported *provider.UnsupportedFeatureError
    if !errors.As(err, &unsupported) { t.Fatalf("err=%v", err) }
}

func TestOpenAIMissingUsageDoesNotPanic(t *testing.T) {
    got := executeFixture(t, `{"id":"x","status":"completed","output":[]}`)
    if got.Usage.Known { t.Fatalf("usage=%+v", got.Usage) }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/provider/... -run 'Test(OpenAI|Execute)' -v`

Expected: FAIL with missing provider packages.

- [ ] **Step 4: Implement registry, error classes, and OpenAI REST mapping**

```go
type ErrorKind string
const (
    ErrorRetryable ErrorKind = "retryable"
    ErrorRateLimit ErrorKind = "rate_limit"
    ErrorOverloaded ErrorKind = "overloaded"
    ErrorAuthentication ErrorKind = "authentication"
    ErrorInvalidRequest ErrorKind = "invalid_request"
)

type Error struct { Kind ErrorKind; Status int; RequestID string; Err error }
func (e *Error) Error() string { return fmt.Sprintf("provider request failed (%s)", e.Kind) }
func (e *Error) Unwrap() error { return e.Err }
```

Post canonical requests to `/v1/responses`, set bearer auth from the provider credential, force the configured upstream model ID, and normalize output items, usage, status, and request ID. Keep provider response bodies out of error text and logs.

- [ ] **Step 5: Run tests and commit**

Run: `devenv shell -- go test ./internal/provider/... -v`

Expected: PASS.

```bash
git add internal/provider
git commit -m "feat(provider): add native OpenAI Responses adapter"
```

### Task 3: Anthropic native adapter

**Files:**

- Create: `internal/provider/anthropic/client.go`
- Create: `internal/provider/anthropic/types.go`
- Create: `internal/provider/anthropic/client_test.go`

**Interfaces:**

- Consumes: `provider.Provider`, canonical items, and model capabilities from Tasks 1-2.
- Produces: Anthropic Messages request/response and SSE translation behind the shared provider interface.

- [ ] **Step 1: Write the failing message/tool translation test**

```go
func TestAnthropicTranslatesInstructionsFunctionsAndToolResults(t *testing.T) {
    server := anthropicFixture(t, func(body messagesRequest) {
        if body.System != "be concise" || body.Model != "claude-test" || len(body.Tools) != 1 { t.Fatalf("body=%+v", body) }
        if body.Messages[1].Content[0].Type != "tool_result" { t.Fatalf("messages=%+v", body.Messages) }
    }, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":2}}`)
    got, err := newAnthropic(server.URL).Execute(context.Background(), anthropicModel(), functionResultRequest())
    if err != nil || got.Output[0].Text != "done" || got.Usage.InputTokens != 8 { t.Fatalf("got=%+v err=%v", got, err) }
}
```

- [ ] **Step 2: Write image, schema, unsupported-hosted-tool, and missing-usage tests**

```go
func TestAnthropicCapabilityValidation(t *testing.T) {
    client := newAnthropic("unused")
    if _, err := client.Execute(context.Background(), anthropicModel(), hostedToolRequest("file_search")); err == nil { t.Fatal("expected unsupported feature") }
    if _, err := client.Execute(context.Background(), textOnlyAnthropicModel(), imageRequest()); err == nil { t.Fatal("expected image rejection") }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/provider/anthropic -v`

Expected: FAIL because the adapter does not exist.

- [ ] **Step 4: Implement Messages mapping**

Map gateway instructions to Anthropic `system`, user/assistant text and images to content blocks, functions to `tools`, function calls to `tool_use`, results to `tool_result`, and supported schema output to `output_config.format`. Normalize content blocks back into gateway output items. Preserve the Anthropic `request-id` header and any provider metadata needed to continue a same-provider turn.

```go
func (c *Client) Execute(ctx context.Context, model domain.Model, req inference.Request) (inference.Result, error) {
    body, err := toMessagesRequest(model, req)
    if err != nil { return inference.Result{}, err }
    var response messagesResponse
    requestID, err := c.post(ctx, "/v1/messages", body, &response)
    if err != nil { return inference.Result{}, err }
    return fromMessagesResponse(response, requestID), nil
}
```

- [ ] **Step 5: Run tests and commit**

Run: `devenv shell -- go test ./internal/provider/anthropic ./internal/provider -v`

Expected: PASS.

```bash
git add internal/provider/anthropic
git commit -m "feat(provider): add native Anthropic adapter"
```

### Task 4: Gemini native adapter

**Files:**

- Create: `internal/provider/gemini/client.go`
- Create: `internal/provider/gemini/types.go`
- Create: `internal/provider/gemini/client_test.go`

**Interfaces:**

- Consumes: the shared provider contract and canonical inference types.
- Produces: Gemini `generateContent` and streaming translation behind `provider.Provider`.

- [ ] **Step 1: Write the failing content/function/schema translation test**

```go
func TestGeminiTranslatesContentFunctionsAndSchema(t *testing.T) {
    server := geminiFixture(t, func(path string, body generateRequest) {
        if !strings.Contains(path, "models/gemini-test:generateContent") { t.Fatalf("path=%s", path) }
        if len(body.Tools) != 1 || body.GenerationConfig.ResponseSchema == nil { t.Fatalf("body=%+v", body) }
    }, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
    got, err := newGemini(server.URL).Execute(context.Background(), geminiModel(), schemaFunctionRequest())
    if err != nil || got.Output[0].Text != "done" || got.Usage.OutputTokens != 1 { t.Fatalf("got=%+v err=%v", got, err) }
}
```

- [ ] **Step 2: Add unsupported-tool and absent-candidate tests**

```go
func TestGeminiRejectsUnsupportedHostedTool(t *testing.T) {
    _, err := newGemini("unused").Execute(context.Background(), geminiModel(), hostedToolRequest("code_interpreter"))
    var unsupported *provider.UnsupportedFeatureError
    if !errors.As(err, &unsupported) { t.Fatalf("err=%v", err) }
}

func TestGeminiEmptyCandidatesReturnsNormalizedError(t *testing.T) {
    _, err := executeGeminiFixture(t, `{"candidates":[],"usageMetadata":{}}`)
    var providerErr *provider.Error
    if !errors.As(err, &providerErr) { t.Fatalf("err=%v", err) }
}

func TestGeminiThoughtSignatureSurvivesRoundTrip(t *testing.T) {
    got := executeGeminiFixture(t, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"opaque-signature"}]}}]}`)
    next := toGenerateRequest(geminiModel(), requestWithPriorItem(got.Output[0]))
    if next.Contents[0].Parts[0].ThoughtSignature != "opaque-signature" { t.Fatalf("next=%+v", next) }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/provider/gemini -v`

Expected: FAIL because the adapter does not exist.

- [ ] **Step 4: Implement Gemini mapping**

Map canonical items to `systemInstruction`, `contents`, inline/file image data, function declarations/calls/responses, and `generationConfig.responseMimeType` plus `responseSchema`. Post to `/v1beta/models/{model}:generateContent` or `:streamGenerateContent?alt=sse` with the server-held key in `x-goog-api-key`, then normalize parts, finish reasons, safety/empty-candidate errors, usage, request IDs, and opaque thought signatures.

- [ ] **Step 5: Run all adapter tests and commit**

Run: `devenv shell -- go test ./internal/provider/... -v`

Expected: PASS.

```bash
git add internal/provider/gemini
git commit -m "feat(provider): add native Gemini adapter"
```

### Task 5: Canonical conversation and attempt persistence

**Files:**

- Create: `migrations/002_conversations.sql`
- Modify: `internal/storage/storage.go`
- Modify: `internal/storage/sqlite/repository.go`
- Create: `internal/storage/sqlite/conversation_test.go`
- Create: `internal/conversation/service.go`
- Create: `internal/conversation/service_test.go`

**Interfaces:**

- Consumes: encrypted content storage, client identity from auth, and canonical inference types.
- Produces: `conversation.Service.Start`, `conversation.Service.Resume`, `conversation.Service.CommitResult`, and transactional attempt records.

- [ ] **Step 1: Write failing ownership and unknown-response tests**

```go
func TestResumeRejectsUnknownAndForeignResponse(t *testing.T) {
    svc := testConversationService(t)
    if _, err := svc.Resume(context.Background(), "client-a", "resp_missing"); !errors.Is(err, conversation.ErrNotFound) { t.Fatalf("err=%v", err) }
    conv := startAndCommit(t, svc, "client-a")
    if _, err := svc.Resume(context.Background(), "client-b", conv.ResponseID); !errors.Is(err, conversation.ErrNotFound) { t.Fatalf("err=%v", err) }
}
```

- [ ] **Step 2: Write failing pin and canonical transcript tests**

```go
func TestCommitAndResumePreservesCanonicalTranscriptAndPin(t *testing.T) {
    svc := testConversationService(t)
    started, _ := svc.Start(context.Background(), "client-a", requestWithText("hello"))
    err := svc.CommitResult(context.Background(), started, pin("openai", "gpt-test", domain.T3), resultWithText("hi"))
    if err != nil { t.Fatal(err) }
    resumed, err := svc.Resume(context.Background(), "client-a", started.ResponseID)
    if err != nil || resumed.Pin.ModelID != "gpt-test" || transcriptText(resumed.Transcript) != "hello\nhi" { t.Fatalf("resumed=%+v err=%v", resumed, err) }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/conversation ./internal/storage/sqlite -run 'Test(Resume|Commit)' -v`

Expected: FAIL with missing conversation schema and service.

- [ ] **Step 4: Extend the schema and repositories**

Create `conversations`, `responses`, `transcript_items`, and `provider_attempts`. Scope lookups by both client ID and response ID. Encrypt transcript payloads, provider-specific continuation metadata, and failed/successful provider bodies through the existing keyring. Store provider/model/tier pins and escalation floor separately from content. Replay `ProviderData` only to the adapter that produced it; cross-provider escalation keeps canonical public content but drops incompatible opaque metadata.

```go
type Service interface {
    Start(context.Context, string, inference.Request) (Turn, error)
    Resume(context.Context, string, string) (Turn, error)
    BeginAttempt(context.Context, Turn, router.Decision) (Attempt, error)
    CommitResult(context.Context, Turn, router.Pin, inference.Result) error
    FailAttempt(context.Context, Attempt, error) error
}
```

- [ ] **Step 5: Run storage and conversation tests and commit**

Run:

```bash
devenv shell -- go test ./internal/conversation ./internal/storage/... -v
devenv shell -- go test -race ./internal/conversation ./internal/storage/...
```

Expected: PASS.

```bash
git add migrations/002_conversations.sql internal/storage internal/conversation
git commit -m "feat(storage): persist pinned canonical conversations"
```

### Task 6: Non-streaming routing and provider execution

**Files:**

- Create: `internal/executor/service.go`
- Create: `internal/executor/service_test.go`
- Modify: `internal/router/policy.go`
- Modify: `internal/domain/features.go`

**Interfaces:**

- Consumes: classifier, routing policy, provider registry, and conversation service.
- Produces: `executor.Service.Execute(context.Context, executor.Input) (executor.Output, error)` for one non-streaming attempt; verification/escalation is added in the final plan.

- [ ] **Step 1: Write the deterministic-route and Jev-route tests**

```go
func TestExecuteSkipsJevForCompatibleConversationPin(t *testing.T) {
    deps := fakeDepsWithPin("gpt-pinned", domain.T3)
    got, err := deps.Executor.Execute(context.Background(), inputWithPreviousResponse())
    if err != nil || deps.Classifier.Calls != 0 || deps.OpenAI.Calls != 1 || got.Result.Model != "gpt-pinned" { t.Fatalf("got=%+v deps=%+v err=%v", got, deps, err) }
}

func TestExecuteUsesJevForUncertainAutomaticRequest(t *testing.T) {
    deps := fakeDepsWithJudgment(domain.T4)
    got, err := deps.Executor.Execute(context.Background(), newAutomaticInput())
    if err != nil || deps.Classifier.Calls != 1 || got.Decision.Tier < domain.T4 { t.Fatalf("got=%+v err=%v", got, err) }
}
```

- [ ] **Step 2: Write explicit-model and unsupported-feature tests**

```go
func TestExecuteHonorsConcreteModelWithoutClassifier(t *testing.T) {
    deps := fakeDeps()
    got, err := deps.Executor.Execute(context.Background(), concreteModelInput("claude-test"))
    if err != nil || got.Decision.ModelID != "claude-test" || deps.Classifier.Calls != 0 { t.Fatalf("got=%+v err=%v", got, err) }
}

func TestAutoRouteExcludesProviderWithoutHostedTool(t *testing.T) {
    deps := fakeDepsWithHostedToolModels()
    got, err := deps.Executor.Execute(context.Background(), hostedToolInput("web_search"))
    if err != nil || got.Decision.Provider != "openai" { t.Fatalf("got=%+v err=%v", got, err) }
}

func TestAutoRouteExcludesUnavailableModel(t *testing.T) {
    deps := fakeDepsWithUnavailableCheapModel()
    got, err := deps.Executor.Execute(context.Background(), newAutomaticInput())
    if err != nil || got.Decision.ModelID == "unavailable-cheap" { t.Fatalf("got=%+v err=%v", got, err) }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/executor -v`

Expected: FAIL because the executor does not exist.

- [ ] **Step 4: Implement the orchestration path**

```go
type Service struct {
    classifier classifier.Classifier
    policy *router.Policy
    providers *provider.Registry
    conversations conversation.Service
}

type Output struct { Result inference.Result; Decision router.Decision; AttemptID string }
```

Resolve/resume the conversation, normalize features, apply deterministic conditions, call Jev only when needed, decide, persist the attempt before the provider call, execute the selected adapter, rewrite the gateway response ID/model, and transactionally commit output plus pin before returning.

- [ ] **Step 5: Add persistence-failure behavior tests**

```go
func TestExecuteRejectsBeforeProviderWhenAttemptCannotBePersisted(t *testing.T) {
    deps := fakeDeps()
    deps.Conversations.BeginAttemptErr = errors.New("sqlite unavailable")
    _, err := deps.Executor.Execute(context.Background(), newAutomaticInput())
    if err == nil || deps.OpenAI.Calls != 0 { t.Fatalf("calls=%d err=%v", deps.OpenAI.Calls, err) }
}
```

- [ ] **Step 6: Run tests and commit**

Run: `devenv shell -- go test ./internal/executor ./internal/router ./internal/conversation -v`

Expected: PASS.

```bash
git add internal/executor internal/router internal/domain
git commit -m "feat(gateway): execute routed non-streaming requests"
```

### Task 7: Streaming execution and `/v1/responses` handler

**Files:**

- Modify: `internal/inference/events.go`
- Create: `internal/provider/sse.go`
- Create: `internal/provider/sse_test.go`
- Create: `internal/executor/stream.go`
- Create: `internal/executor/stream_test.go`
- Create: `internal/api/responses.go`
- Create: `internal/api/responses_test.go`
- Modify: `internal/app/app.go`
- Modify: `config.example.yaml`

**Interfaces:**

- Consumes: the non-streaming executor, provider streams, API decoder/auth, and app wiring.
- Produces: full `POST /v1/responses` behavior and `executor.Service.Stream(context.Context, executor.Input, executor.EventWriter) error`.

- [ ] **Step 1: Write the large-event and cancellation SSE tests**

```go
func TestSSEReaderAcceptsEventLargerThanScannerLimit(t *testing.T) {
    payload := strings.Repeat("x", 128<<10)
    r := provider.NewSSEReader(strings.NewReader("event: response.output_text.delta\ndata: {\"delta\":\""+payload+"\"}\n\n"))
    event, err := r.Next(context.Background())
    if err != nil || len(event.Data) < len(payload) { t.Fatalf("len=%d err=%v", len(event.Data), err) }
}

func TestStreamCancellationClosesUpstream(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    stream := blockingFakeStream()
    cancel()
    if err := drain(ctx, stream); !errors.Is(err, context.Canceled) || !stream.Closed() { t.Fatalf("err=%v closed=%v", err, stream.Closed()) }
}
```

- [ ] **Step 2: Write pre-emission retry and post-emission terminal tests**

```go
func TestStreamRetriesBeforeFirstVisibleEvent(t *testing.T) {
    deps := streamDeps(failBeforeEvent(), successfulStream("ok"))
    err := deps.Executor.Stream(context.Background(), automaticInput(), deps.Writer)
    if err != nil || deps.Writer.Text() != "ok" || deps.Provider.Calls != 2 { t.Fatalf("text=%q calls=%d err=%v", deps.Writer.Text(), deps.Provider.Calls, err) }
}

func TestStreamDoesNotRetryAfterVisibleEvent(t *testing.T) {
    deps := streamDeps(streamThenFail("partial"), successfulStream("replacement"))
    err := deps.Executor.Stream(context.Background(), automaticInput(), deps.Writer)
    if err == nil || deps.Provider.Calls != 1 || !deps.Writer.HasTerminalError() || deps.Conversations.Floor != domain.T4 { t.Fatalf("calls=%d floor=%s err=%v", deps.Provider.Calls, deps.Conversations.Floor, err) }
}
```

- [ ] **Step 3: Run streaming tests and verify failure**

Run: `devenv shell -- go test ./internal/provider ./internal/executor -run 'Test(SSE|Stream)' -v`

Expected: FAIL because event and stream helpers do not exist.

- [ ] **Step 4: Implement line-safe SSE translation and emission tracking**

Use `bufio.Reader.ReadString('\n')`, not `bufio.Scanner`. Normalize OpenAI, Anthropic, and Gemini stream frames into canonical `inference.Event` values. The executor tracks `emitted bool`; it may reopen a stronger/same-tier candidate only while false. Always close upstream bodies and honor the request context.

- [ ] **Step 5: Write the HTTP compatibility test**

```go
func TestResponsesHandlerReturnsActualModelAndRoutingHeaders(t *testing.T) {
    h := newTestHandler(resultFor("resp_gateway", "claude-test", domain.T4))
    req := authenticatedRequest(`{"model":"mindctl-auto","input":"hello"}`)
    rr := httptest.NewRecorder()
    h.ServeHTTP(rr, req)
    var body struct { Model string `json:"model"` }
    if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil { t.Fatal(err) }
    if rr.Code != 200 || body.Model != "claude-test" || rr.Header().Get("X-Mindctl-Tier") != "T4" { t.Fatalf("status=%d headers=%v body=%s", rr.Code, rr.Header(), rr.Body.String()) }
}
```

- [ ] **Step 6: Wire the route and error mapping into the app**

Register the authenticated `POST /v1/responses` handler alongside unauthenticated health/readiness endpoints. Map policy/no-model errors to 400, unsupported explicit features to 400, missing prior responses to 404, rate-limit exhaustion to 429, provider overload to 503, and internal persistence failures to 503.

- [ ] **Step 7: Run gateway verification**

Run:

```bash
devenv shell -- go test ./...
devenv shell -- go test -race ./...
devenv shell -- go vet ./...
```

Expected: PASS.

- [ ] **Step 8: Commit the provider gateway increment**

```bash
git add internal/inference internal/provider internal/executor internal/api internal/app config.example.yaml
git commit -m "feat(gateway): expose routed Responses inference"
```
