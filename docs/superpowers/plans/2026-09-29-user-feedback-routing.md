# User Feedback Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist boolean response ratings, escalate negatively rated conversations once, and blend mature provider/model/task feedback into future automatic routing.

**Architecture:** An authenticated feedback handler calls a service backed by one transactional SQLite repository. The executor loads an immutable aggregate evidence snapshot for automatic routing, and the pure router combines that evidence with configured success priors using the approved Bayesian formula.

**Tech Stack:** Go 1.26.7, `net/http`, SQLite through `database/sql`, existing Mindctl router/executor/storage packages, table-driven Go tests.

**Spec:** `docs/superpowers/specs/2026-09-29-user-feedback-routing-design.md`

## Global Constraints

- The request body is exactly `response_id` plus required boolean `rating`; unknown fields and trailing JSON are invalid.
- `rating: true` is thumbs up and `rating: false` is thumbs down.
- Unknown and foreign-client responses are indistinguishable `404` responses; incomplete responses return `409`; storage failures return `503`.
- Each response can raise its conversation/session floor at most once, to `max(current_floor, min(served_tier + 1, T6))`.
- Global evidence is keyed by exact `(provider, model_id, task_type)` and activates at 10 current ratings.
- The effective prior is `(configured_prior * 20 + thumbs_up_count) / (20 + rating_count)`.
- Learned priors affect automatic candidate comparison only; explicit models and compatible conversation pins keep their current semantics.
- Routing policy remains pure and deterministic; all SQLite I/O occurs before `router.Policy.Decide`.
- Persisted decision metadata must contain the exact evidence and effective prior used for historical explanation.
- No request/response content, feedback comments, tokens, or credentials are stored or logged by this feature.
- Run repository commands through `devenv shell --`.
- Preserve unrelated existing changes in `config.example.yaml` and `internal/config/load_test.go`; stage only files owned by each task.

## Review Focus

- A missing `rating` must not decode as thumbs down; Task 3 adds a strict decoder test for omitted `rating`, explicit `false`, unknown fields, and trailing JSON.
- Decision JSON written before this feature has no task type; Task 2 proves it records feedback in the `unknown` bucket.
- Concurrent duplicate thumbs-down submissions must not escalate twice; Task 2 races two writes and checks both the final row and floor.
- Rating revisions must change aggregate counts without reapplying escalation; Task 2 covers down/up/down and exact counts after each revision.
- Learned evidence must not change explicit or compatible pinned requests, and an evidence read failure must stop before provider I/O; Task 4 covers all three paths.

---

### Task 1: Pure feedback-aware candidate scoring

**Files:**

- Modify: `internal/router/policy.go`
- Modify: `internal/router/cost.go`
- Create: `internal/router/feedback_test.go`
- Modify: `internal/router/policy_test.go`

**Interfaces:**

- Consumes: configured model success priors and the existing `domain.TaskType` selected for a decision.
- Produces:
  - `router.FeedbackEvidence{Provider, ModelID string; TaskType domain.TaskType; ThumbsUpCount, RatingCount int64}`
  - `router.DecisionInput.FeedbackEvidence []FeedbackEvidence`
  - `router.Decision.TaskType domain.TaskType`
  - candidate audit fields `ConfiguredSuccessProbability`, `FeedbackThumbsUpCount`, `FeedbackRatingCount`, and `FeedbackApplied`
  - `router.EffectiveSuccessProbability(domain.Model, domain.TaskType, FeedbackEvidence) (configured float64, effective float64, applied bool, err error)`

- [ ] **Step 1: Write failing threshold and Bayesian formula tests**

Create table-driven tests named `TestEffectiveSuccessProbability` with these assertions:

```go
tests := []struct {
    name     string
    evidence FeedbackEvidence
    want     float64
    applied  bool
}{
    {"nine ratings keep configured prior", FeedbackEvidence{ThumbsUpCount: 0, RatingCount: 9}, 0.8, false},
    {"ten ratings activate blend", FeedbackEvidence{ThumbsUpCount: 5, RatingCount: 10}, 0.7, true},
    {"twenty positive ratings", FeedbackEvidence{ThumbsUpCount: 20, RatingCount: 20}, 0.9, true},
}
```

Use a model whose configured prior is `0.8`. Add invalid evidence cases for negative counts and thumbs-up count greater than rating count; both must return an error.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `devenv shell -- go test ./internal/router -run TestEffectiveSuccessProbability -v`

