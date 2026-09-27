# Session Affinity and Cache-Aware Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep a Claude Code or Codex session on one routed model across turns so the provider prompt cache stays warm. Pick that model by what it will cost over the next few turns with caching, not by a single uncached turn with a worst-case output.

**Architecture:**

- A content-free `routing_sessions` table stores pin, floor and last-turn usage per client session.
- It is keyed by an opaque digest of the client's own session ID plus its first user message.
- Each stateless Messages or Responses request still creates its own conversation and transcript. That conversation is *seeded* from the session's pin and floor, and every pin, floor or usage change on the conversation is written back to the session in the same SQLite transaction.
- The router then scores candidates with an expected-output, cache-hit-ratio, multi-turn horizon estimate. This matters most on the turn that establishes the session pin.

**Tech Stack:** Go, SQLite migrations, and the existing `router`, `executor`, `conversation`, `storage/sqlite` and `api` packages. There are no new dependencies.

**Spec:** There is no separate design spec; the Design section below is authoritative. The mechanisms are adapted from weave-os/router `main@62a5087`, which is Apache-2.0: `internal/proxy/session_key.go`, `internal/requestcontext/conversation.go`, `internal/router/planner/policy.go` and `internal/router/catalog/cost.go`.

## Design

### Why

**Every Claude Code request starts from scratch.** `conversation.Start` creates a new random conversation unless `previous_response_id` resolves. The Messages decoder never sets it, and Codex with `store: false` never sends it. So every turn:

- starts with no pin;
- calls the classifier again;
- may land on a different model than the previous turn;
- if it does, throws away the upstream prompt cache and pays full input price, plus the Anthropic cache-write premium, on the whole history.

**The cost estimate also mis-ranks models.** `router/cost.go` assumes zero cache hits and prices the full `max_tokens` (32k–64k for Claude Code) as output. That favours models with cheap output and cheap uncached input over models whose cached-read rate makes them cheaper over a session.

### Session identity (Task 2)

**Scope.** Only `mindctl-auto` requests without `previous_response_id` get a session key.

- Explicit-model requests never read or write session state. Claude Code's Haiku background calls must not inherit, or be rejected by, the main loop's floor.
- `previous_response_id` keeps its existing conversation semantics.

**Client session ID precedence** (matches upstream):

1. For Messages, a JSON `metadata.user_id` with a `session_id`, `sessionId`, `conversation_id` or `conversationId` field.
2. `X-Mindctl-Session-Id`.
3. `X-Claude-Code-Session-Id`.
4. `Session-Id` (Codex).

The value must be a single header value with no surrounding whitespace. It must be 16–128 printable ASCII characters. Anything else yields no session.

**Key.** `hex(sha256("mindctl-session-v1\x00" + clientID + "\x00" + clientSessionID + "\x00" + firstUserText))`.

- `firstUserText` is the `Text` of the first `message` item with role `user`.
- Instructions and system text are excluded, because Claude Code rewrites them every turn (cwd, git status, timestamps).
- The first user message separates Task/Explore sub-agents, which share the parent's session ID. It also starts a new session after client-side compaction rewrites history. That is intended: the compacted prefix is cold anyway.
- There is no content-only fallback. Hashing a low-entropy first message without a random session ID would let the stored digest be dictionary-checked. Requests without a usable client session ID behave exactly as today.

### Session state is separate from transcript (Task 3)

Messages and stateless Responses callers resend the full history on every request. `executeDecision` sends `turn.TranscriptFor(provider)` upstream, which is every stored item in the conversation.

**Attaching new requests to one long-lived conversation would therefore send and store the history twice, with quadratic growth.** Instead:

- Every request keeps its own conversation and transcript, exactly as today.
- `routing_sessions` holds only non-content routing state:
  - pin provider, model and tier;
  - floor;
  - last-turn usage: uncached input, cache read, cache write, output;
  - `updated_at`.
- `CreateTurn` seeds the new conversation's pin and floor from a live session row. Existing conversation logic then works unchanged: `turnPin`, pin reuse in `Policy.Decide`, the ratchet checks in `BeginProviderAttempt` and `CommitConversationResult`, and `RaiseConversationFloor`.
- The same transactions write the result back to the session. `routing_sessions` rows follow the conversation rules: the floor only rises, and the pin is replaced only at an equal or higher tier.

