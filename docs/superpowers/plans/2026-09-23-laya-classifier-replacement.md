# Laya Classifier Replacement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Jev with the pinned Laya typed-decision classifier, offered as a supplied optional sidecar or a compatible remotely managed endpoint.

**Architecture:** Mindctl's Go process calls one authenticated `mindctl.classifier.v1` endpoint and validates a neutral `ClassifierJudgment`. The supplied FastAPI sidecar owns the six immutable Laya questions, pinned model artifact, lifecycle, and health endpoints; Docker Compose provides the local option without changing the gateway path.

**Tech Stack:** Go 1.26 (`net/http`, `httptest`, `gopkg.in/yaml.v3`, SQLite), Python 3.12, FastAPI, Pydantic, Uvicorn, Laya, PyTorch, uv, Docker Buildx, Docker Compose, GitHub Actions.

**Spec:** [`docs/superpowers/specs/2026-09-23-laya-classifier-design.md`](../specs/2026-09-23-laya-classifier-design.md)

## Global Constraints

- The Laya artifact is `convaiinnovations/laya` at revision `5e7b2b1b8ca2ecdd3f2322d94069c9b6ce7e844b`, variant `typed-decisions`.
- The only classifier protocol is authenticated `mindctl.classifier.v1` at `POST /v1/classify`; local and remote deployments use it unchanged.
- The sidecar owns exactly six fixed questions; requesters cannot select a checkpoint or submit questions.
- The gateway retains the existing deadline, retry, conversation-pin, and T4 safe-fallback behavior, and deterministic policy remains the sole model selector.
- Reject redirects, unknown JSON fields, invalid finite ranges, incomplete distributions, and responses that can disclose prompt or credential data.
- Do not fetch model weights in ordinary Go/Python tests or require GPU hardware in normal development/CI.
- Replace current Jev names everywhere: configuration, Go symbols, database table, routing copy, tests, README, and metrics/labels if present.
- Preserve unrelated existing changes to `Dockerfile` and `package.json`; inspect and integrate rather than overwrite them.

## Review Focus

- **Wrong or compromised remote service:** Task 3 tests reject the wrong schema version, incomplete classifier metadata, bad bearer credentials, redirects, and malformed answer payloads.
- **Startup race:** Task 2 tests `/readyz` remains 503 until the pinned model load completes while `/healthz` remains available.
- **Bad model output:** Task 3 tests every answer type, absent selected probability, non-normalized distributions, illegal labels, non-finite scores, and missing confidence.
- **Upgrade with historical records:** Task 5 migrates a database that already has `jev_judgments`, verifies the renamed table contains the original row, and reopens it through the repository.
- **No classifier available:** Task 4 tests an unreachable Laya endpoint still uses a compatible pin or T4 fallback and never attempts to invoke Jev.

---

## File Structure

| Path | Responsibility |
| --- | --- |
| `internal/domain/features.go` | Provider-neutral classifier judgment used by routing and storage. |
| `internal/classifier/classifier.go` | Classifier interface, request input, unavailable sentinel, and protocol constants. |
| `internal/classifier/laya/client.go` | Bounded authenticated Go HTTP client for `mindctl.classifier.v1`. |
| `internal/classifier/laya/types.go` | Wire schemas, six-answer validation, and Laya-to-domain conversion. |
| `internal/classifier/laya/client_test.go` | Gateway-side protocol, validation, retry, timeout, and redaction tests. |
| `internal/config/types.go`, `internal/config/load_test.go` | `classifier:` YAML configuration and strict secret/endpoint validation. |
| `internal/app/app.go`, `internal/app/app_test.go`, `cmd/mindctl/main_test.go` | Application wiring to Laya and fallback integration coverage. |
| `internal/router/*`, `internal/executor/*`, `internal/storage/*` | Neutral naming while retaining deterministic behavior. |
| `migrations/003_classifier_judgments.sql` | Atomic rename of durable classifier records. |
| `sidecar/laya/pyproject.toml`, `sidecar/laya/uv.lock` | Reproducible Python runtime and locked Laya dependency graph. |
| `sidecar/laya/src/mindctl_laya/*.py` | Contract models, immutable questions, model lifecycle, HTTP app. |
| `sidecar/laya/tests/*.py` | Sidecar contract/lifecycle/security tests using a mocked Laya agent and snapshot downloader. |
| `Dockerfile.laya`, `compose.laya.yaml` | Optional local sidecar image and Compose profile. |
| `.github/workflows/release.yml`, `devenv.nix`, `README.md`, `config.example.yaml` | Development, publication, and operator instructions. |

