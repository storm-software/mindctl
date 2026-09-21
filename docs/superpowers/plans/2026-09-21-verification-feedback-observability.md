# Verification Feedback and Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the gateway with hybrid verification, bounded escalation, feedback-driven conversation floors, retention controls, and content-safe production telemetry.

**Architecture:** Deterministic and model verification produce explicit evidence consumed by a bounded executor state machine. Feedback changes only the owning conversation, while outcomes remain offline inputs for future prior updates; OpenTelemetry and structured logs describe each purpose without exposing captured content.

**Tech Stack:** Go 1.26.7, `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3, Go `log/slog`, OpenTelemetry Go v1.46.0, `otelhttp` v0.71.0, SQLite and provider adapters from the earlier plans.

**Spec:** `docs/superpowers/specs/2026-09-21-jev-model-router-design.md`

## Prerequisite

Complete `docs/superpowers/plans/2026-09-21-jev-router-core.md` and `docs/superpowers/plans/2026-09-21-responses-provider-gateway.md` first.

## Global Constraints

- Run deterministic checks for every completed non-streaming attempt.
- Run a configured model verifier only for selected non-streaming requests and call it directly, never through routing.
- Escalate verification failures by at least one tier and never loop at T6.
- Bound escalation by attempts, estimated cost, and wall-clock duration.
- Permit streaming retries only before the first client-visible event.
- Negative feedback raises only the owning conversation floor; it does not update global priors online.
- Store raw prompts and answers encrypted by default with unlimited retention unless a nonzero period is configured.
- Emit request, response, conversation, decision, attempt, and provider IDs without plaintext content or credentials.
- Reject new inference when durable audit storage is unavailable.
- Run repository commands through `devenv shell --`.

## Review Focus

- The verifier model must not call the router or recursively invoke verification; Task 2 uses a provider spy to prove one direct call.
- A response that passes deterministic checks but fails model verification must advance at least one tier without exceeding attempts, cost, time, or T6; Task 3 covers each boundary independently.
- Duplicate negative feedback must be idempotent, foreign-client feedback must look absent, and a positive-to-negative transition must raise the floor once; Task 4 tests all three.
- Unlimited retention must delete nothing, finite retention must erase ciphertext but retain aggregate telemetry, and missing old keys must return a typed decrypt error without exposing content; Task 5 tests these cases.
- Logs, spans, and metrics must remain content-free even when provider errors contain prompt fragments; Task 6 captures all telemetry sinks and searches for secret markers.

---

### Task 1: Deterministic verification

**Files:**

- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/verifier/verifier.go`
- Create: `internal/verifier/deterministic.go`
- Create: `internal/verifier/deterministic_test.go`

**Interfaces:**

- Consumes: canonical `inference.Request` and `inference.Result` from the provider gateway plan.
- Produces: `verifier.Verifier`, `verifier.Result`, evidence codes, and `verifier.NewDeterministic()`.

- [ ] **Step 1: Add the pinned JSON Schema dependency**

Run: `devenv shell -- go get github.com/santhosh-tekuri/jsonschema/v6@v6.0.3`

Expected: `go.mod` and `go.sum` pin v6.0.3.

- [ ] **Step 2: Write failing completion, empty-output, function, and schema tests**

