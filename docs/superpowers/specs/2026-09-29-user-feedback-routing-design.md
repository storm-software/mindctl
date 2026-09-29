# User Feedback Routing Design

**Date:** 2026-09-29
**Status:** Approved

## Purpose

Mindctl will accept a thumbs-up or thumbs-down rating for a completed gateway
response, persist the caller's current rating, and use that evidence in future
automatic routing.

Feedback has two routing effects:

1. A thumbs-down immediately raises the minimum tier for the response's
   conversation and bound routing session.
2. Once enough ratings exist for a provider, model, and task type, their
   aggregate adjusts that model's configured success prior for later automatic
   decisions across conversations.

The design preserves the deterministic router boundary. Storage supplies an
immutable feedback evidence snapshot to each decision; the routing policy
performs the adjustment without doing I/O.

## Goals

- Accept a strict boolean rating associated with a gateway response ID.
- Authorize feedback with the same client identity used for inference.
- Persist one revisable current rating per response in SQLite.
- Apply each response's negative conversation escalation at most once.
- Use sufficiently large feedback samples in expected-cost model selection.
- Preserve enough decision metadata to explain and reproduce the effective
  success prior.
- Keep concurrent submissions transactional and idempotent.

## Non-goals

- Free-form feedback comments or reasons.
- Lowering a conversation floor after positive feedback.
- Changing the model chosen for an already completed response.
- Applying learned priors to explicit concrete-model requests.
- Cross-installation or remote feedback synchronization.
- A separate background training service or learned model.

## HTTP contract

The authenticated endpoint is:

```http
POST /v1/feedback
Authorization: Bearer <gateway token>
Content-Type: application/json

{
  "response_id": "resp_...",
  "rating": false
}
```

`rating: true` means thumbs up and `rating: false` means thumbs down. The body
decoder requires both fields, rejects unknown fields and trailing JSON, and
uses a nullable boolean while decoding so an omitted rating cannot be confused
with thumbs down.

A successful submission returns the persisted state:

```json
{
  "response_id": "resp_...",
  "rating": false,
  "conversation_floor": "T4"
}
```

The endpoint returns:

- `400` for malformed JSON, missing or empty IDs, missing fields, or unknown
  fields.
- `401` for missing or invalid gateway authentication.
- `404` for an unknown response or a response owned by another client. These
  cases deliberately have the same response.
- `409` when the response exists but has not completed successfully.
- `503` when durable feedback state cannot be read or written.

The endpoint uses the existing Responses-style error envelope and content-safe
messages.

## Persistence model

Migration `008_feedback.sql` adds one feedback row per response. The response
ID is the primary key and references `responses(id)` with cascade deletion. A
row contains:

- the current boolean rating;
- provider and model IDs for the successful attempt being rated;
- the routed task type, with `unknown` for decisions created before this
  feature or classifier fallback decisions;
- the served tier;
- a sticky `negative_applied` flag;
- creation and update timestamps.

Provider, model, task type, and served tier are attribution snapshots. They do
not change if catalog configuration changes later. An index over provider,
model, task type, and rating supports aggregate reads.

The provider attempt's persisted `router.Decision` gains its task type so the
feedback transaction can attribute a completed response without reclassifying
content. Old decision JSON decodes to the `unknown` task type.

### Transaction semantics

One SQLite write transaction performs all feedback work:

1. Resolve the response through its conversation and require the authenticated
   client ID to own it.
2. Require the response and its final provider attempt to be successful.
3. Read the successful attempt with the greatest attempt sequence and its
   persisted decision metadata.
4. Insert the first rating or update the existing rating and timestamp.
5. For a thumbs-down whose `negative_applied` flag is still false, set the
   target floor to one tier above the served tier, capped at T6, and update the
   conversation floor to the greater of its current floor and that target.
6. Propagate the same upward-only floor to the bound routing session through
   the existing session synchronization path.
7. Set `negative_applied` permanently for that response.

This means repeated thumbs-down submissions do not keep escalating. Changing a
rating from down to up changes the global aggregate but does not lower the
floor. Changing it back to down also does not reapply the response's immediate
escalation.

Concurrent submissions for one response serialize through SQLite and produce
one final row. Aggregate counts use current rows rather than an append-only
event count, so retries and rating revisions do not inflate the sample size.

## Global routing evidence

Feedback evidence is grouped by:

```text
(provider, model_id, task_type)
```

Each group contains its current rating count and thumbs-up count. A group does
not affect routing until it contains at least 10 ratings.