**Idle expiry.** A session idle longer than `routing.session.idle_ttl` (default 1h, the longest Anthropic cache TTL) is treated as absent.

- Its cache is cold, so stickiness has no cost value, and the next turn is classified and scored afresh.
- Expired rows are deleted by retention maintenance whatever `sqlite.retention` is set to.
- Setting `idle_ttl: 0s` disables expiry.

### Re-classification (Task 5)

Today a compatible pin skips the classifier for good. Once sessions are sticky, that would lock a session to whatever tier its first message needed.

**Session-bound turns therefore re-run the classifier when the turn starts with a new user request.** A new user request means that no `function_call_output` item appears after the last `assistant` or `function_call` item, and at least one user `message` item does.

- The judgment can only raise the floor. The existing `Decide` keeps a compatible pin, or else switches upward. This matches upstream's tier-upgrade override.
- Tool-result continuations, which are most agent turns, keep the pin with no classifier call.
- For session-bound turns the classifier prompt is the instructions plus the user text of the *current* turn only, not every user message in the resent history.

### Cache-aware horizon cost (Tasks 6–7)

**Why mindctl diverges from upstream's planner.** Upstream's stay/switch planner mainly decides *lateral and downward* switches. mindctl already reuses every compatible pin, and never moves a pin or floor down within a live session. So the planner's stay/switch outcome is already built into mindctl's pin rules, and this plan does not port it.

**What is ported is the pricing.** The only times mindctl chooses among candidates in a session are the first turn, a forced upward switch, and the turn after idle expiry. In each case the chosen model becomes sticky, so it should be priced as sticky.

For a candidate with input price `I`, cached-read price `R`, cache-write price `W`, output price `O` and per-request price `P`, and a request with estimated input `n`, expected output `e`, horizon `H` and cache hit ratio `h`:

