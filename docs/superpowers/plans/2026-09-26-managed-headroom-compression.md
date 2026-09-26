# Managed Headroom Compression Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one Mindctl configuration option enable locally managed Headroom compression across Homebrew, npm, native binaries, and the container image without ever forwarding uncompressed input on compression failure.

**Architecture:** Mindctl selects a model, then a provider-neutral compressor rewrites only round-trip-safe text in a copy of the canonical inference request. A supervised loopback Headroom process handles `/v1/compress`; a pinned, checked runtime installs itself in a private cache on first use. Mindctl retains the original transcript, upstream authentication, streaming, and provider usage accounting.

**Tech Stack:** Go, Headroom `/v1/compress`, pinned Python/uv and dependency lock, GoReleaser, npm launcher, Docker, existing `devenv` and Go tests.

**Spec:** `docs/superpowers/specs/2026-09-26-managed-headroom-compression-design.md`

## Global Constraints

- `headroom.enabled` defaults to `false`; optional `headroom.mode` is `cache` (default) or `token`. Disabling an outage requires editing config and restarting; never silently fall back.
- Headroom never receives gateway/provider/OAuth credentials, forwards no inference request, and exposes no non-loopback listener. Disable its outbound usage beacon and content logging.
- Enabled but unavailable, skipped, or invalid compression returns sanitized HTTP 503 with code `headroom_unavailable` **before any provider request or visible SSE event**. A valid zero-saving result is not an error; no eligible text still requires live sidecar readiness.
- Leave instructions, user text, images, JSON types, tools, opaque continuation, original durable transcript, and unrelated provider paths unchanged. Use marker-free compression, with session affinity per conversation/provider/model; no CCR or redrives.
- Native release targets are macOS amd64/arm64, Linux amd64/arm64, and Windows amd64; first use may download pinned assets. No upstream package edits, external Home Manager changes, generated Homebrew formula edits, publication, or commits without an explicit request.
- Run repository build/test commands with `devenv shell -- ...` because `devenv.nix` exists. An isolated real-Headroom contract test is mandatory before claiming provider coverage; mocked tests alone do not prove upstream billing savings.

## Review Focus

- Two Mindctl instances starting with an empty shared cache: exactly one complete verified installation becomes executable; neither ever runs partial files (Task 3).
- A local process occupies the preferred port or responds without the ephemeral token: Mindctl does not trust or send content to it (Task 4).
- Headroom returns HTTP 200 with `compression_skipped: true`: reject, even though its `messages` look structurally valid (Task 2).
- A turn has only images or user text when the managed child has died: return 503 rather than passing it upstream (Task 5).
- A continuation changes the pinned provider/model while preserving call IDs and encrypted state: use a distinct session, never leak opaque provider data or mutate stored originals (Tasks 2 and 5).

---

### Task 1: Opt-in Configuration and Stable Error Contract

**Files:** Modify `internal/config/types.go`, `internal/config/load.go`, `internal/config/load_test.go`, `internal/api/errors.go`, `internal/api/messages_test.go`, `internal/api/responses_test.go`, `config.example.yaml`; create `internal/headroom/errors.go`.

**Interfaces:** Produce `config.HeadroomConfig{Enabled bool, Mode string}` and `Config.Headroom`; `HeadroomConfig.ModeValue() string` returns `"cache"` for empty mode. Produce sentinel `headroom.ErrUnavailable`; the API's shared `errorDetail` maps it to 503, `server_error`, `headroom_unavailable`, with a fixed message instructing `headroom.enabled: false` and restart. Reuse `writeMessagesError` for Anthropic format.

- [ ] Write `TestHeadroomConfigDefaultsAndValidation`: missing section is disabled/cache; enabled cache and token succeed; invalid mode and unknown fields fail; the shipped example remains readable. Write `TestHeadroomUnavailableResponseFormats` for both JSON formats; assert 503, the stable code, the config instruction, and no raw error or secret.
- [ ] Run `devenv shell -- go test ./internal/config ./internal/api -run 'TestHeadroomConfig|TestHeadroomUnavailable' -count=1`; expect RED (new types/error classification absent).
- [ ] Implement the config and error interfaces above, add a disabled example. Do not alter existing provider/authentication validation.
- [ ] Rerun the focused command; expect PASS, including an unchanged-path control with Headroom absent.

### Task 2: Safe Canonical-Field Compression Client

**Files:** Create `internal/headroom/client.go`, `internal/headroom/client_test.go`, `internal/headroom/mapping.go`, `internal/headroom/mapping_test.go`; reuse `internal/inference/types.go` without changing its wire contract.

