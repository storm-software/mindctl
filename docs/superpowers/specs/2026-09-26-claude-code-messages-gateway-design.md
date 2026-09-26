# Claude Code Messages Gateway Design

**Date:** 2026-09-26

**Status:** Proposed design for review

## Purpose

Let Claude Code use Mindctl directly through an Anthropic Messages-compatible
endpoint while retaining its existing claude.ai subscription login. Mindctl
continues to select from all eligible configured providers, not only Anthropic.
Extend `mindctl status` with a Claude row and `mindctl status claude` after the
new endpoint has a defined client configuration.

## Scope and compatibility

- Add `POST /v1/messages` for non-streaming and SSE streaming requests,
  including a harmless `beta=true` query parameter Claude Code may send.
  Accept the documented
  Anthropic Messages request shape, preserve tool-use and tool-result ordering,
  and emit Anthropic Messages responses, stop reasons, usage, and SSE events.
- Support ordinary text, system instructions, image blocks, tools, multi-turn
  conversations, and the thinking/continuation fields required for supported
  Claude Code turns. Preserve Anthropic-native opaque content when routing back
  to Anthropic; never fabricate signatures or send it to another provider.
- Reuse the existing model catalog, request-scoped credentials, router,
  executor, history, and provider adapters. `mindctl-auto` can select any
  eligible provider with the required capabilities and credentials. Explicit
  model IDs select only their configured catalog match; unknown IDs fail
  clearly rather than being silently remapped. Document Claude Code's main
  and background model overrides; built-in model names may still occur and
  require matching catalog entries or explicit aliases.
- Support the mandatory Messages endpoint first. Token counting is optional
  for Claude Code; an absent `/v1/messages/count_tokens` lets the client use
  its fallback estimate. Model discovery and other auxiliary endpoints are
  outside the initial scope.

The endpoint is opt-in behind a new `claude_messages.enabled` gateway setting,
defaulting to `false`. When disabled, `/v1/messages` is not registered and
existing configurations, including those authenticating gateway clients with
`Authorization`, behave as before. Enabling it requires a dedicated client
auth header distinct from `Authorization`, such as `X-Mindctl-Token`, and an
Anthropic provider configured for Claude OAuth passthrough to make the
subscription-backed route eligible. Other providers retain their existing
independent auth configuration.

When a request requires an Anthropic-only capability, only compatible
Anthropic candidates are eligible for that request. This is per-request
capability filtering, not an Anthropic-only restriction on the Claude client.
Unsupported required fields fail with an actionable client error; Mindctl
never silently drops them or retries against an incompatible provider.

## Authentication and credential boundaries