For an eligible model, the policy starts with the configured task-specific
success prior when present, otherwise the configured default success prior. It
then computes:

```text
effective_success_prior =
    (configured_success_prior * 20 + thumbs_up_count)
    / (20 + rating_count)
```

The fixed weight of 20 treats configuration as twenty prior observations. This
prevents a small or polarized feedback sample from overwhelming the reviewed
configuration while allowing accumulated evidence to change selection.

Only evidence for the decision's exact provider, model, and task type is used.
There is no fallback from a sparse task bucket to a model-wide bucket. A
classifier fallback decision uses the `unknown` bucket. This avoids allowing
feedback from one workload type to alter an unrelated workload.

The effective prior replaces the configured prior in the existing failure
probability and expected-total-cost calculation. It also participates in the
configured minimum-success-probability check. Capabilities, tier floors,
provider availability, caller bounds, and conversation pins remain hard
constraints.

Learned priors apply only when Mindctl is automatically selecting a model. An
explicit concrete-model request keeps its configured prior and exact-model
semantics. A compatible conversation pin remains authoritative; aggregate
feedback affects decisions that actually compare candidates, including a
replacement when the pin is unavailable or incompatible.

## Routing data flow

The application wires one feedback service over SQLite into both the HTTP
handler and executor:

```text
POST /v1/feedback
        |
        v
feedback service ---- transactional upsert + floor ratchet ----> SQLite
        |
        `---- current aggregate evidence ------------------------'

automatic request -> classifier task type -> evidence snapshot -> pure policy
                                                              -> provider
```

The executor requests an aggregate snapshot after it knows the routed task
type and before it asks the policy to compare candidates. Streaming and
non-streaming execution share the same evidence-loading helper so their
selection behavior cannot drift.

A feedback evidence read error stops the inference request and surfaces the
existing persistence-unavailable response. Silently falling back to configured
priors would make routing depend on whether an internal read happened to fail.

## Replay and observability

`router.DecisionInput` receives a copied, immutable evidence slice. A slice is
used so the complete input remains directly JSON-serializable for replay.
Candidate scores record:

- the configured success prior;
- the feedback rating count;
- the feedback thumbs-up count;
- whether the minimum-sample threshold was met;
- the effective success prior used for scoring.

The existing `SuccessProbability` remains the effective value. Persisted
provider-attempt decision JSON therefore explains the result even after more
feedback arrives. Any replay snapshot that carries `DecisionInput` also retains
the exact evidence used for that historical decision.

Debug traces may emit provider ID, model ID, task type, sample count, and the
configured/effective priors. They must not emit client tokens or captured
request and response content.

## Package boundaries

- `internal/feedback` owns input validation, result types, orchestration, and
  the narrow repository/prior-source interfaces.
- `internal/storage/sqlite` implements transactional rating writes and aggregate
  reads.
- `internal/api` owns strict JSON decoding and Responses-style HTTP errors.
- `internal/executor` loads immutable evidence for automatic decisions.
- `internal/router` owns the pure Bayesian calculation and candidate metadata.
- `internal/app` wires the authenticated endpoint and shared feedback service.

These boundaries keep HTTP, SQLite, provider execution, and pure scoring
independently testable.

## Verification

Focused tests cover:

- authentication and strict boolean request decoding;
- unknown, foreign, pending, and completed response behavior;
- first insert, identical retry, up-to-down, down-to-up, and down-to-up-to-down
  transitions;
- one-time escalation, T6 capping, upward-only conversation floors, and bound
  routing-session propagation;
- persistence across database close and reopen;
- concurrent submissions producing one row and at most one escalation;
- aggregation isolation by provider, model, and task type;
- no learned adjustment for 9 ratings and activation at 10;
- exact Bayesian calculations and configured-prior preservation;
- explicit requests ignoring learned priors;
- automatic candidate selection changing when accumulated feedback changes
  expected total cost;
- identical evidence behavior in streaming and non-streaming routing;
- persistence failures returning `503` without content leakage.

Final verification runs:

```text
devenv shell -- go test ./internal/feedback ./internal/storage/sqlite ./internal/api ./internal/router ./internal/executor ./internal/app
devenv shell -- go test ./...
devenv shell -- go test -race ./...
devenv shell -- go vet ./...
```

Focused tests and full Go verification establish the local API, persistence,
and routing behavior. They do not establish behavior in a separately deployed
gateway until that binary is built and exercised against its persisted state.
