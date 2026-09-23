# ChatGPT OAuth Passthrough Design

**Date:** 2026-09-23

## Summary

Mindctl will support routing Codex Responses requests through the caller's
ChatGPT subscription. Codex remains responsible for ChatGPT login and OAuth
token refresh. Mindctl authenticates the Codex client separately, keeps the
OAuth credential request-scoped, and forwards it only to the configured
ChatGPT Codex backend.

Existing API-key providers remain supported. A provider chooses exactly one
authentication mode; Mindctl never silently falls back from ChatGPT OAuth to
an API key.

## Goals

- Allow a Codex client authenticated with ChatGPT OAuth to use Mindctl without
  configuring `OPENAI_API_KEY` for the OpenAI provider.
- Preserve Mindctl's independent client-authentication boundary.
- Preserve the existing deterministic T0-T6 routing, conversation pinning,
  streaming, storage, and provider-normalization behavior.
- Keep OAuth access tokens and ChatGPT account IDs out of request bodies,
  inference objects, logs, telemetry, errors, and persistence.
- Retain backward compatibility for existing API-key configuration.

## Non-goals

- Implementing browser login, device-code login, token refresh, revocation, or
  OAuth credential storage in Mindctl.
- Reading or writing Codex's `auth.json` file.
- Supporting arbitrary pass-through headers or caller-selected upstream URLs.
- Guaranteeing that every configured model is available to a particular
  ChatGPT account or subscription tier.
- Adding WebSocket transport.

## Configuration

### Mindctl

`client_auth.header` selects the HTTP header containing Mindctl's configured
gateway token. It defaults to `Authorization`, preserving existing
configuration. The value must be a valid HTTP field name.

`providers[].auth` selects the provider credential mode:

- `api_key` is the default and preserves the current behavior. It requires a
  non-empty `api_key_env` whose referenced environment variable is present.
- `chatgpt_oauth_passthrough` is supported by the OpenAI adapter. It forbids
  `api_key_env`, receives credentials from each authenticated request, and
  uses the ChatGPT Codex Responses endpoint.

When any provider uses `chatgpt_oauth_passthrough`, `client_auth.header` must
not be `Authorization` or `ChatGPT-Account-Id`, compared case-insensitively.
This prevents gateway authentication from consuming or conflicting with the
upstream credential.

Example:

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

API-key configuration remains valid when `auth` and `client_auth.header` are
omitted:

```yaml
client_auth:
  token_env: MINDCTL_GATEWAY_TOKEN

providers:
  - id: openai
    base_url: https://api.openai.com
    api_key_env: OPENAI_API_KEY
```

### Codex

Codex uses Mindctl as a custom Responses provider, opts into first-party OpenAI
authentication, and supplies the separate Mindctl token through an
environment-backed header:

```toml
model_provider = "mindctl"

[model_providers.mindctl]
name = "Mindctl"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
requires_openai_auth = true
env_http_headers = { "X-Mindctl-Token" = "MINDCTL_GATEWAY_TOKEN" }
```

Codex supplies `Authorization: Bearer <access-token>` and, for the selected
workspace, `ChatGPT-Account-Id`. Codex owns token refresh and updates these
headers on later requests.

## Components and boundaries

### Request-scoped upstream authentication

A small provider-neutral internal package will define an immutable ChatGPT
credential containing only the access token and account ID. It will expose
context helpers for attaching and retrieving the credential. This avoids an
API-to-provider dependency and prevents credentials from entering canonical
inference or persistence types.

After Mindctl gateway authentication succeeds, middleware will parse the
incoming `Authorization` header as a non-empty Bearer credential and read a
non-empty `ChatGPT-Account-Id`. It will attach a credential only when both are
valid. Missing or malformed values are not errors at middleware time because a
request may be routed to a provider that uses another credential mode.

The middleware will not copy any other caller headers into provider state.

### Client authentication

The existing constant-time gateway-token comparison remains unchanged except
that it reads the configured header. The authenticated client ID remains the
only client identity exposed to the Responses handler and storage layer.

The gateway token and the OAuth bearer serve separate purposes:

- The configured gateway header authorizes use of Mindctl.
- `Authorization` and `ChatGPT-Account-Id` authorize the selected upstream
  ChatGPT account.

An invalid gateway token is rejected before classification, routing, storage,
or provider calls.

### Per-request eligibility

Application startup treats a configured OAuth-passthrough provider as
credential-capable so its models can populate the catalog and readiness can
succeed without `OPENAI_API_KEY`.

The Responses handler derives the authoritative credential-availability map
for every request:

- API-key providers retain their startup availability.
- OAuth-passthrough providers are available only when the request context
  contains both the bearer token and account ID.