**Interfaces:** Produce `headroom.Metrics{TokensBefore, TokensAfter, TokensSaved int64}` and `headroom.Compressor` with `Compress(ctx context.Context, model domain.Model, conversationID, providerID string, request inference.Request) (inference.Request, Metrics, error)`. Produce `NewClient(baseURL, token string, httpClient *http.Client) *Client`, `(*Client).Ready(ctx context.Context) error`, and `(*Client).Compress(...)` satisfying `Compressor`. The model argument supplies the upstream model ID. `mode` is **not** a `/v1/compress` `config.mode` value: Task 4 sets the proxy's `--mode` at launch. For each provider/model, derive an opaque session ID from the conversation ID; send `config.session_id`, not `compress_user_messages` or CCR mode.

- [ ] Write `TestClientCompressPreservesCanonicalFields`: fake `/v1/compress` receives only the real selected model and supported Anthropic/OpenAI-chat-shaped messages; replies with changed assistant/tool-result text. Assert only those text fields change; item ordering, call IDs, typed/opaque fields, user text, JSON objects, images, instructions, and the original request remain byte-identical. Assert a provider/model change yields a different session ID, and neither provider credentials nor a full Responses body go to Headroom.
- [ ] Write `TestClientRejectsUnsafeCompression`: for HTTP 200 `compression_skipped`, missing/malformed response, changed roles/count/IDs, invalid token metrics, oversized body, 400/401/404/503, and timeout, assert `errors.Is(err, headroom.ErrUnavailable)` and no raw response text in `err.Error()`; accept a structurally valid result with zero savings. Include unknown item types and tool-output JSON objects as unchanged rather than lossy-converted.
- [ ] Run `devenv shell -- go test ./internal/headroom -run 'TestClientCompress|TestClientRejects' -count=1`; expect RED.
- [ ] Implement a size-bounded, redirect-disabled HTTP client with marker-free `/v1/compress` requests, ephemeral-token header, structural output checks, and defensive deep copies. Map only field classes proven safe by tests; exclude user-message rewrites and JSON non-string tool results. `Ready` uses a bounded authenticated empty-message probe.
- [ ] Rerun focused tests; expect PASS. Record any Headroom wire-format assumption to verify against the real pinned service in Task 4.

### Task 3: Verified First-Use Runtime Provisioning

**Files:** Create `internal/headroom/bootstrap.go`, `internal/headroom/bootstrap_test.go`, `internal/headroom/assets/pyproject.toml`, `internal/headroom/assets/uv.lock`, `internal/headroom/assets/artifacts.json`; modify `internal/headroom/client.go` only if an embedded asset contract requires it.

**Interfaces:** Produce `headroom.NewProvisioner(cacheDir string, client *http.Client, runner headroom.Runner) *Provisioner` and `(*Provisioner).Ensure(ctx context.Context) (executablePath string, err error)`. `Runner` is `func(context.Context, string, []string, []string) error` for a pinned executable, arguments, and explicit environment; the production implementation invokes the pinned uv utility to install managed Python and `headroom-ai[proxy]` into a private locked environment. Embed the dependency lock and platform artifact checksums in the Go binary, so no package manager, Python, or companion configuration is required at install time. Start with Headroom `0.39.0`, then pin a verified compatible Python 3.13 patch and uv release and hash every downloaded executable/archive for each release target; regenerate the lock against those exact pins before merging. Export `Provisioning` as the `Ensure(context.Context) (string, error)` interface for Task 4.

- [ ] Write `TestProvisionerVerifiedAtomicCache` and `TestProvisionerEmbeddedAssets`: fake downloads and runner assert hash rejection never executes a file, interrupted install is discarded, warm start makes no network call, concurrent instances share one completed version, a read-only cache produces a sanitized error, and embedded manifest/lock cover both macOS arches, both Linux arches and Windows amd64.
- [ ] Run `devenv shell -- go test ./internal/headroom -run TestProvisioner -count=1`; expect RED.
- [ ] Implement `Ensure` with OS cache resolution, cross-process lock, temporary staging plus atomic publish, checksum verification, per-version directory, bounded downloads, and no dynamic installation of unpinned dependencies. Keep model assets in the private Headroom cache; pin/check their revisions or hashes when supported. If the pinned service cannot provide verifiable model provenance on a target, report that release blocker rather than silently weakening the approved spec.
- [ ] Rerun focused test and `devenv shell -- go test ./internal/headroom -race -count=1`; expect PASS.

### Task 4: Child Supervision and Real Headroom Contract