Claude Code keeps its saved subscription login and refresh behavior. Configure
`ANTHROPIC_BASE_URL` without `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, or an
`apiKeyHelper` that would replace the subscription credential. Its native
`Authorization: Bearer` credential remains request-scoped in Mindctl and is
usable only by the configured Anthropic OAuth passthrough provider. Mindctl
does not read Claude's account files or persist, print, or log that credential.
Capture this credential only on the new Messages route; the existing
`X-Mindctl-Claude-Token` path for Codex Responses remains unchanged.

Mindctl authenticates the client independently using the configured dedicated
gateway header (`X-Mindctl-Token` in the example configuration). Claude Code
can supply it through `ANTHROPIC_CUSTOM_HEADERS` from a trusted launcher that
reads the existing Mindctl secret; do not embed it in a tracked project file
or use `ANTHROPIC_AUTH_TOKEN` for this purpose. Reject a missing or invalid
gateway token before decoding a request or capturing provider credentials.
Configuration validation rejects gateway authentication via `Authorization`
when the Claude endpoint is enabled, so gateway and subscription tokens
cannot collide. Preserve the existing Responses OAuth/API-key split and
its authentication behavior unchanged.

For Anthropic-bound requests, pass through the caller's relevant
`anthropic-version` and complete `anthropic-beta` capability header rather
than replacing the latter with a fixed OAuth beta value. Do not forward the
subscription bearer, its beta capability, or opaque Anthropic-only data to
any other provider. Other providers use only their separately configured
credentials; having a Claude subscription does not grant access to them.
Gateway forwarding must not follow redirects that could leak credentials.
Codex-originated requests without these Claude headers continue to use the
existing Anthropic adapter defaults.

## Request and response flow

The new API adapter validates and bounds the Messages body, extracts only
supported request semantics into the portable inference request, and records
provider-specific opaque fields separately with provenance. Authentication
supplies the authenticated client ID and, if present, the Claude OAuth
credential to the existing executor. The router filters candidates by
capability and available credentials before selection and fallback. The
selected provider returns the portable result or stream, which the adapter
encodes back to the Messages wire format.

For streams, emit the Messages lifecycle in order: `message_start`,
`content_block_start`/deltas/stop, `message_delta`, then `message_stop`.
Translate text, thinking, and tool input deltas without reordering content
blocks or losing tool IDs. Preserve backpressure and cancellation. Existing
pre-output retry rules still apply; after an SSE event is visible, report the
failure in-band and do not silently switch providers. Avoid committing a
success record for an incomplete stream.

Return an Anthropic-shaped error response for invalid requests,
authentication failures, missing credentials, and provider failures, with
appropriate HTTP status. Never include prompts, tool outputs, upstream
credentials, or raw upstream error bodies in error text or logs. A request
using a non-Anthropic provider gets a synthesized Messages response whose
model, finish reason, and usage represent the actual selected provider; if
an accurate translation is impossible, return a clear error instead.

## Claude configuration and status

Provide opt-in setup instructions for Claude Code: set the non-secret
`ANTHROPIC_BASE_URL` in `~/.claude/settings.json` to the Mindctl Messages
origin (for the documented local default, `http://127.0.0.1:8080`). An
example launcher injects only the gateway header from the existing secret,
without storing its value in Claude settings. Do not overwrite the existing
Headroom-owned Claude configuration or modify the separate Home Manager
repository as part of this change. Document how to deliberately select
Mindctl in that managed setup and how to return to Headroom.

Add `claude` to the existing harness table. `mindctl status claude` reads the
top-level `env.ANTHROPIC_BASE_URL` in `~/.claude/settings.json` and prints
only `connected` when it matches the documented local Mindctl origin
(`http://127.0.0.1:8080`, with an optional trailing slash); otherwise it
prints `disconnected`. Missing settings are disconnected; malformed JSON or
unreadable settings return errors. Like the Codex check, this is a check of
global configuration, not a network probe or a guarantee of effective
project-local/shell overrides, authenticated access, or model availability.

## Verification

- Handler tests cover auth separation, body limits, unknown/unsupported
  fields, model selection, request-to-provider mapping, and provider-to-client
  mapping for text, tools, images, thinking, errors, and usage.
- Stream tests cover event ordering, tool input fragments, cancellation,
  partial failures, and no provider switch after visible output.
- Security tests prove Claude OAuth is forwarded only to Anthropic, not to
  other providers or redirects; malformed credentials fail closed and never
  appear in persistence or diagnostic output.
- CLI tests cover both status forms, absent/different/malformed Claude
  settings, plus unchanged Codex behavior and gateway startup.
- Run focused and full Go suites, then an opt-in real Claude Code multi-turn
  text-and-tool session on each intended route. Local fixture tests alone
  do not establish live subscription or cross-provider compatibility.

## Non-goals

- Managing Claude's login, token refresh, or account files.
- Claiming Claude subscription benefits when a request selects a different
  provider; that provider uses its own configured billing and credentials.
- Automatically editing Home Manager, enabling Mindctl for every Claude Code
  process, or publishing a release as part of this feature.
- Promising transparent support for all future Claude beta features or every
  Anthropic-only feature on non-Anthropic providers.