- `ExpectedTurnCost` (this turn):
  - Session-bound: `n·W + e·O + P`, because a cold model writes the full prefix.
  - Otherwise: `n·I + e·O + P` (today's formula, with `e` in place of `max_tokens`).
- `HorizonCost` (session-bound only): `H · (h·n·R + (1−h)·n·W + e·O + P)`.
- `ExpectedTotalCost = ExpectedTurnCost + HorizonCost + (1−prior)·failure_escalation_cost + latency_penalty`.
- `DirectCost` keeps today's worst-case formula (uncached input and full `max_tokens` output). `max_direct_cost_usd` still caps worst-case spend.
- `max_expected_cost_usd` applies to `ExpectedTurnCost + (1−prior)·failure + latency`, so the horizon never makes a request unroutable.

Inputs:

- `e`: the session's last observed output tokens, clamped to `[1, max_tokens]`. Without session usage it is `routing.expected_output_tokens`, and `0` keeps today's `max_tokens` behaviour.
- `h`: the session's last observed ratio `cache_read / (uncached + cache_read + cache_write)`. Before the first observation it is `routing.session.default_cache_hit_ratio`.
- `H`: `routing.session.horizon_turns`.
- Token classes use the savings package's provider-specific normalization (Anthropic reports three disjoint counts; other providers report cached tokens as a subset of input).

**Worked example** (`n = 100k`, `H = 3`, `h = 0.9`, equal output price):

| | Model A (`I = 3`, `R = 0.3`, `W = 3.75`) | Model B (`I = 2.5`, `R = 1.25`, `W = 2.5`) |
| --- | --- | --- |
| Today's ranking (uncached input) | $0.300 | $0.250, **wins** |
| Horizon input cost | $0.375 + 3 × $0.0645 = $0.569, **wins** | $0.250 + 3 × $0.1375 = $0.663 |

With `routing.session` disabled and `routing.expected_output_tokens: 0`, every score, ranking and rejection is byte-identical to today.

### Non-goals

- A summary handover when switching models.
- Automatic `prompt_cache_key` or `cache_control` injection.
- Background-turn detection.
- Loop breakers.
- Downgrading within a live session.
- Warm/cold modelling beyond idle expiry.
- Changes to savings accounting.

## Global Constraints

- **Content-free storage.** Session rows and conversation `session_key` hold digests and counts only. The raw client session ID and first user text are never persisted, logged or traced; traces carry an 8-character digest prefix at most.
- **Nothing new goes upstream.** `SessionKey` is never sent to any provider. Adapters build their own wire structs; add an assertion so a future `json.Marshal(inference.Request)` can't leak it.
- **Explicit and `previous_response_id` requests are unchanged.** Their behaviour, including storage writes, must not change.
- **Ratchet invariants are unchanged.** No conversation or session pin or floor ever decreases while the session is live.
- **Legacy mode is exact.** With sessions disabled and `expected_output_tokens: 0`, routing decisions are byte-identical to today. Prove it with the existing policy tests and fuzz corpus.
- **Commands.** Run repository commands with `devenv shell -- ...`. Do not commit unless the user asks.

## Review Focus

- **Two sub-agents under one Claude Code session ID** get different keys and cannot raise each other's floor (Task 2).
- **A Claude Code Haiku background request** (explicit dated model) carrying the main session's header neither reads nor writes session state, and is not rejected by the session floor (Tasks 2 and 5).
- **Concurrent requests on one session:**
  - two commits race: the final session floor is the maximum, and the pin never moves to a lower tier;
  - a request seeded before a floor raise cannot commit a result that lowers the session (Task 3).
- **Idle expiry:**
  - one second past `idle_ttl`, the next turn is unpinned and classified, and its seeded floor is the fallback rather than the old ratchet;
  - with `idle_ttl: 0s` the session never expires (Task 3).
- **A long tool loop** sees the classifier exactly once, on the user turn. A tool-result turn with trailing system-reminder text still counts as a continuation (Task 5).
- **Legacy mode** (sessions off, `expected_output_tokens: 0`) keeps every existing policy, executor and fuzz test passing unchanged (Task 6).
- **Worst-case caps:** `max_direct_cost_usd` still rejects a model whose worst-case `max_tokens` cost exceeds the cap, even when its expected cost is low (Task 6).

---

### Task 1: Configuration

**Files:** Modify `internal/config/types.go`, `internal/config/load.go`, `internal/config/load_test.go`, `config.example.yaml`.

**Interfaces:**

- New types:
  - `config.SessionConfig{Enabled *bool, IdleTTL time.Duration, HorizonTurns int, DefaultCacheHitRatio float64}`, added as `RoutingConfig.Session` with yaml key `session`.
  - `RoutingConfig.ExpectedOutputTokens int64`, yaml key `expected_output_tokens`.
- Methods:
  - `SessionConfig.EnabledValue() bool`: default `true`.
  - `IdleTTLValue() time.Duration`: default `1h` when omitted. An explicit `0s` means no expiry, so track presence the way `ModelConfig.UnmarshalYAML` does.
  - `HorizonTurnsValue() int`: default `3`.
  - `DefaultCacheHitRatioValue() float64`: default `0.8`.
- Validation:
  - negative TTL, horizon or expected output fails;
  - a horizon above `20` fails;
  - a ratio outside `[0,1]` fails.

Steps:

- [x] Write `TestRoutingSessionConfigDefaultsAndValidation` with these cases:
  - an omitted section gets the defaults;
  - an explicit `enabled: false` is honoured;
  - an explicit `idle_ttl: 0s` means no expiry, distinct from omitted;
  - each invalid bound fails with a field-named error;
  - `config.example.yaml` still loads.
- [x] Run `devenv shell -- go test ./internal/config -run TestRoutingSessionConfig -count=1`. Expect RED.
- [x] Implement it. In `config.example.yaml`, document the `routing.session` block and `expected_output_tokens: 0`, with comments on the legacy meaning of `0` and on idle expiry resetting the floor.
- [x] Rerun. Expect PASS.

### Task 2: Session identity at the API boundary

**Files:** Create `internal/api/session.go` and `internal/api/session_test.go`. Modify:

- `internal/inference/types.go`;
- `internal/api/messages.go` and `internal/api/messages_decode.go`, only to surface `metadata.user_id` from `AnthropicExtra` without removing it;
- `internal/api/responses.go`;
- `internal/api/messages_test.go` and `internal/api/responses_test.go`.

**Interfaces:**

- Add `SessionKey string` to `inference.Request`, tagged `json:"-"` and documented as gateway-internal.
- Produce `clientSessionID(r *http.Request, metadataUserID string) string` and `sessionKey(clientID, clientSessionID string, input []inference.Item) string`. `sessionKey` returns `""` when either ID is empty or there is no first user text.
- Both handlers set `Request.SessionKey` only when all of these hold:
  - `routing.session` is enabled (thread `SessionEnabled bool` through `ResponsesConfig`);
  - the model is `mindctl-auto`;
  - `PreviousResponseID` is empty.

Steps:

- [x] Write `TestSessionKeyDerivation` with these cases:
  - same ID and first message gives the same key;
  - a different first message (sub-agent) gives a different key;
  - a changed system prompt or instructions gives the same key;
  - a different client ID gives a different key;
  - a missing, whitespace-padded, repeated-conflicting, 15-character or 129-character ID gives no key;
  - there is no key without user text.
- [x] Write `TestClientSessionIDPrecedence`: JSON `metadata.user_id` `session_id` beats `X-Mindctl-Session-Id`, which beats `X-Claude-Code-Session-Id`, which beats `Session-Id`. A non-JSON `metadata.user_id` is ignored rather than guessed at.
- [x] Write `TestHandlersSetSessionKeyOnlyForAutomaticStatelessRequests` using the existing recording `ResponseExecutor` fake. Cover:
  - Messages `mindctl-auto` gets a key;
  - an explicit dated Haiku ID gets none;
  - Responses with `previous_response_id` gets none;
  - sessions disabled gets none;
  - `metadata` is still forwarded in `AnthropicExtra`.
- [x] Add `TestAdaptersNeverForwardSessionKey` in each provider package's existing request-encoding test: build a request with `SessionKey` set and assert the outbound body does not contain the value.
- [x] Run `devenv shell -- go test ./internal/api ./internal/provider/... -run 'TestSessionKey|TestClientSessionID|TestHandlersSetSessionKey|TestAdaptersNeverForwardSessionKey' -count=1`. Expect RED. Implement, then rerun. Expect PASS.

### Task 3: Durable session state

**Files:** Create `migrations/007_routing_sessions.sql` and `internal/storage/sqlite/session_test.go`. Modify `internal/storage/storage.go`, `internal/storage/sqlite/repository.go`, `internal/app/retention.go`, `internal/app/app.go` and `internal/app/app_test.go`.

**Migration:**

```sql
-- Content-free routing affinity for stateless clients. session_key is a
-- digest; raw client session IDs and prompt text are never stored.
CREATE TABLE routing_sessions (
    client_id TEXT NOT NULL,
    session_key TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    pin_provider TEXT,
    pin_model_id TEXT,
    pin_tier INTEGER CHECK (pin_tier IS NULL OR (pin_tier >= 0 AND pin_tier <= 6)),
    escalation_floor INTEGER NOT NULL CHECK (escalation_floor >= 0 AND escalation_floor <= 6),
    last_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_input_tokens >= 0),
    last_cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_cache_read_tokens >= 0),
    last_cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_cache_write_tokens >= 0),
    last_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_output_tokens >= 0),
    last_usage_provider TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (client_id, session_key)
);
CREATE INDEX routing_sessions_updated_at ON routing_sessions(updated_at);
ALTER TABLE conversations ADD COLUMN session_key TEXT;
```

**Interfaces:**

- New storage types:
  - `storage.SessionUsage{Provider string; UncachedInput, CacheRead, CacheWrite, Output int64}`;
  - `NewTurn.SessionKey string` and `NewTurn.SessionIdleTTL time.Duration`;
  - `ConversationRecord.SessionKey string` and `ConversationTurn.SessionUsage *storage.SessionUsage`.
- Add `DeleteExpiredSessions(ctx, idleTTL time.Duration, now time.Time) (int64, error)` to `storage.Repository`.
- Repository behaviour:
  - **`CreateTurn`**, when `SessionKey != ""`:
    - read the row;
    - if it is live (`idleTTL == 0 || updated_at >= now − idleTTL`), copy its pin and floor into the new conversation and its usage into the turn;
    - if it is expired, replace it with an empty row;
    - otherwise insert an empty row;
    - set `conversations.session_key`.
  - **`BeginProviderAttempt`** (initial pin), **`CommitConversationResult`** and **`RaiseConversationFloor`**: after the conversation update, when the conversation has a `session_key`, upsert the session in the same transaction:
    - `escalation_floor = MAX(existing, new)`;
    - replace the pin only when the new tier is at least the stored pin tier;
    - set `updated_at = now`;
    - on commit only, set the `last_*` usage columns.
  - **`GetConversationTurn`** returns `SessionKey` and the seeded `SessionUsage`.
- Retention: `retentionMaintenance` also calls `DeleteExpiredSessions` when the session idle TTL is above zero. In `app.go`, build the maintenance loop when either content retention or session expiry is finite.

Steps:

- [x] Write `TestRoutingSessionSeedAndWriteBack`: turn 1 commits a T2 pin and usage, and turn 2's conversation starts with that pin, floor and usage while its transcript holds only turn 2's input (no duplication).
- [x] Write `TestRoutingSessionRatchet` with these cases:
  - interleaved commits from two conversations on one session leave the maximum floor and never a lower-tier pin;
  - `RaiseConversationFloor` raises the session floor;
  - a lower-tier commit on a seeded conversation is rejected by the existing checks.
- [x] Write `TestRoutingSessionIdleExpiry`:
  - one nanosecond past the TTL, a new turn is unpinned with floor 0 and the row is reset;
  - `0` never expires;
  - `DeleteExpiredSessions` removes only expired rows and leaves conversations intact.
- [x] Write `TestRoutingSessionClientIsolation`: the same key under another client ID is a different session.
- [x] Write `TestRetentionMaintenanceExpiresSessionsWithUnlimitedContent`.
- [x] Run `devenv shell -- go test ./internal/storage/... ./internal/app -run 'TestRoutingSession|TestRetentionMaintenanceExpiresSessions' -count=1`. Expect RED. Implement, then rerun. Expect PASS, and the existing migration and repository tests still pass.

### Task 4: Conversation service

**Files:** Modify `internal/conversation/service.go`, `internal/conversation/service_test.go` and `internal/app/app.go`.

**Interfaces:**

- Produce `conversation.NewWithOptions(store, conversation.Options{SessionIdleTTL time.Duration}) Service`. `New` delegates with zero options.
- `Start` passes `request.SessionKey` and the TTL into `storage.NewTurn`.
- `Turn` gains:
  - `SessionKey string`;
  - `SessionUsage *storage.SessionUsage`;
  - `AffinityID() string`, which returns `SessionKey` when set and otherwise `ConversationID`.

Steps:

- [x] Write `TestStartSeedsSessionTurn` against the SQLite repository:
  - `SessionKey` and `SessionUsage` round-trip;
  - `Transcript` equals only the current input;
  - a request with `PreviousResponseID` ignores `SessionKey`, as defence in depth.
- [x] Run, implement, and rerun `devenv shell -- go test ./internal/conversation -count=1`. Expect RED, then PASS.

### Task 5: Executor session behaviour

**Files:** Modify `internal/executor/service.go`, `internal/executor/stream.go`, `internal/executor/debug.go`, `internal/executor/service_test.go` and `internal/executor/stream_test.go`.

**Interfaces:**

- Produce `newUserTurn(items []inference.Item) bool`, as defined in Design.
- In `Execute` and `streamDecision`, the compatible-pin short-circuit applies only when `turn.SessionKey == "" || !newUserTurn(in.Request.Input)`. Otherwise:
  - classify with `decisionInput.Pin` still set;
  - `FloorFromJudgment` already returns the pin floor, and `Decide` raises it from the judgment;
  - the compatible pin is kept, or the model switches upward.
- `classifierPrompt` uses only the current user turn's text when `turn.SessionKey != ""`.
- Headroom calls pass `turn.AffinityID()` in place of `turn.ConversationID`.
- Traces add `session_bound` (bool), `session_key_prefix` (8 characters) and `classifier_skipped_reason` (`pin_continuation` or `pin_no_session`).

Steps:

- [x] Write `TestSessionContinuationSkipsClassifier` using the existing classifier and provider spies. Across three sequential Messages turns on one session:
  - turn 1 classifies once;
  - turn 2 (tool results plus trailing system-reminder text) makes zero classifier calls and uses the same model;
  - turn 3 (new user text) classifies once.
- [x] Write `TestSessionUserTurnCanOnlyRaise`:
  - a turn-3 judgment below the pin keeps the pin;
  - a turn-3 judgment above the pin tier switches to a higher-tier model, and turn 4 is pinned to it.
- [x] Write `TestNonSessionBehaviourUnchanged`. Snapshot, for the existing fixtures with no session key: classifier call counts, decisions and stored records. They must stay identical.
- [x] Write stream variants (`TestStreamSession*`) of the three tests above. Include a pre-emission retry to an equal-or-stronger candidate that writes back to the session.
- [x] Write `TestHeadroomUsesSessionAffinity`: two turns in one session give the compressor the same affinity ID.
- [x] Run `devenv shell -- go test ./internal/executor -run 'TestSession|TestStreamSession|TestNonSessionBehaviourUnchanged|TestHeadroomUsesSessionAffinity' -count=1`. Expect RED. Implement, then rerun. Expect PASS.

### Task 6: Cache-aware horizon scoring

**Files:** Modify `internal/domain/features.go`, `internal/router/policy.go`, `internal/router/cost.go`, `internal/router/policy_test.go`, `internal/router/policy_fuzz_test.go`, `migrations/007_routing_sessions.sql` and `internal/storage/sqlite/repository.go`.

**Interfaces:**

- Add `RequestFeatures.ExpectedOutputTokens int64`. `Normalize` keeps it within `[0, MaxOutputTokens]` when both are valid.
- Add `router.SessionEstimate{HorizonTurns int; CacheHitRatio float64}` and `DecisionInput.Session *SessionEstimate`.
- Add `ExpectedTurnCost` and `HorizonCost` to `CandidateScore`. Attempts store scores inside `provider_attempts.decision_json`, but the replay API stores them column by column in `candidate_scores`, so migration 007 adds `expected_turn_cost` (backfilled from `direct_cost`, the old ranking cost) and `horizon_cost`.
- Replace `scoreCandidate`'s single `DirectCost` with the three quantities defined in Design:
  - `DirectCost` keeps its current formula;
  - `ExpectedTotalCost` uses `ExpectedTurnCost + HorizonCost`;
  - the `max_expected_cost_usd` check uses the non-horizon expected total.
- `validate` rejects:
  - a negative `ExpectedOutputTokens`;
  - a horizon outside `[0,20]`;
  - a ratio outside `[0,1]`;
  - a cache-write price that is not finite.

Steps:

- [x] Write `TestHorizonScoringPrefersCheaperCachedModel` using the worked example: with `Session` it selects A, and without it selects B.
- [x] Write `TestExpectedOutputReplacesMaxTokensInRanking`: with `max_tokens` 32000 and expected output 2000, a model with cheap input and dear output beats one with dear input and cheap output, and it loses under legacy settings.
- [x] Write `TestWorstCaseCapUnchanged`: `max_direct_cost_usd` rejects on the worst case even when the expected cost is small.
- [x] Write `TestLegacyScoresIdentical`: for every existing `policy_test.go` fixture, `Session == nil` and `ExpectedOutputTokens == 0` produce the same `Decision`, reasons included, as before the change. Extend the fuzz test to assert the same equivalence.
- [x] Run `devenv shell -- go test ./internal/router -count=1` and `devenv shell -- go test ./internal/router -run '^$' -fuzz FuzzPolicyNeverSelectsBelowFloor -fuzztime=30s`. Expect RED, then PASS.

### Task 7: Populate estimates from session usage

**Files:** Modify `internal/savings/savings.go`, `internal/savings/savings_test.go`, `internal/executor/service.go`, `internal/executor/stream.go`, `internal/executor/service_test.go` and `internal/app/app.go`.

**Interfaces:**

- Extract `savings.TokenClasses(provider string, usage inference.Usage) (uncached, cacheRead, cacheWrite, output int64)` from the existing cost formula, and use it in both savings and session usage.
- `CommitConversationResult` stores those classes as `SessionUsage` (Task 3 column wiring).
- Add `executor.Input.ExpectedOutputTokens int64` and `executor.Input.Session SessionSettings{HorizonTurns int; DefaultCacheHitRatio float64}`, supplied by the handlers from config through `api.ResponsesConfig`. Expected output sits outside `SessionSettings` because it also applies to non-session requests.
- `sessionFeatures` sets `ExpectedOutputTokens` from the session's last output, otherwise from `Input.ExpectedOutputTokens`.
- The decision input gets `Session` only when `turn.SessionKey != ""`, with `h` taken from `SessionUsage` or the default.
- `Decision.Reasons` gains `session horizon: turns=H hit_ratio=h (observed|default)`.

Steps:

- [x] Write `TestTokenClassesMatchSavingsPricing` for Anthropic, OpenAI and Gemini usage shapes. Assert that the savings totals are unchanged by the refactor.
- [x] Write `TestSessionEstimatesFromObservedUsage`:
  - turn 2 after a turn-1 result of 90k cache read, 10k write and 1.5k output uses `h = 0.9` and `e = 1500` in the recorded decision;
  - a first turn uses the configured default ratio;
  - a non-session request has `Session == nil`.
- [x] Run `devenv shell -- go test ./internal/savings ./internal/executor -count=1`. Expect RED. Implement, then rerun. Expect PASS.

### Task 8: End-to-end proof and documentation

**Files:** Modify `internal/app/app_test.go`, `README.md` (Configuration section) and `config.example.yaml`.

Steps:

- [x] Write `TestClaudeCodeSessionAffinityEndToEnd` in `internal/app` with a fake Anthropic upstream and a fake classifier:
  1. Three Messages requests share `X-Claude-Code-Session-Id` and a first user message: user, then tool result, then user. They are served by the same model. The classifier is called twice. The upstream sees each request body exactly once, with no duplicated history.
  2. A fourth request with a different first message is an independent session.
  3. A request with an explicit `claude-haiku-4-5-20251001` under the same header succeeds, and the session row is unchanged.
  4. After advancing the clock past `idle_ttl`, the next turn is classified from scratch.
- [x] Run `devenv shell -- go test ./... -count=1`, `devenv shell -- go test ./... -race -count=1` and `git diff --check`. Expect PASS.
- [ ] Manual check (real traffic, optional but required before claiming savings):
  - Run Claude Code against a debug-traced mindctl.
  - Confirm from traces that `session_bound=true` on main-loop turns and `false` on Haiku background calls.
  - Confirm `cache_read_input_tokens` is non-zero on turn 2 onward in `mindctl savings`.
  - **Also record which session-ID source Claude Code actually supplied** (`metadata.user_id` or `X-Claude-Code-Session-Id`). The precedence above follows upstream and has not been observed here.
- [x] Document in `README.md`:
  - what `routing.session` does and its privacy properties (digests only);
  - that idle expiry resets the floor;
  - the `X-Mindctl-Session-Id` header for other clients;
  - `expected_output_tokens`.

## Decisions

Confirmed by the user on 2026-09-27. Task 1 implements these defaults.

1. **Sessions on by default.** `routing.session.enabled` defaults to `true`. This changes routing for existing Claude Code and Codex users: fewer model switches and fewer classifier calls.
2. **Idle expiry resets the floor.** A session idle past `idle_ttl` (default 1h) starts fresh and may return on a lower tier. For stateless clients, this intentionally departs from "never downgrade a conversation", because the provider cache is already cold.
3. **`expected_output_tokens` defaults to `0`.** Non-session requests and a session's first turn keep the legacy `max_tokens` ranking. Later session turns use observed output.
4. **Starting constants.** `horizon_turns: 3` and `default_cache_hit_ratio: 0.8` are untuned starting values. Revisit them using `mindctl savings` data after Task 8's manual check.