Expected: FAIL because the feedback types and calculation do not exist.

- [ ] **Step 3: Implement the pure evidence types and calculation**

In `internal/router/policy.go`, add the exact public types from the Interfaces block. In `internal/router/cost.go`, implement:

```go
func EffectiveSuccessProbability(
    model domain.Model,
    task domain.TaskType,
    evidence FeedbackEvidence,
) (configured float64, effective float64, applied bool, err error)
```

Use `10` as the minimum sample count and `20.0` as the configured-prior weight. Select the task-specific configured prior before applying evidence. Validate configured and evidence values before calculating.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run: `devenv shell -- go test ./internal/router -run TestEffectiveSuccessProbability -v`

Expected: PASS.

- [ ] **Step 5: Write failing policy integration and isolation tests**

Add tests that assert:

- evidence changes candidate order when its effective failure cost makes the formerly cheaper candidate more expensive;
- evidence for the wrong provider, model, or task type does not affect a candidate;
- `Decision.TaskType` equals `DecisionInput.TaskType`;
- every `CandidateScore` records configured/effective probabilities, evidence counts, and whether feedback was applied;
- nil evidence preserves all existing scoring behavior.

- [ ] **Step 6: Run the integration tests and verify RED**

Run: `devenv shell -- go test ./internal/router -run 'TestPolicy.*Feedback|TestFeedbackEvidenceIsolation|TestDecisionRecordsTaskType' -v`

Expected: FAIL because policy scoring does not consume evidence or persist its audit fields.

- [ ] **Step 7: Thread evidence through policy scoring**

Set `Decision.TaskType` from the input. Add a private lookup with this exact
signature:

```go
func findFeedbackEvidence(
    evidence []FeedbackEvidence,
    provider string,
    modelID string,
    task domain.TaskType,
) FeedbackEvidence
```

Return only an entry whose provider, model ID, and task type all match; otherwise
return zero evidence. Pass that evidence to the scoring calculation. Set
`SuccessProbability` to the effective value and derive failure probability and
expected total cost from it. Populate the new audit fields even when feedback
is below threshold.

- [ ] **Step 8: Run router tests**

Run: `devenv shell -- go test ./internal/router -v`

Expected: PASS, including existing policy, fuzz seed, eligibility, and horizon tests.

- [ ] **Step 9: Commit the pure router change**

```bash
git add internal/router/policy.go internal/router/cost.go internal/router/feedback_test.go internal/router/policy_test.go
git commit -m "feat(router): score models with aggregate feedback"
```

---

### Task 2: Transactional feedback persistence and service

**Files:**

- Create: `migrations/008_feedback.sql`
- Create: `internal/feedback/service.go`
- Create: `internal/feedback/service_test.go`
- Modify: `internal/storage/sqlite/repository.go`
- Create: `internal/storage/sqlite/feedback_test.go`
- Modify: `internal/storage/sqlite/migrate.go` only if migration-test support requires it

**Interfaces:**

- Consumes: `router.Decision.TaskType`, completed `responses`/`provider_attempts`, conversation/session floor persistence, and Task 1 feedback evidence types.
- Produces:

```go
var ErrNotFound = errors.New("feedback: response not found")
var ErrNotCompleted = errors.New("feedback: response is not completed")

type Input struct {
    ClientID   string
    ResponseID string
    Rating     bool
}

type Result struct {
    ResponseID        string
    Rating            bool
    ConversationFloor domain.Tier
}

type Repository interface {
    RecordFeedback(context.Context, Input) (Result, error)
    FeedbackEvidence(context.Context, domain.TaskType) ([]router.FeedbackEvidence, error)
}

type Service struct { /* private repository */ }
func New(repository Repository) *Service
func (s *Service) Record(context.Context, Input) (Result, error)
func (s *Service) Evidence(context.Context, domain.TaskType) ([]router.FeedbackEvidence, error)
```

- [ ] **Step 1: Write failing migration and first-rating tests**

In `feedback_test.go`, seed completed responses through the existing conversation repository helpers. Assert that:

- a thumbs up inserts one row with the successful attempt's provider, model, task type, and tier;
- a thumbs down sets `negative_applied=1` and raises the floor to one tier above the served tier;
- T6 stays T6;
- closing and reopening the database preserves the row and floor.

- [ ] **Step 2: Run persistence tests and verify RED**

Run: `devenv shell -- go test ./internal/storage/sqlite -run 'TestFeedback|TestNegativeFeedback' -v`

