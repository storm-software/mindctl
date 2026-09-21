# Jev Model Router Gateway Design

**Date:** 2026-09-21

**Status:** Approved design

## Purpose

Mindctl will provide a Go gateway that accepts OpenAI Responses API requests and
routes them to the least expensive model that has a sufficiently high
probability of completing the request successfully. The gateway will combine
deterministic request features, TypeSafe Jev judgments, configured model
capabilities and prices, historical success priors, verification, feedback, and
escalation.

The optimization target is expected total cost rather than lowest first-attempt
price:

```text
expected_total_cost =
    inference_cost
    + probability_of_failure * expected_escalation_cost
    + configured_latency_penalty
```

Jev supplies narrow, structured signals. Deterministic Go code owns routing
policy, provider eligibility, model selection, and escalation.

## Goals

- Expose `POST /v1/responses` with an OpenAI-compatible request and response
  surface.
- Execute requests through native OpenAI, Anthropic, and Gemini adapters.
- Route automatic requests through seven provider-independent capability tiers.
- Keep explicit model requests under caller control.
- Pin a model to a conversation and permit only upward automatic movement.
- Verify non-streaming responses and escalate failed attempts within configured
  budgets.
- Support streaming while acknowledging that output already sent to a client
  cannot be transparently replaced.
- Record the complete learning tuple: prompt features, selected model, attempts,
  outcome, tokens, latency, and cost.
- Persist raw prompts and answers by default using application-level encryption.
- Keep global model-performance updates offline until minimum sample and
  confidence policies are designed and evaluated.

## Non-goals

- Online learning from individual feedback events.
- Semantic retrieval over historical prompts in the first release.
- A distributed router and provider-worker deployment.
- Perfect emulation of provider-hosted tools that have no faithful equivalent.
- Transparent quality retry after streaming output has reached the client.
- Modifying or vendoring code from `/home/development/repos/model-router`.

## Capability tiers

The policy uses these ordered capability floors:

| Tier | Meaning                                          |
| ---- | ------------------------------------------------ |
| T0   | Trivial work, extraction, and classification     |
| T1   | Normal generation                                |
| T2   | Very easy reasoning and very simple coding       |
| T3   | Easy reasoning and simple coding                 |
| T4   | Average reasoning and normal coding              |
| T5   | Difficult reasoning and complex coding           |
| T6   | Very difficult reasoning and very complex coding |

Tiers are not vendor aliases. Multiple models may occupy one tier, and a model
above the required floor may be selected when its reliability produces a lower
expected total cost. Configuration maps concrete upstream models to tiers and
describes their prices, capabilities, limits, latency priors, and success priors.

## API contract

### Inference

`POST /v1/responses` is the only generation endpoint in the initial release.
Health and readiness endpoints are operational interfaces rather than generation
APIs.

- `model: "mindctl-auto"` enables automatic routing.
- A concrete configured model ID is an explicit caller selection. It bypasses
  automatic model choice, remains fully observable, and is not replaced unless
  the caller explicitly enables escalation.
- Request-scoped controls use namespaced HTTP headers so the JSON body remains a
  valid Responses API request:
  - `X-Mindctl-Min-Tier: T0..T6`
  - `X-Mindctl-Max-Tier: T0..T6`
  - `X-Mindctl-Allow-Escalation: true|false`
- Escalation defaults to enabled for `mindctl-auto` and disabled for a concrete
  model. The header explicitly overrides that default.
- The gateway issues its own response IDs. `previous_response_id` restores the
  canonical conversation transcript, selected model, and escalation floor.
- Client authentication uses a gateway bearer token. Provider and Jev
  credentials remain server-side and are never accepted in an inference body.

Successful responses identify the actual selected model in the standard
`model` field. `X-Mindctl-Decision-ID`, `X-Mindctl-Tier`, and
`X-Mindctl-Attempts` response headers expose content-free routing metadata for
diagnostics while the complete explanation remains in durable telemetry.

The gateway supports the following portable subset across native adapters:

- Text and image inputs
- Instructions and message history
- Custom function tools and matching tool results
- JSON Schema structured outputs
- Non-streaming responses
- SSE text and function-call events
- Normalized usage and completion status

Provider-hosted tools or request features without a faithful native mapping are
capability constraints. Automatic routing excludes incompatible providers. An
explicit request for an incompatible model returns a stable unsupported-feature
error rather than silently changing semantics.

### Feedback

`POST /v1/feedback` accepts:

```json
{
  "response_id": "resp_...",
  "outcome": "positive | negative",
  "reason": "optional caller explanation"
}
```

Positive and negative feedback are stored as explicit outcomes. Negative
feedback raises the associated conversation's minimum tier by one, capped at
T6, for future turns. It does not rewrite the response already returned and does
not update global success priors online.