### Task 1: Neutralize the Go classifier contract and routing vocabulary

**Files:**

- Modify: `internal/domain/features.go`
- Modify: `internal/classifier/classifier.go`
- Modify: `internal/router/policy.go`, `internal/router/floor.go`
- Modify: `internal/router/policy_test.go`, `internal/router/floor_test.go`
- Modify: `internal/executor/service.go`, `internal/executor/service_test.go`, `internal/executor/stream_test.go`
- Modify: `internal/storage/storage.go` and type-only references in `internal/app/*_test.go`, `internal/storage/*_test.go`

**Interfaces:**

- Produces `domain.ClassifierJudgment` with `MinimumTier`, tier/task distributions and confidences, three score/confidence pairs, `Underspecified`, `Classifier`, `ResolvedModel`, `ModelRevision`, and client-measured `Latency`.
- Produces `router.PolicyConfig.MinClassifierConfidence` and `router.DecisionInput.Judgment *domain.ClassifierJudgment`.
- `classifier.Classifier` becomes `Classify(context.Context, Input) (domain.ClassifierJudgment, error)`.

- [ ] **Step 1: Make policy tests require provider-neutral reasons and names**

  Change the representative policy table case to use `domain.ClassifierJudgment` and add this assertion:

  ```go
  func TestClassifierMinimumReasonIsProviderNeutral(t *testing.T) {
      got, err := NewPolicy(PolicyConfig{MinClassifierConfidence: .7}).Decide(
          DecisionInput{Floor: domain.T0, Judgment: &domain.ClassifierJudgment{
              MinimumTier: domain.T4, TierConfidence: .8,
          }, Models: []domain.Model{policyModel("t4", domain.T4, 0, 1)}})
      if err != nil || !slices.Contains(got.Reasons, "classifier minimum raised floor from T0 to T4") {
          t.Fatalf("decision=%+v err=%v", got, err)
      }
  }
  ```

- [ ] **Step 2: Run the focused test to verify the old contract fails**

  Run: `devenv shell -- go test ./internal/router -run TestClassifierMinimumReasonIsProviderNeutral -count=1`

  Expected: FAIL because `ClassifierJudgment` and `MinClassifierConfidence` do not exist.

- [ ] **Step 3: Rename the contract without changing routing semantics**

  Replace the provider-specific type and fields with this shape, update every compile-time reference, and retain latency while removing Jev token accounting:

  ```go
  type ClassifierJudgment struct {
      MinimumTier                         Tier
      TierConfidence                      float64
      TierProbabilities                   map[Tier]float64
      TaskType                            TaskType
      TaskTypeConfidence                  float64
      TaskTypeProbabilities               map[TaskType]float64
      ReasoningScore, ReasoningConfidence float64
      CodingScore, CodingConfidence       float64
      BlastRadius, BlastRadiusConfidence  float64
      Underspecified                      float64
      Classifier, ResolvedModel           string
      ModelRevision                       string
      Latency                             time.Duration
  }
  ```

  Rename `MinJevConfidence` to `MinClassifierConfidence`; use the exact reason strings `classifier minimum` and `classifier tier choice ignored below confidence threshold; existing floor retained`; change all comments and test names to say classifier rather than Jev. Keep the same .7 default and floors.

- [ ] **Step 4: Run neutral core tests**

  Run: `devenv shell -- go test ./internal/classifier ./internal/router ./internal/executor -count=1`

  Expected: PASS, including unchanged pin and fallback behavior.

- [ ] **Step 5: Commit the neutral core refactor**

  ```sh
  git add internal/domain/features.go internal/classifier/classifier.go internal/router internal/executor internal/storage internal/app/*_test.go
  git commit -m "refactor(router): neutralize classifier signals"
  ```

### Task 2: Build the locked Laya sidecar and its immutable HTTP contract

**Files:**

