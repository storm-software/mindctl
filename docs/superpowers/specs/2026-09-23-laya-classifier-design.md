# Laya Classifier Replacement Design

**Status:** Approved design, awaiting implementation plan review

## Purpose

Replace the TypeSafe Jev dependency with the local-first
`convaiinnovations/laya` System 1 typed-decision model while preserving
Mindctl's deterministic routing policy. Operators must be able either to run
the supplied Laya sidecar or to point Mindctl at an independently managed
instance of the same service.

Laya is a Python/Torch package rather than a Go library or hosted service.
Mindctl therefore integrates over a small, versioned HTTP boundary instead of
embedding Python in the gateway process.

## Goals and non-goals

Goals:

- Provide Laya's six existing requirement signals in one typed-decision call:
  minimum tier, task type, reasoning, coding, blast radius, and
  underspecification.
- Ship an optional local OCI sidecar and Compose profile.
- Support an operator-managed sidecar through exactly the same authenticated
  endpoint contract.
- Remove Jev-specific naming from configuration, code, persistence, operator
  documentation, and user-visible routing reasons.
- Retain the current deadline, bounded retry, conversation-pin, and T4 safe
  fallback behavior.

Non-goals:

- Start or supervise Python from the Mindctl Go binary.
- Allow callers to send arbitrary questions, select a checkpoint, or change
  Laya's fixed requirement rubric.
- Download model weights during ordinary Go tests or require GPU hardware for
  development and CI.
- Preserve the removed `jev:` configuration block as a compatibility alias.

## Architecture

```text
Responses request
       |
Mindctl Go gateway -- POST /v1/classify --> Laya sidecar
       |                                       |
       |                                       +-- pinned Laya typed-decisions model
       +-- deterministic policy, provider selection, storage
```

The gateway sees only a `classifier.Classifier` implementation. The Laya HTTP
client produces a provider-neutral `domain.ClassifierJudgment`; it does not
choose a model or make routing-policy decisions. The router consumes the same
signals as today, renamed to neutral terminology. Classifier success can only
raise a deterministic floor. Classifier failure uses a compatible conversation
pin when present, otherwise the configured safe fallback tier (T4 by default).

The sidecar is a small FastAPI process. It initializes Laya during startup via
the typed-decision router with preloading enabled and a pinned
`convaiinnovations/laya` revision/variant. It owns the six fixed questions and
turns the input state into a single Laya prediction. The sidecar starts serving
readiness only after model initialization has completed.

The published `mindctl-laya` image contains Python, the pinned Laya package,
Torch, and the sidecar. An opt-in Compose profile puts it on the gateway's
network and mounts a model-cache volume. The existing Mindctl image remains a
small Go image. CPU/GPU scheduling and cache placement are deployment choices,
not gateway configuration.

## Classifier HTTP contract

The sidecar and remote deployments implement `mindctl.classifier.v1`.

`POST /v1/classify` requires `Authorization: Bearer <token>` and accepts a
bounded JSON document:

```json
{
  "schema_version": "mindctl.classifier.v1",
  "state": {
    "prompt": "...",
    "features": { "...": "..." },
    "current_model": "...",
    "available_models": ["..."]
  }
}
```

Only the `state` is client input. The model repository, exact revision,
typed-decision variant, and questions are sidecar configuration. This prevents
an untrusted request from changing the classifier workload or model artifact.

The successful response identifies the serving model and returns fixed,
typed answers:

```json
{
  "schema_version": "mindctl.classifier.v1",
  "classifier": {
    "name": "laya",
    "repository": "convaiinnovations/laya",
    "revision": "5e7b2b1b8ca2ecdd3f2322d94069c9b6ce7e844b",
    "variant": "typed-decisions"
  },
  "answers": {
    "minimum_tier": {
      "type": "choice",
      "choice": "T4",
      "confidence": 0.9,
      "probabilities": { "T0": 0.0, "T4": 0.9 }
    },
    "task_type": {
      "type": "choice",
      "choice": "coding",
      "confidence": 0.8,
      "probabilities": { "coding": 0.8 }
    },
    "reasoning_required": {
      "type": "score",
      "score": 3.2,
      "confidence": 0.7,
      "probabilities": { "3": 0.6 },
      "legend": { "0": "None", "3": "Moderate" }
    },
    "coding_required": {
      "type": "score",
      "score": 3.0,
      "confidence": 0.8,
      "probabilities": { "3": 0.8 },
      "legend": { "0": "None", "3": "Bounded implementation" }
    },
    "blast_radius": {
      "type": "score",
      "score": 1.0,
      "confidence": 0.9,
      "probabilities": { "1": 0.9 },
      "legend": { "0": "Negligible", "1": "Local and reversible" }
    },
    "underspecified": { "type": "noul", "noul": 0.1 }
  }
}
```