```go
func TestDeterministicVerification(t *testing.T) {
    cases := []struct{name string; req inference.Request; result inference.Result; want Status; code string}{
        {"completed text", textRequest(), completedText("ok"), Pass, "completed"},
        {"empty output", textRequest(), completedText(""), Fail, "empty_output"},
        {"unknown function", functionRequest("lookup"), functionResult("delete_all", `{}`), Fail, "unknown_function"},
        {"invalid arguments", functionRequest("lookup"), functionResult("lookup", `{`), Fail, "invalid_arguments_json"},
        {"valid schema", schemaRequest(personSchema()), completedText(`{"name":"Ada"}`), Pass, "schema_valid"},
        {"invalid schema", schemaRequest(personSchema()), completedText(`{"name":3}`), Fail, "schema_invalid"},
    }
    v := NewDeterministic()
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got, _ := v.Verify(context.Background(), Input{Request:tc.req, Candidate:tc.result})
            if got.Status != tc.want || !hasEvidence(got, tc.code) { t.Fatalf("got=%+v", got) }
        })
    }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/verifier -run TestDeterministicVerification -v`

Expected: FAIL because verifier types do not exist.

- [ ] **Step 4: Implement deterministic verification**

```go
type Status string
const ( Pass Status = "pass"; Fail Status = "fail"; Indeterminate Status = "indeterminate" )

type Evidence struct { Code, Message string }
type Result struct { Status Status; Confidence float64; Evidence []Evidence }
type Input struct { Request inference.Request; Candidate inference.Result; Rubric string }
type Verifier interface { Verify(context.Context, Input) (Result, error) }
```

For JSON Schema, unmarshal output into `any`, use `jsonschema.NewCompiler()`, set `DefaultDraft(jsonschema.Draft2020)`, add the request schema as an in-memory resource, compile it, and validate the instance. Treat malformed caller schemas as request errors and candidate violations as verification failures.

- [ ] **Step 5: Add output-limit and provider-status cases**

```go
func TestDeterministicIndeterminateForUnknownProviderStatus(t *testing.T) {
    got, err := NewDeterministic().Verify(context.Background(), Input{Request:textRequest(), Candidate:inference.Result{Status:"unknown"}})
    if err != nil || got.Status != Indeterminate { t.Fatalf("got=%+v err=%v", got, err) }
}
```

- [ ] **Step 6: Run tests and commit**

Run: `devenv shell -- go test ./internal/verifier -v`

Expected: PASS.

```bash
git add go.mod go.sum internal/verifier
git commit -m "feat(verifier): validate routed responses deterministically"
```

### Task 2: Direct model verifier and selection policy

**Files:**

- Create: `internal/verifier/model.go`
- Create: `internal/verifier/model_test.go`
- Create: `internal/verifier/policy.go`
- Create: `internal/verifier/policy_test.go`
- Modify: `internal/config/types.go`
- Modify: `internal/config/load_test.go`
- Modify: `config.example.yaml`

**Interfaces:**

- Consumes: one explicitly configured verifier model and its native provider adapter.
- Produces: `verifier.ModelVerifier`, `verifier.Policy.ShouldVerify(verifier.PolicyInput) bool`, and strict verifier configuration.

- [ ] **Step 1: Write the failing direct-call and no-recursion test**

```go
func TestModelVerifierCallsProviderDirectlyOnce(t *testing.T) {
    providerSpy := &fakeProvider{Result: verifierJSON(`{"pass":false,"confidence":0.91,"reason":"unsupported claim"}`)}
    v := NewModelVerifier(providerSpy, verifierModel())
    got, err := v.Verify(context.Background(), Input{Request:textRequest(), Candidate:completedText("claim"), Rubric:"Answer the request correctly"})
    if err != nil || got.Status != Fail || providerSpy.Calls != 1 { t.Fatalf("got=%+v calls=%d err=%v", got, providerSpy.Calls, err) }
}
```

The production constructor accepts only `provider.Provider` and a concrete verifier model. No router or executor dependency is available to the verifier.

- [ ] **Step 2: Write failing verifier-policy tests**

```go
func TestPolicySelectsOnlyConfiguredNonStreamingRisk(t *testing.T) {
    p := Policy{MinTier:domain.T4, TaskTypes:map[domain.TaskType]bool{domain.TaskCoding:true}, RiskAt:0.7}
    if !p.ShouldVerify(PolicyInput{Tier:domain.T4, TaskType:domain.TaskCoding, Risk:0.8, Stream:false}) { t.Fatal("expected verification") }
    if p.ShouldVerify(PolicyInput{Tier:domain.T4, TaskType:domain.TaskCoding, Risk:0.8, Stream:true}) { t.Fatal("streaming cannot use model verification") }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/verifier -run 'Test(Model|Policy)' -v`

Expected: FAIL with missing model verifier and policy.

- [ ] **Step 4: Implement the direct verifier request**

Build a canonical request containing the original request, candidate output, and narrow rubric. Force JSON Schema output:

```json
{
  "type": "object",
  "properties": {
    "pass": { "type": "boolean" },
    "confidence": { "type": "number", "minimum": 0, "maximum": 1 },
    "reason": { "type": "string" }
  },
  "required": ["pass", "confidence", "reason"],
  "additionalProperties": false
}
```

Call the configured provider directly, parse strictly, and map malformed verifier output to `Indeterminate`. Tag the request purpose as verifier in attempt telemetry without sending it through `router.Policy` or `executor.Service`.

- [ ] **Step 5: Add strict configuration**

Add verifier model ID, minimum tier, task types, risk threshold, and behavior for indeterminate results (`pass`, `fail`, or `escalate`; example default `escalate`). Validation must reject a missing verifier model or a model ID absent from the catalog.

- [ ] **Step 6: Run tests and commit**

Run: `devenv shell -- go test ./internal/verifier ./internal/config -v`

Expected: PASS.

```bash
git add internal/verifier internal/config config.example.yaml
git commit -m "feat(verifier): add policy-gated model verification"
```

### Task 3: Bounded verification and escalation state machine

**Files:**

- Modify: `internal/executor/service.go`
- Create: `internal/executor/escalation.go`
- Create: `internal/executor/escalation_test.go`
- Modify: `internal/storage/storage.go`
- Modify: `internal/storage/sqlite/repository.go`
- Create: `migrations/003_verification.sql`
- Modify: `internal/config/types.go`
- Modify: `config.example.yaml`

**Interfaces:**

- Consumes: deterministic/model verifiers, router candidates, provider registry, and attempt repository.
- Produces: `executor.Budget`, escalation outcomes, persisted verifier runs, and completed multi-attempt non-streaming execution.

- [ ] **Step 1: Write failing tier-advance and T6 tests**

```go
func TestVerificationFailureEscalatesAtLeastOneTier(t *testing.T) {
    deps := escalationDeps(domain.T3, verifier.Fail, domain.T4, verifier.Pass)
    got, err := deps.Executor.Execute(context.Background(), automaticInput())
    if err != nil || tiers(deps.Attempts) != "T3,T4" || got.Decision.Tier != domain.T4 { t.Fatalf("attempts=%v got=%+v err=%v", deps.Attempts, got, err) }
}

func TestT6FailureDoesNotLoop(t *testing.T) {
    deps := escalationDeps(domain.T6, verifier.Fail)
    _, err := deps.Executor.Execute(context.Background(), automaticInput())
    if !errors.Is(err, executor.ErrVerificationFailed) || len(deps.Attempts) != 1 { t.Fatalf("attempts=%d err=%v", len(deps.Attempts), err) }
}
```

- [ ] **Step 2: Write one test per budget**

```go
func TestEscalationBudgets(t *testing.T) {
    tests := []struct{name string; budget executor.Budget; want executor.StopReason}{
        {"attempts", executor.Budget{MaxAttempts:1, MaxCost:10, MaxDuration:time.Hour}, executor.StopAttempts},
        {"cost", executor.Budget{MaxAttempts:5, MaxCost:0.0001, MaxDuration:time.Hour}, executor.StopCost},
        {"duration", executor.Budget{MaxAttempts:5, MaxCost:10, MaxDuration:time.Nanosecond}, executor.StopDuration},
    }
    for _, tc := range tests { t.Run(tc.name, func(t *testing.T) { assertStopsFor(t, tc.budget, tc.want) }) }
}
```

Add a concrete-model control test in the same file:

```go
func TestConcreteModelEscalatesOnlyWhenCallerOptsIn(t *testing.T) {
    assertConcreteModelAttempts(t, false, 1)
    assertConcreteModelAttempts(t, true, 2)
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/executor -run 'Test(Verification|T6|Escalation)' -v`

Expected: FAIL because the state machine and budgets do not exist.

- [ ] **Step 4: Implement escalation and persistence**

```go
type Budget struct { MaxAttempts int; MaxCost float64; MaxDuration time.Duration }
type AttemptOutcome struct {
    AttemptID string
    Tier domain.Tier
    ModelID string
    Verification verifier.Result
    ActualCost float64
}
```

After each non-streaming provider result, run deterministic verification, then optional model verification. A Fail or configured Indeterminate outcome sets the next floor to `min(current tier + 1, T6)`. Re-enter candidate selection with already-attempted models excluded. A retryable provider failure may choose another candidate at the same or a higher tier; authentication and invalid-request failures do not retry unless a different provider can validly satisfy the canonical request. Respect the concrete-model escalation default and request override. Persist failure/verifier evidence and cost before starting the next attempt. Stop before an attempt that would exceed any budget.

Add a provider-failure test:

```go
func TestRetryableProviderFailureUsesSameOrHigherTier(t *testing.T) {
    deps := providerFailureDeps(domain.T3, provider.ErrorRetryable, domain.T3)
    got, err := deps.Executor.Execute(context.Background(), automaticInput())
    if err != nil || got.Decision.Tier < domain.T3 || len(deps.Attempts) != 2 { t.Fatalf("got=%+v attempts=%v err=%v", got, deps.Attempts, err) }
}
```

- [ ] **Step 5: Test terminal default and best-effort mode**

```go
func TestExhaustedBudgetDefaultsToErrorAndCanReturnBestEffort(t *testing.T) {
    assertTerminalMode(t, executor.TerminalError, true, false)
    assertTerminalMode(t, executor.TerminalBestEffort, false, true)
}
```

Best-effort responses must carry `X-Mindctl-Verification: failed`; default error mode returns a Responses-style error and no candidate body.

- [ ] **Step 6: Run tests and commit**

Run: `devenv shell -- go test ./internal/executor ./internal/storage/... ./internal/config -v`

Expected: PASS.

```bash
git add internal/executor internal/storage internal/config migrations/003_verification.sql config.example.yaml
git commit -m "feat(gateway): verify and escalate failed responses"
```

### Task 4: Feedback endpoint and conversation-floor updates

**Files:**

- Create: `migrations/004_feedback.sql`
- Modify: `internal/storage/storage.go`
- Modify: `internal/storage/sqlite/repository.go`
- Create: `internal/storage/sqlite/feedback_test.go`
- Create: `internal/feedback/service.go`
- Create: `internal/feedback/service_test.go`
- Create: `internal/api/feedback.go`
- Create: `internal/api/feedback_test.go`
- Modify: `internal/app/app.go`

**Interfaces:**

- Consumes: authenticated client identity, response/conversation ownership, and SQLite transactions.
- Produces: `feedback.Service.Record(context.Context, feedback.Input) (feedback.Result, error)` and authenticated `POST /v1/feedback`.

- [ ] **Step 1: Write failing idempotency and ownership tests**

```go
func TestNegativeFeedbackRaisesFloorOnce(t *testing.T) {
    svc, responseID := seededFeedbackService(t, "client-a", domain.T3)
    for range 2 {
        if _, err := svc.Record(context.Background(), Input{ClientID:"client-a", ResponseID:responseID, Outcome:Negative}); err != nil { t.Fatal(err) }
    }
    if got := conversationFloor(t, svc, responseID); got != domain.T4 { t.Fatalf("floor=%s", got) }
}

func TestForeignClientFeedbackLooksNotFound(t *testing.T) {
    svc, responseID := seededFeedbackService(t, "client-a", domain.T3)
    _, err := svc.Record(context.Background(), Input{ClientID:"client-b", ResponseID:responseID, Outcome:Negative})
    if !errors.Is(err, feedback.ErrNotFound) { t.Fatalf("err=%v", err) }
}
```

- [ ] **Step 2: Write the positive-to-negative transition test**

```go
func TestPositiveToNegativeRaisesFloorExactlyOnce(t *testing.T) {
    svc, responseID := seededFeedbackService(t, "client-a", domain.T3)
    _, _ = svc.Record(context.Background(), Input{ClientID:"client-a", ResponseID:responseID, Outcome:Positive})
    _, _ = svc.Record(context.Background(), Input{ClientID:"client-a", ResponseID:responseID, Outcome:Negative})
    _, _ = svc.Record(context.Background(), Input{ClientID:"client-a", ResponseID:responseID, Outcome:Negative})
    if got := conversationFloor(t, svc, responseID); got != domain.T4 { t.Fatalf("floor=%s", got) }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/feedback ./internal/storage/sqlite -run Test -v`

Expected: FAIL because feedback storage and service do not exist.

- [ ] **Step 4: Implement transactional feedback**

Create a unique feedback row per `(client_id, response_id)`, store outcome provenance and encrypted reason, and update the conversation floor only on the first transition into negative. Cap at T6. Do not modify global model priors.

```go
type Input struct { ClientID, ResponseID string; Outcome Outcome; Reason []byte }
type Result struct { ResponseID string; ConversationFloor domain.Tier; Changed bool }
```

- [ ] **Step 5: Implement and test the HTTP handler**

```go
func TestFeedbackHandlerAuthenticatesAndReturnsFloor(t *testing.T) {
    h := authenticatedFeedbackHandler(testFeedbackService())
    rr := serveJSON(h, "/v1/feedback", `{"response_id":"resp_1","outcome":"negative","reason":"incorrect"}`, "client-a")
    if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"conversation_floor":"T4"`) { t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String()) }
}
```

Reject unknown outcomes and unknown JSON fields with 400; unknown/foreign responses return 404; storage unavailability returns 503.

- [ ] **Step 6: Run tests and commit**

Run: `devenv shell -- go test ./internal/feedback ./internal/api ./internal/storage/... -v`

Expected: PASS.

```bash
git add migrations/004_feedback.sql internal/storage internal/feedback internal/api internal/app
git commit -m "feat(feedback): escalate conversations from negative outcomes"
```

### Task 5: Retention worker and encryption failure handling

**Files:**

- Create: `internal/maintenance/retention.go`
- Create: `internal/maintenance/retention_test.go`
- Modify: `internal/storage/sqlite/repository.go`
- Modify: `internal/storage/sqlite/repository_test.go`
- Modify: `internal/contentcrypto/keyring.go`
- Modify: `internal/contentcrypto/keyring_test.go`
- Modify: `internal/app/app.go`

**Interfaces:**

- Consumes: configured retention duration, repository content-deletion method, and keyring.
- Produces: `maintenance.Retention.Run(context.Context) error`, injected clock/ticker tests, and typed decrypt/readiness failures.

- [ ] **Step 1: Write unlimited and finite retention tests**

```go
func TestRetentionZeroNeverDeletes(t *testing.T) {
    repo := &fakeRetentionRepo{}
    worker := Retention{Repository:repo, Retention:0, Now:fixedNow}
    if err := worker.RunOnce(context.Background()); err != nil || repo.DeleteCalls != 0 { t.Fatalf("calls=%d err=%v", repo.DeleteCalls, err) }
}