Expected: FAIL because migration 008 and repository methods do not exist.

- [ ] **Step 3: Add the schema and minimal feedback package contracts**

Create `response_feedback` with:

```text
response_id TEXT PRIMARY KEY REFERENCES responses(id) ON DELETE CASCADE
rating INTEGER NOT NULL CHECK (rating IN (0, 1))
provider TEXT NOT NULL
model_id TEXT NOT NULL
task_type TEXT NOT NULL
served_tier INTEGER NOT NULL CHECK (served_tier BETWEEN 0 AND 6)
negative_applied INTEGER NOT NULL CHECK (negative_applied IN (0, 1))
created_at INTEGER NOT NULL
updated_at INTEGER NOT NULL
```

Add an aggregate index on `(provider, model_id, task_type, rating)`. Create
`internal/feedback/service.go` with the exact public contracts above; its
methods validate required IDs, delegate once, and return copied evidence
slices.

- [ ] **Step 4: Implement `RecordFeedback` as one write transaction**

Add:

```go
func (db *DB) RecordFeedback(ctx context.Context, input feedback.Input) (feedback.Result, error)
```

Resolve ownership by joining responses to conversations with both client ID and response ID. Map absent/foreign rows to `feedback.ErrNotFound`. Require response status `completed` and the greatest-sequence successful attempt; otherwise return `feedback.ErrNotCompleted`. Decode the attempt decision, default an empty task type to `domain.TaskUnknown`, and preserve the first attribution snapshot on rating revisions.

For the first negative application, calculate `min(servedTier+1, T6)`, raise the conversation floor with `max`, call the existing session synchronization helper inside the same transaction, and make `negative_applied` sticky.

- [ ] **Step 5: Run first-rating tests and verify GREEN**

Run: `devenv shell -- go test ./internal/storage/sqlite -run 'TestFeedback|TestNegativeFeedback' -v`

Expected: PASS.

- [ ] **Step 6: Write failing ownership, revision, legacy, and concurrency tests**

Add exact cases for:

- unknown and foreign client IDs both returning `feedback.ErrNotFound`;
- pending response returning `feedback.ErrNotCompleted`;
- identical retry leaving one row and one escalation;
- down/up/down changing the stored rating each time while applying only one escalation;
- old decision JSON without `TaskType` storing `task_type='unknown'`;
- two concurrent thumbs-down writes producing one row and one floor increase;
- a bound routing session receiving the same upward-only floor.

- [ ] **Step 7: Implement aggregate evidence reads**

Add:

```go
func (db *DB) FeedbackEvidence(
    ctx context.Context,
    task domain.TaskType,
) ([]router.FeedbackEvidence, error)
```

Use one grouped query filtered to the exact task type and ordered by provider
then model ID. Count current rows and sum `rating=1`; never read raw
transcript/result blobs. Wrap database failures with the existing content-safe
`storage.Error` path.

- [ ] **Step 8: Write and run aggregate/service tests**

Test exact provider/model/task isolation, rating revision count changes, empty results, validation, delegated errors, deterministic ordering, and defensive slice copying.

Run: `devenv shell -- go test ./internal/feedback ./internal/storage/sqlite -v`

Expected: PASS, including concurrency and reopen cases.

- [ ] **Step 9: Commit feedback persistence**

```bash
git add migrations/008_feedback.sql internal/feedback internal/storage/sqlite/repository.go internal/storage/sqlite/feedback_test.go internal/storage/sqlite/migrate.go
git commit -m "feat(feedback): persist response ratings"
```

Omit `internal/storage/sqlite/migrate.go` from staging when it did not require a change.

---

### Task 3: Authenticated feedback HTTP endpoint

**Files:**

- Create: `internal/api/feedback.go`
- Create: `internal/api/feedback_test.go`
- Modify: `internal/api/errors.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`

**Interfaces:**

- Consumes: `feedback.Service.Record`, existing `api.ClientID`, `api.Authenticate`, client-auth maximum body bytes, and Responses-style errors.
- Produces:

```go
type FeedbackRecorder interface {
    Record(context.Context, feedback.Input) (feedback.Result, error)
}

func NewFeedbackHandler(recorder FeedbackRecorder, maxBodyBytes int64) http.Handler
```

The success JSON fields are `response_id`, `rating`, and `conversation_floor`.

- [ ] **Step 1: Write failing strict-decoder table tests**