- Create: `sidecar/laya/pyproject.toml`, `sidecar/laya/uv.lock`
- Create: `sidecar/laya/src/mindctl_laya/__init__.py`, `contract.py`, `questions.py`, `service.py`, `app.py`
- Create: `sidecar/laya/tests/conftest.py`, `test_contract.py`, `test_app.py`
- Modify: `devenv.nix`

**Interfaces:**

- Produces `create_app(settings: Settings, load_agent: Callable[[], Agent]) -> FastAPI`.
- Produces `POST /v1/classify`, `GET /healthz`, and `GET /readyz` with the documented `mindctl.classifier.v1` payloads.
- Consumes `LAYA_CLASSIFIER_TOKEN`, `LAYA_MODEL_REVISION`, and `LAYA_MODEL_VARIANT`; defaults are the revision and `typed-decisions` in Global Constraints.

- [ ] **Step 1: Add the failing contract tests before the service exists**

  Write a mocked agent/snapshot fixture and tests that pin the external behavior:

  ```python
  @pytest.mark.anyio
  async def test_classify_requires_token_and_emits_all_six_answers(client):
      unauthorized = await client.post("/v1/classify", json=valid_request())
      assert unauthorized.status_code == 401
      response = await client.post("/v1/classify", headers=auth(), json=valid_request())
      assert response.status_code == 200
      body = response.json()
      assert body["schema_version"] == "mindctl.classifier.v1"
      assert body["classifier"]["revision"] == "5e7b2b1b8ca2ecdd3f2322d94069c9b6ce7e844b"
      assert set(body["answers"]) == {
          "minimum_tier", "task_type", "reasoning_required",
          "coding_required", "blast_radius", "underspecified",
      }

  @pytest.mark.anyio
  async def test_ready_is_unavailable_until_model_load_completes(client, loader):
      loader.block()
      assert (await client.get("/healthz")).status_code == 200
      assert (await client.get("/readyz")).status_code == 503
      loader.release()
      await wait_until_ready(client)
      assert (await client.get("/readyz")).status_code == 200
  ```

  Add cases for an extra request field (422), body larger than the configured limit (413), an inference exception (503 with no prompt/exception text), and the exact six fixed question IDs/types/criteria.

- [ ] **Step 2: Install the project environment and prove the tests fail**

  Run: `devenv shell -- uv run --project sidecar/laya pytest -q`

  Expected: FAIL because the project, application factory, and routes do not exist.

- [ ] **Step 3: Define the locked runtime and strict Pydantic schemas**

  Add Python 3.12 and `uv` to `devenv.nix`. Create `pyproject.toml` with `requires-python = ">=3.10"`, runtime dependencies `fastapi`, `uvicorn`, `laya`, and `huggingface-hub`, plus the `pytest`, `pytest-anyio`, and `httpx` development group. Generate and commit the resolver output with:

  ```sh
  devenv shell -- uv lock --project sidecar/laya
  ```

  In `contract.py`, use `ConfigDict(extra="forbid")` for `ClassifierRequest`, nested `State`, and answer response models. Define `SCHEMA_VERSION = "mindctl.classifier.v1"`, a 1 MiB `MAX_BODY_BYTES`, and exact output models for choice, score, and noul.

- [ ] **Step 4: Implement fixed questions, lifecycle, and redacted endpoints**

  In `questions.py`, define one immutable mapping with the existing T0–T6, eight task types, 0–6 reasoning/coding, 0–4 blast-radius, and noul rubrics. In `service.py`, call `huggingface_hub.snapshot_download("convaiinnovations/laya", revision=settings.model_revision)` during startup, then call `laya.load(local_snapshot, subfolder="typed-decisions")`; protect `agent.predict` with an `asyncio.Lock` plus `run_in_threadpool`.

  Implement the core route behavior as:

  ```python
  @router.post("/v1/classify", response_model=ClassifyResponse)
  async def classify(request: Request, body: ClassifyRequest, _: None = Depends(require_token)):
      if request.headers.get("content-length") and int(request.headers["content-length"]) > MAX_BODY_BYTES:
          raise HTTPException(413, "request body too large")
      try:
          answers = await service.predict(body.state.model_dump(mode="json"), QUESTIONS)
      except Exception:
          raise HTTPException(503, "classifier unavailable") from None
      return ClassifyResponse.from_laya(answers, settings.classifier_metadata())
  ```

  Install a `BodyLimitMiddleware` that counts all ASGI `http.request` body chunks and returns 413 after `MAX_BODY_BYTES`, so chunked requests cannot bypass the `Content-Length` precheck. `/healthz` must always return 200 after app construction; `/readyz` returns 503 until model loading succeeds and 200 only with loaded immutable metadata. Do not log request state or exception strings.