func TestFiniteRetentionDeletesContentButKeepsTelemetry(t *testing.T) {
    db := seededOldAndNewContent(t)
    worker := Retention{Repository:db, Retention:30*24*time.Hour, Now:fixedNow}
    if err := worker.RunOnce(context.Background()); err != nil { t.Fatal(err) }
    assertOldCiphertextDeleted(t, db)
    assertAggregateAttemptStillPresent(t, db)
    assertNewCiphertextPresent(t, db)
}
```

- [ ] **Step 2: Write missing-old-key typed-error test**

```go
func TestDecryptMissingOldKeyReturnsTypedError(t *testing.T) {
    ring, _ := New("new", map[string][]byte{"new":bytes.Repeat([]byte{2}, 32)})
    _, err := ring.Decrypt(Envelope{Version:1, KeyID:"old", Nonce:make([]byte, 12), Ciphertext:[]byte("opaque")})
    var missing *UnknownKeyError
    if !errors.As(err, &missing) || strings.Contains(err.Error(), "opaque") { t.Fatalf("err=%v", err) }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/maintenance ./internal/contentcrypto ./internal/storage/... -v`

Expected: FAIL with missing maintenance package or missing typed error.

- [ ] **Step 4: Implement retention and lifecycle wiring**

`RunOnce` returns immediately for zero retention. For a nonzero duration, delete only encrypted content fields older than the cutoff in bounded batches and retain IDs, features, decision data, usage, latency, costs, outcomes, and timestamps. `Run` uses an injected ticker and exits on context cancellation. Start it from `app.New` and stop it during `App.Close`.

- [ ] **Step 5: Run tests and commit**

Run: `devenv shell -- go test ./internal/maintenance ./internal/contentcrypto ./internal/storage/... ./internal/app -v`

Expected: PASS.

```bash
git add internal/maintenance internal/contentcrypto internal/storage internal/app
git commit -m "feat(storage): enforce encrypted content retention policy"
```

### Task 6: Content-safe logs, traces, metrics, and readiness

**Files:**

- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/telemetry/telemetry.go`
- Create: `internal/telemetry/http.go`
- Create: `internal/telemetry/telemetry_test.go`
- Create: `internal/telemetry/metrics.go`
- Modify: `internal/api/operations.go`
- Modify: `internal/app/app.go`
- Modify: `internal/executor/service.go`
- Modify: `internal/classifier/jev/client.go`
- Modify: `config.example.yaml`

**Interfaces:**

- Consumes: lifecycle events from API, routing, Jev, providers, verification, storage, and feedback.
- Produces: `telemetry.New(config) (*Telemetry, error)`, purpose-labeled spans/metrics, content-safe structured logging, and readiness latching after storage failures.

- [ ] **Step 1: Add pinned OpenTelemetry dependencies**

Run:

```bash
devenv shell -- go get go.opentelemetry.io/otel@v1.46.0 go.opentelemetry.io/otel/sdk@v1.46.0
devenv shell -- go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.71.0
devenv shell -- go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.46.0
```

- [ ] **Step 2: Write the secret-marker telemetry test**

```go
func TestTelemetryNeverEmitsCapturedContent(t *testing.T) {
    const secret = "PROMPT-SECRET-7b92"
    sinks := newCapturedTelemetry()
    svc := instrumentedFailingService(sinks, errors.New("provider echoed "+secret))
    _, _ = svc.Execute(context.Background(), inputWithPrompt(secret))
    combined := sinks.Logs() + sinks.Spans() + sinks.Metrics()
    if strings.Contains(combined, secret) { t.Fatalf("content leaked: %s", combined) }
}
```

- [ ] **Step 3: Write purpose and readiness tests**

```go
func TestTelemetryLabelsAllInferencePurposes(t *testing.T) {
    sinks := runInstrumentedScenario(t)
    for _, purpose := range []string{"routing", "jev", "provider_inference", "verification", "storage", "feedback"} {
        if !sinks.HasPurpose(purpose) { t.Fatalf("missing purpose %s", purpose) }
    }
}

func TestStorageFailureLatchesReadinessUntilProbeSucceeds(t *testing.T) {
    readiness := telemetry.NewReadiness()
    readiness.Fail(errors.New("write failed"))
    if readiness.Ready() { t.Fatal("expected unavailable") }
    readiness.ProbeSucceeded()
    if !readiness.Ready() { t.Fatal("expected recovered") }
}
```

- [ ] **Step 4: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/telemetry ./internal/executor ./internal/api -run 'TestTelemetry|TestStorage' -v`

Expected: FAIL because telemetry components do not exist.

- [ ] **Step 5: Implement content-safe instrumentation**

Use `slog` with an allowlist of attributes: IDs, tier, model ID, provider, reason code, outcome, counts, durations, token counts, and cost. Never attach request/response bodies, verifier reasons containing model text, authorization headers, or raw error strings from providers.

Create counters/histograms for route count, Jev calls/failures/latency/tokens, provider latency/tokens/errors/cost, verification outcomes/cost, escalation depth, feedback outcomes, and SQLite latency/size. Wrap HTTP with `otelhttp`; create child spans for each purpose. OTLP export is optional by configuration and no-op by default.

- [ ] **Step 6: Integrate readiness failure and recovery**

Any repository write failure marks readiness unavailable immediately. `/readyz` performs a bounded storage probe; one successful probe restores readiness. `/healthz` remains alive. A post-emission streaming write failure sends a terminal error when possible, logs only IDs/codes, and marks readiness unavailable.

- [ ] **Step 7: Run tests and commit**

Run:

```bash
devenv shell -- go test ./internal/telemetry ./internal/executor ./internal/api ./internal/app -v
devenv shell -- go test -race ./internal/telemetry ./internal/executor
```

Expected: PASS.

```bash
git add go.mod go.sum internal/telemetry internal/api internal/app internal/executor internal/classifier config.example.yaml
git commit -m "feat(observability): trace router outcomes without content"
```

### Task 7: End-to-end fixtures, race suite, and operator documentation

**Files:**

- Create: `internal/integration/gateway_test.go`
- Create: `internal/integration/stream_test.go`
- Create: `internal/integration/feedback_test.go`
- Create: `docs/router.md`
- Modify: `README.md`
- Modify: `config.example.yaml`

**Interfaces:**

- Consumes: the fully assembled app and local fake Jev/OpenAI/Anthropic/Gemini servers.
- Produces: executable end-to-end proof and operator documentation for local startup, security, retention, and smoke testing.

- [ ] **Step 1: Write the full non-streaming escalation test**

```go
func TestGatewayRoutesVerifiesEscalatesAndPersists(t *testing.T) {
    env := newGatewayFixture(t,
        jevChooses(domain.T3),
        providerReturns("cheap", `{"name":3}`),
        providerReturns("strong", `{"name":"Ada"}`),
    )
    response := env.PostResponses(schemaRequestBody(), "client-a")
    if response.Status != 200 || response.Model != "strong" || response.Header("X-Mindctl-Attempts") != "2" { t.Fatalf("response=%+v", response) }
    assertAttemptTiers(t, env.DB, response.ID, domain.T3, domain.T4)
    assertEncryptedContentOnly(t, env.DBPath, "Ada")
}
```

- [ ] **Step 2: Write the streaming boundary test**

```go
func TestGatewayStreamingRetriesOnlyBeforeEmission(t *testing.T) {
    assertPreEmissionRetrySucceeds(t)
    assertPostEmissionFailureIsTerminalAndRaisesFloor(t)
}
```

- [ ] **Step 3: Write the feedback conversation test**

```go
func TestNegativeFeedbackRaisesNextTurnFloorWithoutChangingGlobalPrior(t *testing.T) {
    env := newGatewayFixture(t, jevChooses(domain.T2), successfulProvider())
    first := env.PostResponses(textRequestBody(), "client-a")
    env.PostFeedback(first.ID, "negative", "client-a")
    next := env.PostResponses(previousResponseBody(first.ID), "client-a")
    if next.Tier < domain.T3 { t.Fatalf("tier=%s", next.Tier) }
    assertPriorUnchanged(t, env.DB, first.Model)
}
```

- [ ] **Step 4: Run the complete deterministic suite**

Run:

```bash
devenv shell -- go test ./...
devenv shell -- go test -race ./...
devenv shell -- go vet ./...
```

Expected: PASS with no live credentials or paid API calls.

- [ ] **Step 5: Document operation and privacy boundaries**

`docs/router.md` must include:

- `devenv shell -- go run ./cmd/mindctl -config config.example.yaml`
- Required environment variables and 32-byte base64 encryption key generation
- `mindctl-auto`, concrete-model behavior, tier/escalation headers, and response headers
- `/v1/responses`, `/v1/feedback`, `/healthz`, and `/readyz` examples
- Streaming retry limitations
- Jev and verifier external data disclosures
- Unlimited default retention, disk-growth warning, and finite-retention configuration
- Key rotation procedure retaining old decryption keys
- Opt-in live smoke-test environment variables

Update the root README with a concise gateway link rather than duplicating the guide.

- [ ] **Step 6: Run documentation formatting and whitespace checks**

Run:

```bash
devenv shell -- pnpm exec prettier --check README.md docs/router.md docs/superpowers/specs/2026-09-21-jev-model-router-design.md docs/superpowers/plans/*.md
git diff --check
```

Expected: PASS.

- [ ] **Step 7: Commit the completed initial implementation**

```bash
git add internal/integration docs/router.md README.md config.example.yaml
git commit -m "test(router): prove the complete routing gateway flow"
```