**Files:** Create `internal/headroom/manager.go`, `internal/headroom/manager_test.go`, `internal/headroom/integration_test.go`; modify `internal/headroom/client.go` only where a real service proves the assumed wire contract differs.

**Interfaces:** Produce `headroom.NewManager(cfg config.HeadroomConfig, provisioner Provisioning, client *http.Client, runner ProcessRunner) *Manager`, `(*Manager).Start(ctx context.Context) error`, `(*Manager).Compress(...)` satisfying `Compressor`, and idempotent `(*Manager).Close() error`. `ProcessRunner.Start(context.Context, string, []string, []string) (Process, error)` returns a `Process` with `Wait() error` and `Kill() error`; the production runner uses `os/exec`. `Start` records a failed bootstrap/launch as an unavailable state without exposing content; the caller may keep the HTTP gateway running to answer 503s. `Compress` verifies child liveness and authenticated readiness even if no text is eligible. Use one child worker and one loopback-only per-instance port with a random token; disable beacon/content logging and pass an environment allowlist with no provider keys. Keep failures terminal until operator restart or config disablement; do not infer a healthy endpoint from port availability alone.

- [ ] Write `TestManagerSupervisionAndIsolation`: fake provisioner/process and listener assert bound startup/shutdown, no orphan child, startup failure then `Compress` unavailable, killed child rejects even an image-only request, occupied port cannot capture traffic, wrong token fails closed, and secrets in Mindctl's parent environment never appear in the child's environment.
- [ ] Run `devenv shell -- go test ./internal/headroom -run TestManager -count=1`; expect RED; implement `Manager`, then rerun and expect PASS.
- [ ] Add `TestRealHeadroomCompressionContract` using the locked service and no provider credentials: verify text/tool-result changes survive round-trip, `config.session_id` replays a stable prefix, unknown/unsupported shapes are left untouched, and both proxy modes are accepted at launch. Put it behind an explicit local/CI integration opt-in so ordinary unit tests never fetch model weights.
- [ ] Run `devenv shell -- env MINDCTL_HEADROOM_INTEGRATION=1 go test ./internal/headroom -run TestRealHeadroomCompressionContract -count=1` against a fresh temporary cache, then repeat against a warm cache; expect PASS on a supported local target. If Headroom alters protected structure or cannot produce a safe mapping, stop and revise this design with the user; do not ship a guessed mapping.

### Task 5: Executor, App, and Pre-SSE Fail-Closed Integration

**Files:** Modify `internal/executor/service.go`, `internal/executor/stream.go`, `internal/executor/service_test.go`, `internal/executor/stream_test.go`, `internal/app/app.go`, `internal/app/app_test.go`, `internal/api/messages_test.go`, `internal/api/responses_test.go`.

**Interfaces:** Produce `executor.NewWithCompressor(classifier classifier.Classifier, policy *router.Policy, providers *provider.Registry, conversations conversation.Service, compressor headroom.Compressor, logger *slog.Logger) *Service`; existing `New` and `NewWithLogger` delegate with `nil` compressor. In `app.New`, create/start the manager only if enabled; retain it for shutdown. Immediately before `adapter.Execute`/`adapter.Stream`, call `Compress(ctx, model, turn.ConversationID, candidate.Provider, providerScopedRequest(request, candidate.Provider))`. A compression error ends the attempt without trying another model. Move `writer.Start` behind compression but preserve its ordering relative to the provider stream on success.

- [ ] Write `TestExecuteHeadroomFailurePreventsProviderCall`, `TestStreamHeadroomFailurePreventsSSE`, and `TestHeadroomRouteFailureBeforeSSE`: disabled path unchanged; enabled success forwards only the safe compressed copy, commits the original/result, and reports true provider usage; unavailable/skipped/child-dead errors yield 503 (both handlers), zero provider calls and no SSE bytes. Include image-only/user-only input and a resumed turn that changes provider/model without transferring opaque continuation data.
- [ ] Run `devenv shell -- go test ./internal/executor ./internal/app ./internal/api -run 'TestExecuteHeadroom|TestStreamHeadroom|TestHeadroomRoute' -count=1`; expect RED.
- [ ] Wire the constructor and app ownership, start in degraded fail-closed mode when bootstrap fails, and preserve existing retry-before-visible-output behavior only for actual provider errors. On compression failure mark the attempt failed with no provider request ID, do not escalate/fallback, and emit content-safe diagnostics and Headroom estimate metrics separately from provider billing.
- [ ] Rerun focused tests, then `devenv shell -- go test ./internal/executor ./internal/app ./internal/api -count=1`; expect PASS.

### Task 6: Native Packaging and Operator Documentation