Cover valid `true`, valid `false`, missing `rating`, missing/empty `response_id`, a string rating, unknown fields, trailing JSON, oversized body, wrong method, and missing authenticated client context. Explicitly assert that `{ "response_id": "resp_1", "rating": false }` reaches the recorder with `Rating == false`.

- [ ] **Step 2: Run API tests and verify RED**

Run: `devenv shell -- go test ./internal/api -run TestFeedback -v`

Expected: FAIL because the feedback handler does not exist.

- [ ] **Step 3: Implement the strict handler**

Decode through `http.MaxBytesReader`, `json.Decoder.DisallowUnknownFields`, a request struct with `Rating *bool`, and a second decode requiring `io.EOF`. Accept only `POST`. Read the authenticated client ID from context and call the recorder once.

Encode a `200` response with the exact success fields. Do not log or echo request bodies.

- [ ] **Step 4: Map feedback errors**

Extend `errorDetail` so:

- `feedback.ErrNotFound` maps to `404`, code `response_not_found`;
- `feedback.ErrNotCompleted` maps to `409`, code `response_not_completed`;
- validation errors map to the existing `400` envelope;
- repository `storage.Error` continues mapping to `503`.

- [ ] **Step 5: Run API tests and verify GREEN**

Run: `devenv shell -- go test ./internal/api -run TestFeedback -v`

Expected: PASS.

- [ ] **Step 6: Write failing application wiring tests**

Add tests proving `/v1/feedback` is mounted, requires the configured gateway token, uses the same stable client identity as `/v1/responses`, and persists a rating. An authenticated `GET` must return `405`; an unauthenticated `GET` must return `401`.

- [ ] **Step 7: Wire the shared service and authenticated endpoint**

In `internal/app/app.go`, construct one `feedback.Service` over `a.store`. Mount:

```go
mux.Handle(
    "/v1/feedback",
    api.Authenticate(
        api.NewFeedbackHandler(feedbackService, maxBodyBytes),
        cfg.ClientAuth.HeaderName(),
        appTokens{{ID: "configured-client", Value: clientToken}},
    ),
)
```

Keep the service available for Task 4 executor wiring.

- [ ] **Step 8: Run API and app tests**

Run: `devenv shell -- go test ./internal/api ./internal/app -v`

Expected: PASS.

- [ ] **Step 9: Commit the endpoint**

```bash
git add internal/api/feedback.go internal/api/feedback_test.go internal/api/errors.go internal/app/app.go internal/app/app_test.go
git commit -m "feat(api): accept boolean response feedback"
```

---

### Task 4: Feed persisted evidence into automatic execution

**Files:**

- Create: `internal/executor/feedback.go`
- Create: `internal/executor/feedback_test.go`
- Modify: `internal/executor/service.go`
- Modify: `internal/executor/stream.go`
- Modify: `internal/executor/service_test.go`
- Modify: `internal/executor/stream_test.go`
- Modify: `internal/app/app.go`

**Interfaces:**

- Consumes: `feedback.Service.Evidence` and Task 1's JSON-safe `router.DecisionInput.FeedbackEvidence` slice.
- Produces:

```go
type FeedbackSource interface {
    Evidence(context.Context, domain.TaskType) ([]router.FeedbackEvidence, error)
}

func NewWithFeedback(
    classifier classifier.Classifier,
    policy *router.Policy,
    providers *provider.Registry,
    conversations conversation.Service,
    feedbackSource FeedbackSource,
    compressor headroom.Compressor,
    logger *slog.Logger,
) *Service
```

Existing constructors delegate to `NewWithFeedback` with a nil source so focused legacy tests retain their existing setup.

- [ ] **Step 1: Write failing automatic-routing evidence tests**

Create a fake `FeedbackSource` and two configured candidates whose order changes under the approved evidence formula. Assert that a fresh automatic non-streaming request selects the feedback-adjusted candidate and that its persisted decision contains the evidence counts and effective prior.

- [ ] **Step 2: Write failing exclusion and error tests**

Assert that:

- an explicit model request never calls the feedback source;
- a compatible pinned fast path never calls the feedback source;
- a source error returns before `BeginAttempt` or provider execution;
- classifier failure requests only the `domain.TaskUnknown` bucket.

- [ ] **Step 3: Run executor tests and verify RED**

Run: `devenv shell -- go test ./internal/executor -run 'Test.*Feedback' -v`

