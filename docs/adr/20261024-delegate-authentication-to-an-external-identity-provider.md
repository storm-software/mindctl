# Delegate authentication to an external identity provider

- Status: accepted
- Deciders: sullivanpj
- Date: 2026-10-24

Technical Story: Integrate an identity provider such as Better Auth so mindctl authentication is handled outside the gateway.

## Context and Problem Statement

The gateway routes and the management API each compared requests against one static token taken from an environment variable. Every gateway caller shared the client ID `configured-client`, so conversations, routing sessions, and usage could not be told apart by person. Storage already scopes those records by client ID. How should mindctl accept identities from an external identity provider (IdP) without becoming an issuer itself?

## Decision Drivers

- Claude Code, Codex, and Cursor send a static credential for the whole session (`ANTHROPIC_CUSTOM_HEADERS`, `env_http_headers`, or a pasted API key). They cannot refresh a short-lived token.
- `Authorization` already carries the upstream subscription credential on the Claude and Codex routes, so the gateway credential stays in `X-Mindctl-Token`. Only Cursor's Chat Completions route uses `Authorization: Bearer`.
- Work with any standards-compliant IdP, not only Better Auth.
- Keep existing static-token deployments working unchanged.

## Considered Options

- Standard protocols in mindctl: RFC 7662 introspection for gateway credentials, and OpenID Connect with PKCE plus JWT access tokens for the dashboard.
- Short-lived IdP JWTs on the gateway routes.
- A forward-auth proxy (oauth2-proxy, Pomerium) in front of mindctl.
- Better Auth's `enableSessionForAPIKeys` with its `get-session` endpoint.
- Mindctl minting its own gateway tokens after an IdP login.

## Decision Outcome

Chosen option: "Standard protocols in mindctl", because it fits the static-header constraint and any compliant IdP works. Gateway clients send long-lived, revocable per-user keys, which mindctl checks by introspection. Active answers are cached for `cache_ttl`, one minute by default. The dashboard signs in as a public PKCE client. The management API checks JWT access tokens against the issuer's JWKS with `github.com/coreos/go-oidc/v3`, requiring `audience` in `aud` and `required_scope` in `scope`. The audience must differ from the client ID, so ID tokens are refused. Static tokens remain an optional break-glass credential, checked first.

Short-lived JWTs would expire mid-session. A forward-auth proxy conflicts with the `Authorization` passthrough, and mindctl would have to trust a header set by the proxy. Better Auth's own documentation discourages `enableSessionForAPIKeys`, and it would tie mindctl to Better Auth. Minting tokens would make mindctl an issuer again.

### Positive Consequences

- Requests authenticated by the IdP use client ID `idp:<sub>`, so each user has their own conversations, routing sessions, and usage.
- Keys are issued and revoked in the IdP, with no gateway restart.
- If the IdP is unreachable, mindctl returns 503 `auth_unavailable`, so clients retry instead of discarding their key.
- The dashboard token moved from `localStorage` to `sessionStorage`.

### Negative Consequences

- New dependencies: `go-oidc/v3`, plus `go-jose/v4` and `golang.org/x/oauth2` indirectly.
- A revoked key keeps working for up to `cache_ttl`.
- Better Auth's `verifyApiKey` is server-only, so Better Auth deployments add a small introspection route that wraps it (shown in the README).
- Conversations made with the static token stay under `configured-client`; they do not move to users.
- The dashboard's code exchange is relayed through `POST /dashboard/oidc/token` so the IdP needs no CORS policy. Single sign-on in the browser needs HTTPS or localhost for Web Crypto.
- The scope check reads the RFC 9068 `scope` claim only, not Okta's or Entra ID's `scp`.
