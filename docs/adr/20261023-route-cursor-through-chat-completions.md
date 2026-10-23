# Route Cursor through Chat Completions

- Status: accepted
- Deciders: sullivanpj
- Date: 2026-10-23

Technical Story: Route Cursor requests through mindctl the way Claude Code and Codex already are.

## Context and Problem Statement

Claude Code and Codex are pointed at the local gateway by editing their own config, because each client already speaks an API the gateway serves. Cursor's IDE can send OpenAI Chat Completions to a base URL the user sets, and that is the only supported way to move its model traffic. The CLI and Cursor's own models speak Cursor's private agent protocol and have no base-URL setting.

## Decision Drivers

- Keep the gateway token out of Cursor's database, matching Claude and Codex.
- Leave `Authorization` free on `/v1/responses` and `/v1/messages` so those routes can still carry subscription credentials.
- Cover the agent, which means tool calls, not only plain text.

## Considered Options

- Add `POST /v1/chat/completions` and a Cursor harness that edits `state.vscdb`.
- Teach the gateway Cursor's Connect protocol and point `agent --endpoint` at it.
- Document the Settings screen and skip a harness command.

## Decision Outcome

Chosen option: "Add `POST /v1/chat/completions` and a Cursor harness that edits `state.vscdb`", because that is the same shape as the Claude and Codex harnesses and it uses an API Cursor already sends. The Connect protocol was set aside: it is an agent runtime, not a model API, and the CLI ignores IDE keys. A docs-only setup would leave Cursor out of `mindctl on`, `off`, `uninstall`, and `status`.

Chat Completions authenticates `Authorization: Bearer` with the gateway token. The other routes keep the configured client header. The harness writes `openAIBaseUrl`, `useOpenAIKey`, and a `mindctl-auto` model. It does not write the API key. Setup refuses to run while the Cursor process is open.

### Positive Consequences

- `mindctl --cursor` joins the existing harness menu.
- Local Chat and Agent requests that use the OpenAI key can be routed, classified, and recorded like Codex requests.

### Negative Consequences

- Tab, Auto, Cloud Agents, the Cursor CLI, and Cursor's built-in models still go to Cursor.
- Sampling fields such as `temperature` are ignored. A named `tool_choice` object is rejected.
- The user pastes the gateway token into Cursor's OpenAI API key field. That value is the gateway credential, not an upstream provider key.
- Cursor must be quit before setup, or it overwrites `state.vscdb` from memory.