- [ ] **Step 5: Run Python tests and lock validation**

  Run: `devenv shell -- uv run --locked --project sidecar/laya pytest -q`

  Expected: PASS, without downloading Laya weights because the fixture injects a fake agent and snapshot downloader.

- [ ] **Step 6: Commit the sidecar contract**

  ```sh
  git add devenv.nix sidecar/laya
  git commit -m "feat(laya): add typed-decision sidecar contract"
  ```

### Task 3: Add the strict Go Laya client

**Files:**

- Create: `internal/classifier/laya/client.go`, `internal/classifier/laya/types.go`, `internal/classifier/laya/client_test.go`
- Delete: `internal/classifier/jev/client.go`, `internal/classifier/jev/types.go`, `internal/classifier/jev/client_test.go`

**Interfaces:**

- Produces `laya.NewClient(cfg config.ClassifierConfig, token string, httpClient *http.Client) *Client`.
- `Client.Classify(context.Context, classifier.Input) (domain.ClassifierJudgment, error)` is the only Go implementation of `classifier.Classifier`.
- Consumes `mindctl.classifier.v1` and returns `classifier.ErrUnavailable` for every unsafe/unavailable response.

- [ ] **Step 1: Port the old client tests to the new contract and add contract rejection cases**

  Define a valid v1 JSON fixture with classifier metadata and complete distributions. Assert request shape and output mapping:

  ```go
  func TestClientClassifyMapsLayaContract(t *testing.T) {
      server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          if r.URL.Path != "/v1/classify" || r.Header.Get("Authorization") != "Bearer secret" {
              t.Errorf("path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
          }
          var got struct { SchemaVersion string `json:"schema_version"`; State classifier.Input `json:"state"` }
          if err := json.NewDecoder(r.Body).Decode(&got); err != nil { t.Fatal(err) }
          if got.SchemaVersion != "mindctl.classifier.v1" || !reflect.DeepEqual(got.State, sampleInput()) { t.Fatal(got) }
          _, _ = io.WriteString(w, validLayaResponse)
      }))
      judgment, err := testClient(server.URL).Classify(context.Background(), sampleInput())
      if err != nil || judgment.Classifier != "laya" || judgment.ModelRevision == "" { t.Fatalf("%+v %v", judgment, err) }
  }
  ```

  Add table cases for schema mismatch, wrong classifier name, empty repository/revision/variant, non-200 body, redirect, invalid bearer token, unknown field, missing selected probability, a probability sum outside `1e-3`, NaN/Inf score, illegal tier/task, wrong answer type, missing legend, and errors containing a private prompt or upstream body.

- [ ] **Step 2: Run the client tests to verify they fail before implementation**

  Run: `devenv shell -- go test ./internal/classifier/laya -count=1`

  Expected: FAIL because the Laya package and client do not exist.

- [ ] **Step 3: Implement the bounded endpoint client and answer conversion**

  Copy only the safe mechanics from the old client: a five-second default timeout, a copied HTTP client with redirects rejected, a 1 MiB response cap, retry after 429/529 and temporary transport errors, cancellation-aware exponential backoff, and `ErrUnavailable` as the sole outward error.

  Emit this wire request and validate every response field before mapping it:

  ```go
  type request struct {
      SchemaVersion string           `json:"schema_version"`
      State         classifier.Input `json:"state"`
  }

  func (c *Client) Classify(parent context.Context, input classifier.Input) (domain.ClassifierJudgment, error) {
      body, err := json.Marshal(request{SchemaVersion: schemaVersion, State: input})
      if err != nil { return domain.ClassifierJudgment{}, classifier.ErrUnavailable }
      // execute POST strings.TrimRight(c.config.Endpoint, "/")+"/v1/classify"
      // with the bounded retry/deadline policy, then call parseJudgment.
  }
  ```

  `parseJudgment` must demand all six named answers and full legal probability maps. Set `Classifier` to `laya`, copy repository/variant/revision into stable metadata fields, and set `Latency` only after a successful response. Do not retain the old Jev package or token fields.