### Operations

- `GET /healthz` reports whether the process is alive.
- `GET /readyz` reports whether configuration, migrations, SQLite, required
  encryption keys, and at least one enabled provider are usable.

## Architecture

The first release is a modular monolith: one Go binary with explicit internal
interfaces and no inter-process RPC.

```text
/v1/responses
      |
authentication and request normalization
      |
deterministic feature extraction and pre-routing
      |                         \
      | decisive                \ uncertain
      v                          v
deterministic route        Jev System One classifier
      |                          |
      +-------------+------------+
                    v
          deterministic policy
                    |
           tier and concrete model
                    |
       OpenAI / Anthropic / Gemini adapter
                    |
        verification and bounded escalation
                    |
       Responses-compatible result or stream
```

The principal package boundaries are:

```text
cmd/mindctl/                 binary entrypoint
internal/api/                Responses, feedback, health handlers
internal/config/             YAML loading and validation
internal/domain/             provider-neutral request/result types
internal/router/             features, T0-T6 policy, candidate scoring
internal/classifier/jev/     Jev System One HTTP client
internal/provider/           OpenAI, Anthropic, Gemini adapters
internal/executor/           attempts, budgets, streaming, escalation
internal/verifier/           deterministic and model verification
internal/storage/sqlite/     migrations and repositories
internal/contentcrypto/      AES-GCM encryption and key rotation
internal/telemetry/          structured events and tracing
migrations/                  embedded SQLite migrations
config.example.yaml          documented runnable configuration
```

Interfaces are defined by their consumers. Provider adapters do not depend on
routing policy, the policy does not perform network or storage operations, and
the Jev classifier returns signals rather than decisions.

## Routing flow

### 1. Normalize and extract deterministic features

The gateway converts a supported Responses request into a canonical request and
extracts:

- Estimated input and conversation tokens
- Modalities
- Custom and hosted tool requirements
- Structured-output requirements
- Context and output limits
- Explicit model or tier bounds
- Conversation pin and escalation floor
- Enabled providers, credentials, health, and model availability
- Known provider cache state when exposed by usage metadata

Hard constraints are applied before any probabilistic scoring. A model lacking a
required modality, tool, output feature, context capacity, credential, or
availability status is ineligible.

### 2. Deterministic pre-routing

The pre-router skips Jev when the route is already determined, including:

- A concrete model request
- A compatible pinned conversation model
- A request with an explicit tier that resolves to a single policy outcome
- A configured deterministic rule for a narrow task such as a known extraction
  or classification contract

The pre-router may establish a minimum tier but must not select an ineligible
model.

### 3. Jev classification

Uncertain automatic requests make one TypeSafe System One call with independent
questions evaluated over the current user turn and relevant structured execution
context. The call asks for:

- `minimum_tier`: Choice over T0 through T6
- `task_type`: Choice over configured task categories
- `reasoning_required`: Score
- `coding_required`: Score
- `blast_radius`: Score
- `underspecified`: Noul

The response preserves each answer's typed value, probability distribution or
score, any confidence field supplied for that answer type, the resolved Jev
model version, usage, and latency. The implementation calls
`POST https://api.typesafe.ai/v1/systemone` directly because TypeSafe currently
documents Python and JavaScript SDKs but not a Go SDK. The Jev model identifier
is configurable and should be pinned for evaluated deployments rather than
silently following a moving alias.

The Jev client has an outer deadline, bounded exponential backoff for 429 and 529
responses, and no prompt-bearing debug logs. A timeout, transport error, or
malformed answer returns an unavailable classification; it never crashes the
request path.

### 4. Determine the tier floor

Pure deterministic policy combines pre-router features and Jev signals.

- Explicit caller constraints take precedence.
- A conversation pin prevents downgrades.
- Low Jev confidence cannot lower the floor.
- High blast radius or underspecification may raise the floor according to
  configurable thresholds.
- Reasoning and coding scores may raise the floor but cannot make it lower than
  Jev's minimum tier.
- Jev failure uses the conversation pin when present and otherwise a configurable
  safe fallback, initially T4.
- Bounds are validated. An impossible `min_tier`/`max_tier`/capability
  intersection returns a policy error rather than choosing an unsafe model.

Every transformation appends to an ordered reason chain so the final decision is
reproducible from stored inputs.

### 5. Select a concrete model

For every eligible model at or above the floor, policy calculates estimated
input, output, cache, and fixed charges, then combines them with configured
success and latency priors. Success priors are keyed by model and task type,
with a model-wide fallback.

The selected candidate minimizes expected total cost while meeting configured
minimum success probability, maximum cost, and maximum latency constraints.
Ties prefer the lower direct cost, then lower latency, then stable configuration
order. The decision records all rejected candidates and their rejection reasons
or scores.