The actual fixed distributions must contain the complete configured criteria,
not the abbreviated example above. The Go client validates the schema version,
model metadata, answer types, legal labels, finite score ranges, confidence
ranges, and complete normalized probability distributions before accepting a
judgment. It records its own end-to-end latency. Laya does not report provider
token usage, so the neutral judgment has no fabricated token counts.

`GET /healthz` reports that the process can answer HTTP. `GET /readyz` reports
that the pinned model is loaded and able to accept inference. Neither endpoint
returns secrets, prompts, model cache paths, or exception detail.

## Configuration and compatibility

Mindctl replaces `jev:` with one endpoint-oriented block:

```yaml
classifier:
  endpoint: http://laya:8091
  token_env: LAYA_CLASSIFIER_TOKEN
  timeout: 5s
  max_retries: 2
```

There is no local/remote behavior switch in the Go gateway. The example Compose
profile supplies the `http://laya:8091` endpoint; an independently managed
sidecar uses its own URL and the same bearer token. The sidecar's image/runtime
configuration pins the Laya repository revision and typed-decision variant.

The neutral names are used throughout: `ClassifierJudgment`,
`min_classifier_confidence`, classifier routing reasons, metrics, and
documentation. `jev_judgments` is renamed to `classifier_judgments` by a new
SQLite migration. Existing rows retain their serialized fields and remain
readable as historical classifier judgments; the migration does not reinterpret
them as Laya output. The pre-release configuration change is deliberately
breaking so an old Jev endpoint cannot accidentally be invoked.

## Failure handling and security

- Both gateway and sidecar cap request/response body sizes. The sidecar accepts
  only the defined input schema and rejects unknown/malformed structures.
- The sidecar compares a required bearer token without exposing it. Compose
  supplies the same referenced environment value to both services; remote
  operators configure their own secret distribution.
- The gateway continues its five-second default outer deadline and bounded
  exponential retries only for explicitly retryable availability failures.
- Sidecar startup and prediction failures become generic unavailable responses;
  logs and error bodies exclude request state, raw provider exceptions, and
  credentials.
- Inference concurrency is bounded. Readiness is false while model load is in
  progress or after fatal initialization failure.
- The gateway treats authentication, validation, redirect, timeout, and
  malformed-answer failures as `classifier.ErrUnavailable`, preserving the
  existing safe routing fallback.

## Verification

Python sidecar tests mock Laya's router and assert the immutable six-question
definition, mapping of choice/score/noul results, auth enforcement, body limits,
readiness transitions, and redacted failure responses. They do not fetch model
weights.

Go tests cover the Laya endpoint client: request/auth shape, redirect refusal,
timeout/retry rules, strict response validation, and every malformed answer
case. Shared JSON fixtures exercise both sides of `mindctl.classifier.v1`.

Gateway/config tests prove local-Compose and remote endpoint configurations are
identical to the Go application, preserve no-classifier fallback and pin
behavior, reject old Jev configuration, and validate the new secret reference.
Storage tests cover table migration, reading historical records, and persistence
of new neutral judgments. Router tests prove behavior is unchanged apart from
neutral names.

Image and Compose tests build the gateway and sidecar artifacts and start a
mocked sidecar path. A separate opt-in live smoke test may download the pinned
Hugging Face artifact and make one real typed-decision request; it is never part
of the default Go/Python test suites and must declare its CPU/GPU runtime.

## Acceptance criteria

1. A Compose user can opt into the supplied Laya sidecar and complete an
   authenticated classification through Mindctl.
2. A remote sidecar with the same contract produces the same gateway behavior.
3. Classifier output never directly selects a provider/model; deterministic
   policy still owns selection and failure fallback.
4. No code, configuration, migrations, metrics, docs, or user-facing route
   reasons retain Jev as the current classifier dependency.
5. Existing SQLite records survive the table rename, and focused contract,
   migration, config, Go, Python, and packaging tests provide the stated proof.