- [ ] **Step 4: Run focused Go client tests**

  Run: `devenv shell -- go test ./internal/classifier/laya -count=1`

  Expected: PASS, with no reference to `internal/classifier/jev` remaining.

- [ ] **Step 5: Commit the Laya client**

  ```sh
  git add internal/classifier/laya internal/classifier/jev internal/domain/features.go
  git commit -m "feat(router): call Laya classifier endpoint"
  ```

### Task 4: Switch gateway configuration and application wiring

**Files:**

- Modify: `internal/config/types.go`, `internal/config/load_test.go`
- Modify: `internal/app/app.go`, `internal/app/app_test.go`, `internal/app/catalog_test.go`
- Modify: `cmd/mindctl/main_test.go`
- Modify: `config.example.yaml`

**Interfaces:**

- Produces `config.ClassifierConfig{Endpoint, TokenEnv string; Timeout time.Duration; MaxRetries int}` under `Config.Classifier`.
- `config.Validate` requires `Classifier.TokenEnv`, rejects an invalid endpoint/negative retry/timeouts, and has no Jev path.
- `app.New` resolves the classifier token once and wires `laya.NewClient`.

- [ ] **Step 1: Write configuration and fallback failures first**

  Replace fixture YAML with `classifier:` and add:

  ```go
  func TestLoadRejectsLegacyJevConfiguration(t *testing.T) {
      path := writeConfig(t, validYAMLWith("jev:\n  api_key_env: JEV_API_KEY\n"))
      _, err := Load(path, testEnv)
      if err == nil || !strings.Contains(err.Error(), "field jev not found") {
          t.Fatalf("err=%v", err)
      }
  }

  func TestUnavailableLayaPreservesT4Fallback(t *testing.T) {
      app := configuredAppWithClassifier(t, "http://127.0.0.1:1")
      result := executeAutomaticRequest(t, app)
      if result.Decision.Tier < domain.T4 { t.Fatalf("tier=%s", result.Decision.Tier) }
  }
  ```

  Keep a compatible pinned-conversation test that proves classifier unavailability does not invoke it and does not lower the pin.

- [ ] **Step 2: Run tests to verify config and app still expect Jev**

  Run: `devenv shell -- go test ./internal/config ./internal/app ./cmd/mindctl -run 'Test(LoadRejectsLegacyJevConfiguration|UnavailableLayaPreservesT4Fallback)' -count=1`

  Expected: FAIL because the current configuration accepts `jev` and application wiring imports the Jev client.

- [ ] **Step 3: Implement the single endpoint configuration path**

  Add:

  ```go
  type ClassifierConfig struct {
      Endpoint   string        `yaml:"endpoint"`
      TokenEnv   string        `yaml:"token_env"`
      Timeout    time.Duration `yaml:"timeout"`
      MaxRetries int           `yaml:"max_retries"`
  }
  ```

  Require a nonempty token reference and an absolute HTTP(S) endpoint without userinfo, query, or fragment. Use `laya.NewClient(cfg.Classifier, classifierToken, a.httpClient)` in `newWithLookup`; retain the one-read secret snapshot. Rename configuration comments and policy field `min_classifier_confidence`. Update every fixture and `config.example.yaml` to the approved endpoint/token form.

- [ ] **Step 4: Run gateway configuration and application tests**

  Run: `devenv shell -- go test ./internal/config ./internal/app ./cmd/mindctl -count=1`

  Expected: PASS, including strict rejection of `jev:` and safe Laya-unavailable fallback.

- [ ] **Step 5: Commit gateway migration**

  ```sh
  git add internal/config internal/app cmd/mindctl config.example.yaml
  git commit -m "feat(config): configure neutral Laya classifier"
  ```

### Task 5: Rename durable classifier storage without losing historical rows

**Files:**

- Create: `migrations/003_classifier_judgments.sql`
- Modify: `internal/storage/sqlite/repository.go`, `internal/storage/sqlite/commit_test.go`, `internal/storage/sqlite/repository_test.go`
- Modify: `internal/storage/storage.go`