**Files:** Modify `README.md`, `packages/npm/README.md`, `packages/npm/test/stage.test.mjs`, `packages/npm/test/launch.test.mjs`, `.github/workflows/release.yml` only where new asset paths must trigger release checks; inspect `.goreleaser.yaml`, `packages/npm/scripts/stage.mjs`, and `HomebrewFormula/mindctl.rb` but do not hand-edit the generated formula.

**Interfaces:** `mindctl` remains the only installed executable for Homebrew, npm and native archives; Task 3's embedded manifest/lock supplies the first-run sidecar on every target. Document cold-start network/cache requirements, `headroom.enabled`/`mode`, fail-closed 503 and the exact disable-and-restart action, and how to avoid double compression when Headroom already fronts Mindctl.

- [ ] Write `TestHeadroomNativePayloadStaging` in `packages/npm/test/stage.test.mjs`: stage all five target archives and assert their only runtime executable is the Mindctl binary; the existing npm launcher remains, with no second Python/Headroom installer. Add `TestHeadroomLauncherEnvironment` asserting npm launches the Go executable without altering provider credential environment; Task 3's embedded-asset test proves the Go binary carries the manifest/lock.
- [ ] Run `devenv shell -- node --test packages/npm/test/stage.test.mjs packages/npm/test/launch.test.mjs`; expect baseline PASS for unchanged package behavior, then exercise the new cold-start coverage in Task 7.
- [ ] Update native archive/staging code only if the Go embed is not present in every archive; retain the generated Homebrew formula and existing npm target matrix. Add docs and CI paths for any added asset directory. Confirm Homebrew's archive binary and native binaries use the same bootstrap entrypoint, with no `headroom` prerequisite.
- [ ] Rerun the node tests; run `devenv shell -- goreleaser release --snapshot --clean` and inspect the five produced archives; expect all target binaries and no publication.

### Task 7: Container Path and Cross-Platform Proof

**Files:** Modify `Dockerfile`, `.github/workflows/release.yml` if necessary; create `.github/workflows/headroom-compat.yml` for a narrowly scoped PR matrix, `tools/scripts/src/headroom-native-smoke.mjs`, and `tools/scripts/src/headroom-container-smoke.mjs`; update `README.md` only for container-specific setup.

**Interfaces:** The OCI image runs the same manager, with a Python-capable runtime and writable private cache; it must not rely on a separately configured container, nor break the default-disabled startup. The workflow exercises real pinned Headroom cold and warm starts for macOS amd64/arm64, Linux amd64/arm64 and Windows amd64, independently of mock-only Go tests. `headroom-native-smoke.mjs` accepts a path to a packaged `mindctl` executable or npm launcher and drives a fixture config plus local fake provider; it asserts enabled first-run compression and cached restart with no external provider bill. Use the same archived executable that Homebrew installs on macOS, rather than editing its generated formula. Do not touch external storm-ops workflows or deploy/publish in validation.

- [ ] Add `TestHeadroomContainerColdStart` in `tools/scripts/src/headroom-container-smoke.mjs`: with disabled config it starts without Headroom download; with enabled config and writable cache it provisions, compresses, restarts warm, and returns 503 with no upstream request when the child fails.
- [ ] Run `devenv shell -- docker build -t mindctl-headroom:local .` followed by `devenv shell -- node tools/scripts/src/headroom-container-smoke.mjs` on a supported Docker host; expect RED before changing the distroless runtime, or record host Docker unavailability rather than claiming proof.
- [ ] Write `TestHeadroomNativeColdAndWarmStart` in `tools/scripts/src/headroom-native-smoke.mjs` for a packaged executable and the npm launcher: assert enabled compression reaches a local fake upstream, the warm run uses the cache, and a killed child yields 503/zero upstream calls. Run it for the local snapshot archive via `devenv shell -- node tools/scripts/src/headroom-native-smoke.mjs <packaged-binary>`; expect RED until bootstrap and integration work.
- [ ] Supply a minimal Python-capable runtime in `Dockerfile` with non-root cache ownership and preserved Go binary entrypoint; add a PR OS/architecture matrix whose supported runners invoke the Task 4 integration test and native smoke for each packaged archive and npm launcher. Keep default-disabled image behavior unchanged.
- [ ] Rerun container smoke where Docker is available, then `devenv shell -- go test ./...`, `devenv shell -- node --test packages/npm/test/*.test.mjs`, `git diff --check`, and check the matrix and manifest cover all five shipped targets. Report separately: mock tests, real local Headroom, per-OS CI, container, and actual provider billing (not established without live inference).