The resulting map enters the existing router eligibility input. No routing
policy, tier, cost, capability, pinning, or escalation rule changes.

If an OAuth-passthrough provider is pinned but the current request lacks OAuth
credentials, the existing eligibility policy may select another eligible
configured provider. It will not substitute an API key for the OAuth provider.

### OpenAI provider adapter

The adapter receives an explicit authentication mode at construction:

- `api_key` sends its resolved static bearer to `<base_url>/v1/responses`.
- `chatgpt_oauth_passthrough` retrieves the request credential from context,
  sends it to `<base_url>/responses`, and adds `ChatGPT-Account-Id`.

OAuth requests also set Mindctl-owned `originator` and `User-Agent` headers.
Caller-provided values for those headers are ignored. The adapter never
forwards arbitrary inbound headers.

The existing no-redirect client behavior applies to both modes, ensuring a
bearer credential cannot follow a redirect to another origin. Both streaming
and non-streaming requests use the same request-construction and
authentication path.

## Request flow

1. Codex refreshes its ChatGPT OAuth session when required.
2. Codex sends a Responses request to Mindctl with its OAuth bearer, ChatGPT
   account ID, and the separate environment-backed Mindctl gateway token.
3. Mindctl validates the gateway token using the configured header.
4. Middleware captures a valid OAuth credential in request context only.
5. The Responses handler derives per-request provider credential availability.
6. The existing classifier and deterministic policy select an eligible model.
7. If the ChatGPT provider is selected, the OpenAI adapter calls the ChatGPT
   Codex Responses endpoint using only the captured bearer and account ID.
8. Mindctl normalizes and returns the response through its existing Responses
   and SSE paths.

## Error handling and security

- Missing or malformed OAuth headers make the OAuth provider ineligible. They
  do not trigger an upstream request.
- A missing or invalid gateway token returns the existing unauthorized error.
- ChatGPT `401` and `403` responses map to the existing sanitized provider
  authentication error.
- Provider response bodies and credentials never appear in client errors.
- Authentication failures do not cause API-key fallback within the same
  provider.
- Redirects remain disabled.
- Tokens and account IDs are not written to SQLite, encrypted transcript
  content, telemetry, routing decisions, or logs.
- ChatGPT subscription access remains subject to model eligibility, workspace,
  rate, and usage limits enforced by the upstream account.

## Compatibility

- Omitted `client_auth.header` continues to mean `Authorization`.
- Omitted `providers[].auth` continues to mean `api_key`.
- Existing API-key URLs and `/v1/responses` path construction remain
  unchanged.
- Existing Anthropic and Gemini behavior is unchanged.
- The incoming Mindctl Responses endpoint remains `/v1/responses`.

## Testing

Tests will be written before production changes and will cover:

1. Strict configuration decoding and validation:
   - accepted default API-key configuration;
   - accepted OAuth-passthrough configuration without `OPENAI_API_KEY`;
   - unknown auth modes;
   - missing or forbidden API-key references;
   - invalid or conflicting gateway-auth headers;
   - OAuth mode on unsupported providers.
2. Authentication middleware:
   - gateway authentication from the configured header;
   - preservation and request-scoped capture of the OAuth bearer and account
     ID;
   - missing or malformed OAuth inputs;
   - no credential exposure through the authenticated client identity.
3. Responses and routing integration:
   - OAuth provider eligibility when both headers are present;
   - ineligibility when either header is absent;
   - unchanged API-key availability.
4. OpenAI adapter behavior:
   - API-key endpoint and headers remain unchanged;
   - OAuth streaming and non-streaming requests use `/responses`;
   - only the bearer, account ID, and Mindctl-owned client headers are sent;
   - missing context credentials fail before network I/O;
   - redirects remain rejected;
   - upstream authentication failures remain sanitized.
5. Application-level behavior:
   - startup and a complete OAuth request succeed without
     `OPENAI_API_KEY`;
   - API-key mode continues to work.

Final verification will run inside the repository's devenv:

```sh
devenv shell -- go test ./...
devenv shell -- go test -race ./...
devenv shell -- go vet ./...
NX_SOCKET_DIR=/tmp/mindctl-nx devenv shell -- pnpm nx sync
```

## Documentation

`config.example.yaml` will show the OAuth-passthrough setup and explain the
API-key alternative. The README will include the corresponding Codex
`config.toml` example, environment-variable requirement, credential ownership,
and subscription-limit caveat.

## References

- [OpenAI Codex authentication storage](https://github.com/openai/codex/blob/main/codex-rs/login/src/auth/storage.rs)
- [OpenAI Codex provider configuration](https://github.com/openai/codex/blob/main/codex-rs/model-provider-info/src/lib.rs)
- [OpenAI Codex backend client headers](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client.rs)