**Interfaces:**

- Produces an embedded migration that executes `ALTER TABLE jev_judgments RENAME TO classifier_judgments` exactly once.
- Repository writes/reads `classifier_judgments` and serializes `domain.ClassifierJudgment`.

- [ ] **Step 1: Add a migration regression test using a pre-migration database**

  Create a temporary SQLite database, apply only `001_core.sql` and `002_conversations.sql`, insert a known JSON row into `jev_judgments`, then open it normally. Assert both migration and data preservation:

  ```go
  var got string
  if err := db.SQL().QueryRow(`SELECT judgment_json FROM classifier_judgments WHERE request_id = ?`, "request-1").Scan(&got); err != nil {
      t.Fatal(err)
  }
  if got != `{"MinimumTier":4,"ResolvedModel":"jev-history"}` {
      t.Fatalf("historical JSON changed: %s", got)
  }
  ```

  Update existing count assertions to name `classifier_judgments` and add a repository round-trip with `Classifier: "laya"` and a nonempty model revision.

- [ ] **Step 2: Run the storage tests to verify the missing migration fails**

  Run: `devenv shell -- go test ./internal/storage/sqlite -run 'Test(MigratesJevJudgmentsWithoutDataLoss|RepositoryRoundTripsClassifierScoreConfidences)' -count=1`

  Expected: FAIL because `classifier_judgments` and migration `003` do not exist.

- [ ] **Step 3: Add the atomic table rename and update repository SQL**

  Create `migrations/003_classifier_judgments.sql` containing exactly:

  ```sql
  ALTER TABLE jev_judgments RENAME TO classifier_judgments;
  ```

  Replace the insert/select strings in `repository.go`, use `domain.ClassifierJudgment` in storage records, and preserve `json.Unmarshal` compatibility for historical JSON whose newly added fields are absent.

- [ ] **Step 4: Run all storage and migration tests**

  Run: `devenv shell -- go test ./internal/storage/... -count=1`

  Expected: PASS, including reopen and rollback coverage.

- [ ] **Step 5: Commit persistence migration**

  ```sh
  git add migrations internal/storage
  git commit -m "feat(storage): persist neutral classifier judgments"
  ```

### Task 6: Ship and document the optional sidecar

**Files:**

- Create: `Dockerfile.laya`, `compose.laya.yaml`
- Modify: `.github/workflows/release.yml`, `README.md`, `.dockerignore`

**Interfaces:**

- Produces `ghcr.io/storm-software/mindctl-laya:<version>` and `:sha-<short>` release images from `Dockerfile.laya`.
- Produces an opt-in Compose `laya` profile, internal network endpoint `http://laya:8091`, cache volume, and no host port by default.

- [ ] **Step 1: Write packaging/documentation checks before adding assets**

  Add a shell-level verification in the release workflow test job and a documentation assertion:

  ```sh
  test -f Dockerfile.laya
  docker build --file Dockerfile.laya --tag mindctl-laya:test .
  docker compose -f compose.laya.yaml config --quiet
  rg -n 'classifier:|LAYA_CLASSIFIER_TOKEN|compose.laya.yaml|mindctl-laya' README.md config.example.yaml
  ```

- [ ] **Step 2: Run the packaging checks to verify the sidecar assets are absent**

  Run: `test -f Dockerfile.laya && docker compose -f compose.laya.yaml config --quiet`

  Expected: FAIL because neither sidecar asset exists.

- [ ] **Step 3: Implement the image, profile, release image, and operator guidance**

  Use a Python 3.12 slim base in `Dockerfile.laya`; copy only `sidecar/laya/pyproject.toml`, `uv.lock`, and source; install with `uv sync --locked --no-dev`; run as a non-root user; expose 8091; start `uvicorn mindctl_laya.app:app --host 0.0.0.0 --port 8091`. Do not download model weights at image build time.

  In `compose.laya.yaml`, define a `laya` service in profile `laya`, pass `LAYA_CLASSIFIER_TOKEN`, mount `laya-model-cache:/root/.cache/huggingface`, and make the gateway config endpoint resolve as `http://laya:8091`. Do not publish a sidecar port by default.

  Add a second Buildx metadata/build pair in `release.yml` for `ghcr.io/${{ github.repository }}-laya`, using the same semver and SHA tag conventions as the gateway. Extend PR paths and CI to run `uv run --locked --project sidecar/laya pytest -q` and both image build checks.

  Update README with exact local profile launch, the externally managed endpoint form, model cache/runtime expectations, bearer-token requirement, and an explicit statement that normal tests do not download weights. Replace all current Jev mentions. Update `.dockerignore` only to exclude sidecar virtual environments and caches, not its source or lockfile.