Expected: FAIL because the executor has no feedback source.

- [ ] **Step 4: Add the shared evidence-loading helper and constructor**

In `internal/executor/feedback.go`, implement a helper that:

- returns nil without I/O for explicit requests, compatible pin reuse, or a nil source;
- normalizes an empty task type to `domain.TaskUnknown`;
- fetches once and defensively copies the evidence slice into `DecisionInput`;
- returns source errors unchanged so `storage.Error` reaches the API mapper.

Have `New`, `NewWithLogger`, and `NewWithCompressor` delegate through `NewWithFeedback` without changing their public signatures.

- [ ] **Step 5: Integrate non-streaming automatic selection**

Load evidence after classification chooses the task type, or after fallback selects `TaskUnknown`, and immediately before candidate comparison. Do not load evidence in the existing compatible-pin fast return.

- [ ] **Step 6: Run non-streaming tests and verify GREEN**

Run: `devenv shell -- go test ./internal/executor -run 'TestExecute.*Feedback|TestFeedbackEvidence' -v`

Expected: PASS.

- [ ] **Step 7: Write and implement matching streaming tests**

Add a streaming test where evidence changes the first selected provider/model and a failure test proving source failure occurs before writer start or provider I/O. Reuse the same helper from `stream.go`; do not duplicate evidence rules.

Run: `devenv shell -- go test ./internal/executor -run 'TestStream.*Feedback' -v`

Expected: PASS.

- [ ] **Step 8: Wire feedback evidence in the application**

Replace the application composition call with `executor.NewWithFeedback`, passing the same `feedback.Service` instance mounted by Task 3. Add an application-level test that submits ten ratings, begins a fresh automatic request, and observes the changed routed model through the response headers.

- [ ] **Step 9: Run executor and app tests**

Run: `devenv shell -- go test ./internal/executor ./internal/app -v`

Expected: PASS.

- [ ] **Step 10: Commit executor integration**

```bash
git add internal/executor/feedback.go internal/executor/feedback_test.go internal/executor/service.go internal/executor/stream.go internal/executor/service_test.go internal/executor/stream_test.go internal/app/app.go internal/app/app_test.go
git commit -m "feat(router): learn from persisted feedback"
```

---

### Task 5: User documentation and full verification

**Files:**

- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-09-29-user-feedback-routing-design.md` only if implementation review reveals a factual mismatch
- Test: all Go packages

**Interfaces:**

- Consumes: the completed endpoint and routing behavior from Tasks 1-4.
- Produces: public usage documentation and final repository verification evidence.

- [ ] **Step 1: Document the feedback request and behavior**

Add a concise README section with one authenticated `curl` example for each boolean rating, the one-time conversation escalation rule, the 10-rating threshold, and the Bayesian formula. State that learned priors apply to automatic routing and ratings can be revised by reposting the response ID.

- [ ] **Step 2: Run focused package verification**

Run:

```text
devenv shell -- go test ./internal/feedback ./internal/storage/sqlite ./internal/api ./internal/router ./internal/executor ./internal/app
```

Expected: PASS.

- [ ] **Step 3: Run full Go verification**

Run:

```text
devenv shell -- go test ./...
devenv shell -- go test -race ./...
devenv shell -- go vet ./...
```

Expected: all commands exit zero. If an environmental restriction blocks a command, record the exact restriction separately from code failures and rerun with the repository's permitted temporary cache path where applicable.

- [ ] **Step 4: Inspect schema and routing proof surfaces**

Confirm with focused assertions or read-only inspection that:

- raw SQLite rows contain no request/response content or tokens;
- provider-attempt decision JSON includes task type, counts, configured prior, and effective prior;
- repeated feedback changes one row rather than appending events;
- automatic response headers identify the model selected under learned evidence.

- [ ] **Step 5: Check the final diff**

Run:

```text
git diff --check
git status --short
```

Expected: no whitespace errors; only intended feedback files plus the user's pre-existing unrelated edits are present.

- [ ] **Step 6: Commit documentation**

```bash
git add README.md docs/superpowers/specs/2026-09-29-user-feedback-routing-design.md
git commit -m "docs: explain feedback-driven routing"
```

Omit the spec from staging when implementation required no factual correction.

- [ ] **Step 7: Request final code review**

Use `superpowers:requesting-code-review` for the complete branch. Address any correctness issue with a focused RED/GREEN cycle, rerun the affected verification, and then rerun `git diff --check`.