Production outcomes are stored but do not mutate priors online. Updating priors
requires an offline analysis and an explicit configuration change.

## Conversation policy

The first automatic selection pins both the model and provider to the gateway
conversation. Subsequent turns reuse that selection when it remains compatible.
Automatic downgrade is prohibited, avoiding lost provider cache value and
unpredictable quality changes.

An escalation raises the conversation floor and replaces the pin with the
successful stronger selection. A provider or model may also change when the
pinned target is unavailable or no longer supports the request; the policy must
choose an equal or stronger eligible tier and record the forced switch.

The gateway stores a canonical transcript rather than relying on provider-native
response IDs. This permits `previous_response_id` to work with all three native
provider protocols. Provider-specific IDs may be stored as auxiliary metadata
for diagnostics and cache-aware execution.

## Provider adapters

Each adapter implements the same executor contract for non-streaming and
streaming calls and returns canonical output items, usage, provider request IDs,
finish status, retry classification, and latency.

- **OpenAI:** maps canonical input to the native Responses API and normalizes
  typed output items and SSE events.
- **Anthropic:** maps instructions and message items to Messages, translates
  custom tools and tool results, converts supported image inputs, and normalizes
  content blocks and streaming events.
- **Gemini:** maps canonical content to `generateContent`/streaming content,
  translates supported functions and schemas, and normalizes candidates, parts,
  finish reasons, and usage.

Adapters must not invent equivalents for unsupported hosted tools. Capability
declarations and adapter validation protect this boundary twice: once during
eligibility filtering and once before the network call.

## Verification and escalation

### Deterministic verification

Every completed non-streaming attempt checks:

- Successful transport and provider completion state
- Presence of a usable output item when output is expected
- JSON Schema conformance for structured output
- Function-call names, argument JSON, and requested tool constraints
- Configured length and required-output rules

These checks produce explicit pass, fail, or indeterminate results with evidence.

### Model verification

Policy may invoke a configured verifier for selected non-streaming requests,
based on tier, task type, risk, underspecification, caller controls, or
deterministic uncertainty. The verifier receives the canonical request, candidate
answer, and a narrow rubric and returns pass/fail plus confidence and reasons.

The verifier model is configured explicitly and called directly through its
provider adapter. It does not pass through the router, preventing recursive
routing or escalation. Verifier cost and latency are recorded separately.

### Escalation

On retryable provider failure, the executor may try another eligible model at
the same or a higher tier. On verification failure, the next attempt must be at
least one tier higher. Attempts are bounded by configured maximum attempts,
total estimated cost, and wall-clock duration.

The executor persists every attempt, including rejected encrypted output and
verification evidence. When budgets are exhausted, the default behavior is a
Responses-style error; operators may explicitly configure best-effort behavior
to return the strongest completed candidate with failure metadata headers. The
complete failure remains recorded in either mode. The executor never loops at
T6.

For streaming, retry and escalation are allowed only before the first
client-visible event. After emission, a failure becomes a terminal SSE error,
the attempt is recorded, and the conversation floor increases for the next
turn.

## Persistence and privacy

SQLite is the initial persistence layer. It uses embedded migrations, foreign
keys, WAL mode, a busy timeout, and repository interfaces. The schema covers:

- Conversations and model pins
- Gateway requests and canonical responses
- Routing decisions and candidate evaluations
- Jev requests and judgments
- Provider attempts and usage
- Verifier runs
- Feedback and outcome provenance
- Model price and success-prior snapshots used for each decision

Raw prompts, answers, rejected outputs, feedback reasons, and other captured
content are encrypted with AES-GCM before storage. Each encrypted value stores a
key ID, unique random nonce, ciphertext, and format version. Configuration
provides one active key and optional older decryption keys through environment
variables. This supports rotation by writing with the active key while retaining
read access to existing records. Missing active key material is a startup error
because raw capture is enabled by default.

Raw-content retention defaults to unlimited. A nonzero configured retention
period enables a maintenance task that removes expired encrypted content while
retaining non-content telemetry and aggregate outcomes. Unlimited retention has
an explicit disk-growth and data-governance cost and must be visible in startup
logs and the example configuration.

Raw prompt and answer text never appears in normal logs or tracing attributes.
Sending a prompt to Jev and sending request/answer content to an optional model
verifier are documented external data disclosures controlled by configuration.

## Configuration

One YAML file defines behavior; secrets are referenced by environment-variable
name rather than embedded values. Configuration includes:

- HTTP listen address and gateway client-auth secret reference
- Jev endpoint, pinned model, secret reference, timeouts, and retry limits
- Provider endpoints and secret references
- Model catalog, tiers, capabilities, context limits, availability, and prices
- Task-specific and model-wide success priors
- Tier thresholds and T4 safe fallback
- Verifier selection rules and verifier model
- Escalation attempt, cost, and latency budgets
- SQLite path and retention duration, defaulting to unlimited
- Active encryption key ID and decryption key references
- Logging and tracing settings