- [ ] **Step 4: Run sidecar packaging checks**

  Run: `devenv shell -- uv run --locked --project sidecar/laya pytest -q && docker build --file Dockerfile.laya --tag mindctl-laya:test . && docker compose -f compose.laya.yaml config --quiet`

  Expected: PASS without pulling model weights.

- [ ] **Step 5: Commit packaging and docs**

  ```sh
  git add Dockerfile.laya compose.laya.yaml .dockerignore .github/workflows/release.yml README.md config.example.yaml
  git commit -m "feat(laya): ship optional classifier sidecar"
  ```

### Task 7: Run full verification and prove the removal boundary

**Files:**

- Modify only if needed to fix a failing test from the commands below; otherwise no files.

**Interfaces:**

- Verifies the implementation of all previous tasks; produces no new public interface.

- [ ] **Step 1: Run the complete deterministic suite**

  Run: `devenv shell -- go test ./... -count=1 && devenv shell -- go test -race ./... -count=1 && devenv shell -- go vet ./...`

  Expected: PASS.

- [ ] **Step 2: Run sidecar and contract suite from the locked environment**

  Run: `devenv shell -- uv run --locked --project sidecar/laya pytest -q`

  Expected: PASS without model download.

- [ ] **Step 3: Check formatting, migration state, and complete Jev removal**

  Run:

  ```sh
  devenv shell -- gofmt -w internal/domain/features.go internal/classifier/classifier.go internal/classifier/laya/*.go internal/config/types.go internal/app/app.go internal/router/*.go internal/executor/*.go internal/storage/storage.go internal/storage/sqlite/*.go
  devenv shell -- go test ./... -count=1
  devenv shell -- pnpm prettier --check README.md config.example.yaml compose.laya.yaml Dockerfile.laya docs/superpowers/specs/2026-09-23-laya-classifier-design.md
  git diff --check
  ! rg -n -i '\bjev\b' --glob '!docs/superpowers/**' .
  ```

  Expected: all commands pass; historical design documents may retain the word Jev, but active source/config/operator documentation may not.

- [ ] **Step 4: Build runtime artifacts without inference**

  Run: `docker build --file Dockerfile.laya --tag mindctl-laya:verify . && docker compose -f compose.laya.yaml config --quiet && devenv shell -- goreleaser check`

  Expected: PASS; image build and Compose resolution complete without model weights.

- [ ] **Step 5: Commit only required verification fixes**

  ```sh
  git add internal/domain internal/classifier internal/config internal/app internal/router internal/executor internal/storage migrations/003_classifier_judgments.sql sidecar/laya Dockerfile.laya compose.laya.yaml .dockerignore .github/workflows/release.yml README.md config.example.yaml devenv.nix
  git commit -m "test(laya): verify classifier replacement"
  ```

## Plan self-review

- **Spec coverage:** Tasks 1 and 4 preserve deterministic routing/fallback and neutral naming; Task 2 implements fixed-question Laya inference and readiness; Task 3 implements the endpoint contract and strict client validation; Task 5 preserves historical SQLite data; Task 6 provides local/remote operator delivery; Task 7 proves the integrated result.
- **Placeholder scan:** No deferred work markers, generic error-handling instructions, or unspecified test cases remain. The artifact revision, variant, endpoint, status behavior, configuration, names, commands, and acceptance checks are explicit.
- **Type consistency:** `domain.ClassifierJudgment`, `config.ClassifierConfig`, `laya.NewClient`, and `mindctl.classifier.v1` are introduced before each consuming task. The sidecar returns metadata that the Go client maps without token accounting.
- **Review Focus:** Each listed failure condition has an owning test in Tasks 2–5. Ordinary test commands use mocks and locked dependencies, while no task invokes a live Hugging Face inference download.
