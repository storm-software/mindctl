# Managed Headroom Compression Design

**Date:** 2026-09-26

**Status:** Proposed design for review

## Purpose

Let operators opt into Headroom's context compression through **Mindctl alone**
to reduce provider input tokens. This must work for Homebrew, npm, and native
binary installs without installing Python, Headroom, or another service by
hand. Mindctl may download and cache pinned runtime and model assets
automatically. Compression is never a reason to bypass Mindctl's routing,
authentication, or conversation ownership.

## Selected approach and alternatives

Mindctl manages a local Headroom process and calls its compression-only
`POST /v1/compress` endpoint after selecting the upstream model, before
forwarding any request to that model. Mindctl remains the only provider client;
Headroom never receives provider credentials or forwards requests upstream.
The Headroom package is not copied or modified in this repository. A
platform-specific, pinned runtime is installed into Mindctl's private cache on
first use and supervised for the lifetime of the gateway. The operator only
sets Mindctl's `headroom.enabled` option.

An in-process port of Headroom would duplicate Python/Rust compression logic
and drift from upstream. Placing the Headroom forwarding proxy between Mindctl
and each provider would instead change provider-specific authentication,
Responses/Codex paths, streaming, and upstream endpoint selection. Neither is
the initial integration. Existing setups that already put Headroom in front of
Mindctl must avoid enabling both compression layers; this change does not
silently rewrite external client or Home Manager configuration.

## Configuration and lifecycle

- Add a top-level `headroom` configuration section with `enabled: false` by
  default. Accept `mode: cache` (default) or `mode: token` as an optional
  Mindctl setting; no independent Headroom configuration is required. Reject
  unknown options and invalid modes using the existing strict configuration
  reader. Document `headroom.enabled: false` and restart as the way to disable
  compression after an outage. Existing configs retain their current behavior.
- Pin an upstream Headroom version, Python interpreter, bootstrap utility,
  Python dependency lock, and platform artifacts. Fetch over HTTPS, verify
  expected hashes, extract atomically under Mindctl's per-user cache directory,
  and lock provisioning so parallel starts do not corrupt it. Never execute an
  unverified or partly installed download. A supported target must have a
  reproducible installation path; report a clear error on unsupported targets.
- Run Headroom as a Mindctl-owned child bound only to loopback on a per-instance
  port, with an ephemeral authentication token. Pass a minimal environment,
  excluding gateway, OAuth, and provider credentials. Disable Headroom's
  outbound usage beacon and content logging by default; model/runtime downloads
  are permitted. Bound startup, readiness, request, and shutdown times, and
  terminate the child on Mindctl shutdown. A disabled installation neither
  provisions nor starts Headroom.
- The existing Homebrew formula, npm launcher, and native release archives all
  launch the same Go executable, which owns provisioning and supervision.
  Exercise cold and warm starts on every shipped OS/architecture target. The
  container distribution must likewise provide a Python-capable, managed
  runtime or an equally single-configuration bundled process; the current
  distroless image cannot execute a Python child unchanged. Keep compression
  disabled by default for every distribution.

## Request and conversation contract

Compression occurs only **after** Mindctl has validated and authenticated the
request, loaded its durable transcript, selected an eligible model, and scoped
provider-native fields to that provider. It runs **before** starting a
provider request or making an SSE event visible. Both streaming and
non-streaming executions use the same compression contract. Do not make
routing, classification, or model availability depend on a speculative
compressed token estimate; record actual provider usage separately.

Use Headroom's marker-free compression mode: no CCR retrieval markers, injected
retrieval tools, provider re-drives, output steering, or Headroom model routing.
Identify compressible canonical text and text-valued tool results, and build
Headroom-supported message shapes from those fields. Include user text as
context, but do not opt into rewriting user messages in the initial version:
Headroom's replayable session mode does not combine with that option. Reapply
only validated text changes to a copy of the provider-scoped request. Do not
change item count/order, roles, call IDs, tool names or schemas, JSON types, system
instructions, image data, reasoning summaries/signatures, encrypted content,
opaque provider data, or authentication fields. Preserve these fields exactly
for the OpenAI Responses/Codex, Anthropic Messages, and Gemini adapters.
Headroom's `/v1/compress` endpoint accepts OpenAI chat and Anthropic message
shapes, **not** the full Responses request; never pass a Responses body to it
or lossy-convert one. A dedicated canonical-field mapping with strict
round-trip validation must be exercised against the pinned real Headroom
version before treating a field as supported. If a field cannot be mapped
safely, leave it untouched. A request with no eligible fields is valid and
does not require a compression call, but still requires a healthy managed
Headroom service when the feature is enabled; check child liveness and a
bounded readiness probe before forwarding it. A valid result is not required
to save tokens on every turn.

Keep the durable conversation transcript in its original form: never persist
the compressed provider input in place of the user's input or the provider's
result. For continuation requests, use a stable Headroom session identity
scoped to the Mindctl conversation and selected provider/model so a cached
prefix is byte-stable across turns. Use a single managed worker or otherwise
maintain session affinity; a different provider/model starts a distinct
compression session. If Headroom cannot replay a session consistently,
reject the attempt rather than bypassing it. A child restart may lose cache
benefits but must not corrupt conversation contents or cross provider
boundaries.

## Fail-closed behavior

When `headroom.enabled` is true, no compression failure may result in an
uncompressed upstream provider request. Download, installation, launch,
readiness, HTTP, timeout, invalid response, unexpected shape, or session
failure leaves the gateway able to serve errors, but marks compression
unavailable. Reject affected requests, including ones without eligible text,
with a sanitized **503** and stable `headroom_unavailable` code before
contacting a provider. Also reject Headroom's HTTP 200 fail-open result when
it explicitly reports
`compression_skipped: true`, including its documented timeout case. A valid
unchanged result with zero token savings is not a failure. Never silently
disable compression, fall back to another model, or edit the user's config.
The error tells the operator to set `headroom.enabled: false` and restart to
resume service without compression.

The Responses and Anthropic Messages endpoints express the same condition in
their respective error formats. For SSE requests, ensure compression completes
before emitting any event, so failure returns an ordinary HTTP 503 rather
than a partial success stream. Record failed attempts without attributing a
provider request ID or usage. Log only safe error categories and aggregate
Headroom token metrics, never prompts, tool output, secrets, or raw sidecar
error bodies. Metrics from Headroom describe estimated compression; provider
usage remains the source for billed input and cached tokens.

## Verification and rollout

- Config tests cover default-off behavior, enabled modes, and unchanged
  authentication and provider catalog validation.
- Mock Headroom tests cover eligible field mapping and structural invariants;
  unchanged requests with no eligible fields; `compression_skipped`; 400/401/
  404/503, timeout, malformed responses, startup/download failure, child exit,
  and concurrency. Every failure proves **zero** upstream inference requests.
- Real pinned-Headroom contract tests cover text/tool-result compression,
  streaming and non-streaming paths, Responses/Codex continuation fields,
  Anthropic native content, Gemini-bound requests, and multi-turn prefix
  stability. Unsupported fields are documented rather than claimed to save
  tokens. Verify a real Headroom response can round-trip safely before
  shipping compression for each field class.
- Distribution checks exercise first-run download and warm cached restart for
  macOS x86_64/arm64, Linux x86_64/arm64, and Windows x86_64; verify staged
  npm, Homebrew, and native assets and container startup where applicable.
  Tests distinguish deterministic mock proof from live Headroom integration;
  provider billing reduction cannot be claimed without measuring a live
  provider response.

No Headroom-managed client reconfiguration, external Home Manager changes,
automatic provider rerouting, or upstream Headroom modifications are included.