Configuration is fully validated before the listener starts. Unknown tiers,
duplicate model IDs, missing referenced models, impossible bounds, invalid price
units, absent required secrets, or unusable encryption keys are startup errors.
Hot reload is not part of the first release.

## Failure behavior

- Invalid configuration or migration failure: fail startup.
- Jev unavailable or malformed: use the conversation pin or T4 fallback.
- Retryable provider failure before output: select another eligible model at the
  same or a higher tier within budget.
- Verification failure: escalate at least one tier.
- No compatible model: return a stable Responses-style error.
- Streaming failure after emission: send a terminal error event and raise the
  conversation floor.
- SQLite unavailable before execution: fail readiness and reject inference so
  the gateway does not intentionally create unaudited attempts.
- SQLite failure after streaming begins: terminate the stream when possible,
  emit a critical content-free log, and fail readiness; already emitted bytes
  cannot be recalled.

Provider HTTP errors are classified into retryable, non-retryable request,
authentication, rate-limit, overload, and unknown groups. Authentication and
invalid-request failures do not consume attempts on stronger models unless a
different provider can validly satisfy the same canonical request.

## Observability

Structured logs and traces carry request, response, conversation, decision,
attempt, and provider-request IDs. Spans distinguish routing, Jev, provider
inference, verification, storage, and feedback purposes. Metrics include:

- Route counts by tier, provider, model, task type, and reason
- Jev latency, failures, confidence, and token usage
- Provider latency, tokens, cache tokens, errors, and actual cost
- Verification pass/fail/indeterminate counts and cost
- Escalation rate, attempt depth, and terminal outcome
- Positive and negative feedback by original route
- SQLite latency, lock contention, and database size

Metrics and logs exclude plaintext captured content and credentials.

## Testing strategy

The implementation will use deterministic local tests and opt-in live smoke
tests.

- Table-driven policy tests cover every tier, bounds, confidence gates, risk,
  underspecification, pins, and fallback behavior.
- Property and fuzz tests assert that selection never falls below the floor,
  never selects an incompatible model, never automatically downgrades a pinned
  conversation, and never exceeds configured escalation bounds.
- Expected-cost tests cover input, output, cache, verifier, and escalation cost.
- `httptest` provider fixtures verify outbound native wire formats and inbound
  response/SSE normalization without paid services.
- Jev fixtures cover the documented Choice, Score, and Noul response shapes,
  malformed answers, deadlines, 429/529 retry behavior, and T4 fallback.
- Executor scenarios cover non-streaming verification escalation, retryable
  provider errors, budget exhaustion, pre-first-event streaming retry, and
  post-emission terminal failure.
- API tests cover authentication, Responses shapes, stable errors,
  `previous_response_id`, conversation pinning, and feedback escalation.
- SQLite tests cover migrations, transactions, concurrency, ciphertext at rest,
  unique nonces, key rotation, and the unlimited-retention default.
- Race tests cover concurrent requests and conversations.

The normal verification commands are:

```text
devenv shell -- go test ./...
devenv shell -- go test -race ./...
```

Live Jev and provider smoke tests are opt-in and require explicit credentials.
They are additional integration evidence, not a prerequisite for the deterministic
test suite.

## Initial delivery boundary

The first implementation is complete when a locally configured gateway can:

1. Accept a supported `mindctl-auto` Responses request.
2. Produce a deterministic or Jev-assisted T0-T6 decision.
3. Select a candidate using expected total cost.
4. Execute through each native provider adapter against local protocol fixtures.
5. Buffer and verify non-streaming output, escalating when required.
6. Stream output with the documented pre-emission retry boundary.
7. Continue a pinned conversation using `previous_response_id`.
8. Apply negative feedback to the conversation floor.
9. Persist encrypted content and complete attempt telemetry in SQLite.
10. Pass the deterministic and race-enabled Go test suites.

## References

- [OpenAI: Migrate to the Responses API](https://developers.openai.com/api/docs/guides/migrate-to-responses)
- [TypeSafe System One HTTP API](https://docs.typesafe.ai/api)
- [TypeSafe confidence-gated routing](https://docs.typesafe.ai/patterns/confidence-routing)
- [its-panzer/jev-model-router](https://github.com/its-panzer/jev-model-router)
- [TokenTrim/jev-routing-experiment](https://github.com/TokenTrim/jev-routing-experiment)
- [gargpratyush/jev-router](https://github.com/gargpratyush/jev-router)
- Local infrastructure reference: `/home/development/repos/model-router`
