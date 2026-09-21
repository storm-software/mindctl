# Jev Router Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Go routing core, strict configuration, Jev classifier, encrypted SQLite persistence, and a runnable health/readiness server.

**Architecture:** A modular Go binary keeps routing policy pure while network, encryption, and storage sit behind narrow interfaces. This first increment does not execute provider inference; it boots from validated YAML, persists reproducible decisions, classifies uncertain requests with Jev, and exposes operational health.

**Tech Stack:** Go 1.26.7, `net/http`, `database/sql`, `gopkg.in/yaml.v3` v3.0.1, `modernc.org/sqlite` v1.59.0, AES-GCM from the Go standard library.

**Spec:** `docs/superpowers/specs/2026-09-21-jev-model-router-design.md`

## Global Constraints

- Use capability tiers T0 through T6 exactly as defined in the spec.
- Jev supplies signals only; deterministic Go policy owns the final route.
- Optimize expected total cost, not first-attempt price.
- Use T4 as the configurable safe fallback when Jev is unavailable and no conversation pin exists.
- Never select below a caller constraint, conversation floor, or capability requirement.
- Persist raw content only after AES-GCM encryption; retention defaults to unlimited.
- Read gateway, provider, Jev, and encryption secrets from referenced environment variables, never YAML values.
- Do not modify `/home/development/repos/model-router` or any external ecosystem package.
- Run repository commands through `devenv shell --`.

## Review Focus

- Unknown YAML fields, missing environment variables, duplicate model IDs, and invalid tier bounds must prevent startup; Task 1 tests each case.
- Empty candidate sets and incompatible capabilities must return a typed policy error, never fall back below the floor; Task 2 tests both cases.
- A cheaper model with a high failure prior must lose to a more reliable model when expected total cost is higher; Task 3 pins this calculation.
- Jev 429/529 responses, deadlines, and partially malformed answers must produce bounded retries followed by the T4 fallback path; Task 4 tests all four conditions.
- Encryption must use unique nonces, preserve old-key reads during rotation, keep plaintext out of SQLite, and never expire content when retention is zero; Tasks 5 and 6 test these invariants.

---

### Task 1: Go module and strict configuration

**Files:**

- Create: `go.mod`
- Create: `go.sum`
- Create: `internal/config/types.go`
- Create: `internal/config/load.go`
- Create: `internal/config/load_test.go`
- Create: `config.example.yaml`

**Interfaces:**

- Consumes: environment lookup supplied as `func(string) (string, bool)`.
- Produces: `config.Load(path string, getenv func(string) (string, bool)) (config.Config, error)` and `config.Config.Validate(getenv func(string) (string, bool)) error`.

- [ ] **Step 1: Initialize the pinned module**

Run:

```bash
devenv shell -- go mod init github.com/storm-software/mindctl
devenv shell -- go mod edit -go=1.26.0 -toolchain=go1.26.7
devenv shell -- go get gopkg.in/yaml.v3@v3.0.1 modernc.org/sqlite@v1.59.0
```

Expected: `go.mod` names `github.com/storm-software/mindctl`, pins Go 1.26.0/toolchain 1.26.7, and records both dependencies.

- [ ] **Step 2: Write failing strict-load tests**

```go
func TestLoadRejectsUnknownField(t *testing.T) {
    path := writeConfig(t, "listen: ':8080'\nunknown: true\n")
    _, err := Load(path, func(string) (string, bool) { return "", false })
    if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
        t.Fatalf("expected unknown-field error, got %v", err)
    }
}

func TestValidateRejectsMissingSecretAndDuplicateModel(t *testing.T) {
    cfg := validConfig()
    cfg.Models = append(cfg.Models, cfg.Models[0])
    err := cfg.Validate(func(string) (string, bool) { return "", false })
    if err == nil || !strings.Contains(err.Error(), "duplicate model") ||
        !strings.Contains(err.Error(), "missing environment variable") {
        t.Fatalf("expected combined validation error, got %v", err)
    }
}

func TestValidateRejectsInvalidTierBounds(t *testing.T) {
    cfg := validConfig()
    cfg.Routing.MinTier, cfg.Routing.MaxTier = "T5", "T3"
    if err := cfg.Validate(testEnv); err == nil {
        t.Fatal("expected min tier above max tier to fail")
    }
}
```

- [ ] **Step 3: Run the tests and observe the missing package failure**

Run: `devenv shell -- go test ./internal/config -run 'Test(Load|Validate)' -v`

Expected: FAIL because `Load`, `Config`, and the configuration types do not exist.

- [ ] **Step 4: Implement strict YAML loading and validation**

```go
type Config struct {
    Listen      string           `yaml:"listen"`
    ClientAuth ClientAuthConfig `yaml:"client_auth"`
    Jev         JevConfig        `yaml:"jev"`
    SQLite      SQLiteConfig     `yaml:"sqlite"`
    Encryption EncryptionConfig `yaml:"encryption"`
    Routing     RoutingConfig    `yaml:"routing"`
    Providers   []ProviderConfig `yaml:"providers"`
    Models      []ModelConfig    `yaml:"models"`
}

func Load(path string, getenv func(string) (string, bool)) (Config, error) {
    file, err := os.Open(path)
    if err != nil { return Config{}, fmt.Errorf("open config: %w", err) }
    defer file.Close()
    dec := yaml.NewDecoder(file)
    dec.KnownFields(true)
    var cfg Config
    if err := dec.Decode(&cfg); err != nil {
        return Config{}, fmt.Errorf("decode config: %w", err)
    }
    if err := cfg.Validate(getenv); err != nil { return Config{}, err }
    return cfg, nil
}
```

Validation must aggregate errors with `errors.Join`, reject unknown tiers, duplicate IDs, missing provider references, negative prices, missing secret environment variables, and encryption keys that are not base64-encoded 32-byte values. `config.example.yaml` must use environment-variable names such as `OPENAI_API_KEY`, not secret values.

- [ ] **Step 5: Run focused and full tests**

Run:

```bash
devenv shell -- go test ./internal/config -v
devenv shell -- go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit the configuration foundation**

```bash
git add go.mod go.sum internal/config config.example.yaml
git commit -m "feat(router): add strict gateway configuration"
```

### Task 2: Tier, catalog, and eligibility domain

**Files:**

- Create: `internal/domain/tier.go`
- Create: `internal/domain/model.go`
- Create: `internal/domain/features.go`
- Create: `internal/router/eligibility.go`
- Create: `internal/router/eligibility_test.go`

**Interfaces:**

- Consumes: validated `config.ModelConfig` values from Task 1.
- Produces: `domain.ParseTier(string) (domain.Tier, error)`, `router.EligibleModels(router.EligibilityInput) ([]domain.Model, []router.Rejection)`, and shared model/feature types for every later plan.

- [ ] **Step 1: Write failing tier and eligibility tests**

```go
func TestTierRoundTrip(t *testing.T) {
    for i, text := range []string{"T0", "T1", "T2", "T3", "T4", "T5", "T6"} {
        tier, err := domain.ParseTier(text)
        if err != nil || tier.Rank() != i || tier.String() != text {
            t.Fatalf("round trip %q: tier=%v err=%v", text, tier, err)
        }
    }
}

func TestEligibleModelsRejectsCapabilityAndFloorViolations(t *testing.T) {
    models := []domain.Model{
        {ID: "text-t3", Tier: domain.T3, Capabilities: domain.Capabilities{Text: true}},
        {ID: "vision-t4", Tier: domain.T4, Capabilities: domain.Capabilities{Text: true, Images: true}, ContextWindow: 4096},
    }
    got, rejected := router.EligibleModels(router.EligibilityInput{
        Models: models, Floor: domain.T4,
        Features: domain.RequestFeatures{NeedsImages: true, ContextTokens: 8192},
    })
    if len(got) != 0 || len(rejected) != 2 { t.Fatalf("got=%v rejected=%v", got, rejected) }
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `devenv shell -- go test ./internal/domain ./internal/router -run 'Test(Tier|Eligible)' -v`

Expected: FAIL with missing domain and router declarations.

- [ ] **Step 3: Implement the shared types**

```go
type Tier uint8

const (
    T0 Tier = iota
    T1
    T2
    T3
    T4
    T5
    T6
)

type Model struct {
    ID, Provider, UpstreamID string
    Tier                     Tier
    Capabilities             Capabilities
    ContextWindow            int64
    MaxOutputTokens          int64
    Pricing                  Pricing
    SuccessPriors            map[TaskType]float64
    DefaultSuccessPrior      float64
    LatencyP95               time.Duration
    Order                    int
    Available                bool
}

type TaskType string

const (
    TaskUnknown TaskType = "unknown"
    TaskExtraction TaskType = "extraction"
    TaskClassification TaskType = "classification"
    TaskGeneration TaskType = "generation"
    TaskReasoning TaskType = "reasoning"
    TaskCoding TaskType = "coding"
    TaskToolUse TaskType = "tool_use"
    TaskMultimodal TaskType = "multimodal"
)

type Capabilities struct {
    Text, Images, Functions, JSONSchema bool
    HostedTools map[string]bool
}

type Pricing struct {
    InputPerMillion, CachedInputPerMillion, OutputPerMillion float64
    PerRequestUSD float64
}

type RequestFeatures struct {
    InputTokens, ContextTokens, MaxOutputTokens int64
    NeedsText, NeedsImages, NeedsFunctions      bool
    NeedsJSONSchema, NeedsHostedTools           bool
    HostedToolTypes                             []string
}
```

Implement `EligibleModels` as a pure filter that returns explicit rejection codes for tier, context, output limit, modality, tools, credentials, and availability. It must never silently weaken a requirement.

- [ ] **Step 4: Add the empty-candidate typed-error test**

```go
func TestRequireEligibleReturnsTypedError(t *testing.T) {
    _, err := router.RequireEligible(nil, []router.Rejection{{ModelID: "m", Code: router.RejectContext}})
    var noModel *router.NoEligibleModelError
    if !errors.As(err, &noModel) || len(noModel.Rejections) != 1 {
        t.Fatalf("expected typed error, got %v", err)
    }
}
```

- [ ] **Step 5: Run tests and commit**

Run: `devenv shell -- go test ./internal/domain ./internal/router -v`

Expected: PASS.

```bash
git add internal/domain internal/router/eligibility.go internal/router/eligibility_test.go
git commit -m "feat(router): define tiers and model eligibility"
```

### Task 3: Pure expected-cost routing policy

**Files:**

- Create: `internal/router/cost.go`
- Create: `internal/router/policy.go`
- Create: `internal/router/policy_test.go`
- Create: `internal/router/policy_fuzz_test.go`

**Interfaces:**

- Consumes: `domain.Model`, `domain.RequestFeatures`, and eligibility output from Task 2.
- Produces: `router.NewPolicy(router.PolicyConfig) *router.Policy` and `(*router.Policy).Decide(router.DecisionInput) (router.Decision, error)`.

- [ ] **Step 1: Write the failing expected-cost selection test**

```go
func TestPolicyChoosesLowerExpectedTotalCost(t *testing.T) {
    cheap := model("cheap", domain.T4, 0.0001, 0.75)
    reliable := model("reliable", domain.T4, 0.0004, 0.99)
    p := router.NewPolicy(router.PolicyConfig{FailureEscalationCost: 0.002})
    got, err := p.Decide(router.DecisionInput{
        Floor: domain.T4, TaskType: domain.TaskCoding,
        Models: []domain.Model{cheap, reliable}, Features: oneRequest(),
    })
    if err != nil { t.Fatal(err) }
    if got.ModelID != "reliable" { t.Fatalf("selected %s", got.ModelID) }
}
```

- [ ] **Step 2: Write failing floor, pin, and tie-break tests**

```go
func TestPolicyNeverDowngradesPin(t *testing.T) {
    got, err := testPolicy().Decide(router.DecisionInput{
        Floor: domain.T2, Pin: &router.Pin{ModelID: "strong", Floor: domain.T5},
        Models: catalog(), Features: oneRequest(),
    })
    if err != nil || got.Tier < domain.T5 { t.Fatalf("decision=%+v err=%v", got, err) }
}

func TestPolicyTieBreaksByDirectCostLatencyThenConfigOrder(t *testing.T) {
    got, _ := testPolicy().Decide(equalExpectedCostInput())
    if got.ModelID != "lower-direct-cost" { t.Fatalf("decision=%+v", got) }
}
```

- [ ] **Step 3: Run focused tests and observe failure**

Run: `devenv shell -- go test ./internal/router -run 'TestPolicy' -v`

Expected: FAIL because policy and cost types do not exist.

- [ ] **Step 4: Implement policy and reproducible explanations**

```go
type DecisionInput struct {
    Features    domain.RequestFeatures
    Models      []domain.Model
    Floor       domain.Tier
    TaskType    domain.TaskType
    Pin         *Pin
    MinTier     *domain.Tier
    MaxTier     *domain.Tier
    Judgment    *domain.JevJudgment
}

type Pin struct {
    ModelID, Provider string
    Floor domain.Tier
}

type CandidateScore struct {
    ModelID, Provider string
    DirectCost, FailureProbability, EscalationCost float64
    LatencyPenalty, ExpectedTotalCost              float64
}

type Decision struct {
    Tier domain.Tier
    ModelID, Provider string
    Reasons []string
    Candidates []CandidateScore
    Rejections []Rejection
}
```

Calculate direct cost from estimated input/output/cache tokens. Apply explicit bounds, pin floor, Jev floor, risk, underspecification, and confidence gates before eligibility. Sort candidates by expected total cost, direct cost, latency, then configuration order. Include every floor movement and rejection in the decision.

- [ ] **Step 5: Add the invariant fuzz test**

```go
func FuzzPolicyNeverSelectsBelowFloor(f *testing.F) {
    f.Add(uint8(4), uint8(2))
    f.Fuzz(func(t *testing.T, floorRaw, modelRaw uint8) {
        floor := domain.Tier(floorRaw % 7)
        modelTier := domain.Tier(modelRaw % 7)
        got, err := testPolicy().Decide(singleModelInput(floor, modelTier))
        if err == nil && got.Tier < floor { t.Fatalf("selected %s below %s", got.Tier, floor) }
    })
}
```

- [ ] **Step 6: Run policy tests and commit**

Run:

```bash
devenv shell -- go test ./internal/router -v
devenv shell -- go test ./internal/router -fuzz=FuzzPolicyNeverSelectsBelowFloor -fuzztime=3s
```

Expected: PASS.

```bash
git add internal/router/cost.go internal/router/policy.go internal/router/policy_test.go internal/router/policy_fuzz_test.go
git commit -m "feat(router): select models by expected total cost"
```

### Task 4: Jev System One classifier

**Files:**

- Create: `internal/classifier/classifier.go`
- Create: `internal/classifier/jev/client.go`
- Create: `internal/classifier/jev/types.go`
- Create: `internal/classifier/jev/client_test.go`
- Modify: `internal/domain/features.go`

**Interfaces:**

- Consumes: Jev configuration from Task 1 and domain tiers/features from Task 2.
- Produces: `classifier.Classifier` with `Classify(context.Context, classifier.Input) (domain.JevJudgment, error)` and sentinel `classifier.ErrUnavailable`.

- [ ] **Step 1: Write the successful wire-contract test**

```go
func TestClientClassifyMapsTypedAnswers(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer secret" {
            t.Fatalf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
        }
        io.WriteString(w, `{"model":"jev-1.13.0","answers":{"minimum_tier":{"type":"choice","choice":"T4","probabilities":{"T4":0.9,"T5":0.1},"confidence":0.8},"task_type":{"type":"choice","choice":"coding","probabilities":{"coding":1},"confidence":1},"reasoning_required":{"type":"score","score":4,"legend":{"4":"average"},"probabilities":{"4":1},"confidence":1},"coding_required":{"type":"score","score":4,"legend":{"4":"normal"},"probabilities":{"4":1},"confidence":1},"blast_radius":{"type":"score","score":2,"legend":{"2":"moderate"},"probabilities":{"2":1},"confidence":1},"underspecified":{"type":"noul","noul":0.2}},"usage":{"input_tokens":120,"output_tokens":20}}`)
    }))
    defer server.Close()
    got, err := newTestClient(server.URL).Classify(context.Background(), sampleInput())
    if err != nil || got.MinimumTier != domain.T4 || got.ResolvedModel != "jev-1.13.0" {
        t.Fatalf("judgment=%+v err=%v", got, err)
    }
}
```

- [ ] **Step 2: Write bounded retry and malformed-answer tests**

```go
func TestClientRetries429And529ThenReturnsUnavailable(t *testing.T) {
    statuses := []int{429, 529, 529}
    client, calls := scriptedClient(statuses, 3)
    _, err := client.Classify(context.Background(), sampleInput())
    if !errors.Is(err, classifier.ErrUnavailable) || *calls != 3 {
        t.Fatalf("calls=%d err=%v", *calls, err)
    }
}

func TestClientRejectsPartialAnswerAndHonorsDeadline(t *testing.T) {
    client := malformedOrSlowClient(20 * time.Millisecond)
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
    defer cancel()
    _, err := client.Classify(ctx, sampleInput())
    if !errors.Is(err, classifier.ErrUnavailable) { t.Fatalf("err=%v", err) }
}
```

- [ ] **Step 3: Run tests and verify failure**

Run: `devenv shell -- go test ./internal/classifier/... -v`

Expected: FAIL with missing classifier declarations.

- [ ] **Step 4: Implement the direct HTTP client**

```go
type Classifier interface {
    Classify(context.Context, Input) (domain.JevJudgment, error)
}

type Input struct {
    Prompt string
    Features domain.RequestFeatures
    CurrentModel string
    AvailableModels []string
}
```

Add the classifier output to `internal/domain/features.go`:

```go
type JevJudgment struct {
    MinimumTier Tier
    TierConfidence float64
    TierProbabilities map[Tier]float64
    TaskType TaskType
    TaskTypeConfidence float64
    ReasoningScore, CodingScore, BlastRadius, Underspecified float64
    ResolvedModel string
    InputTokens, OutputTokens int64
    Latency time.Duration
}
```

Send one JSON request to `{base_url}/v1/systemone` containing the prompt and structured execution context, configured model, and six typed questions. Use Choice for tier/task type, Score for reasoning/coding/blast radius, and Noul for underspecification. Retry only 429, 529, and temporary transport errors with bounded exponential backoff. Never log the prompt or response body.

- [ ] **Step 5: Add the policy fallback integration test**

```go
func TestUnavailableClassifierFallsBackToT4(t *testing.T) {
    input := router.DecisionInput{Floor: domain.T0, Models: catalog(), Features: oneRequest()}
    input.Floor = router.FloorFromJudgment(nil, nil, domain.T4)
    got, err := testPolicy().Decide(input)
    if err != nil || got.Tier < domain.T4 { t.Fatalf("decision=%+v err=%v", got, err) }
}

func TestUnavailableClassifierPreservesConversationPin(t *testing.T) {
    pin := &router.Pin{ModelID:"pinned", Provider:"openai", Floor:domain.T5}
    floor := router.FloorFromJudgment(nil, pin, domain.T4)
    if floor != domain.T5 { t.Fatalf("floor=%s", floor) }
}
```

- [ ] **Step 6: Run tests and commit**

Run: `devenv shell -- go test ./internal/classifier/... ./internal/router -v`

Expected: PASS.

```bash
git add internal/classifier internal/domain internal/router
git commit -m "feat(router): classify routing requirements with Jev"
```

### Task 5: AES-GCM content keyring

**Files:**

- Create: `internal/contentcrypto/keyring.go`
- Create: `internal/contentcrypto/keyring_test.go`

**Interfaces:**

- Consumes: active key ID and decoded 32-byte keys from validated configuration.
- Produces: `contentcrypto.New(active string, keys map[string][]byte) (*Keyring, error)`, `(*Keyring).Encrypt([]byte) (Envelope, error)`, and `(*Keyring).Decrypt(Envelope) ([]byte, error)`.

- [ ] **Step 1: Write failing round-trip, nonce, rotation, and plaintext tests**

```go
func TestKeyringEncryptsWithUniqueNoncesAndRotates(t *testing.T) {
    oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
    oldRing, _ := New("old", map[string][]byte{"old": oldKey})
    first, _ := oldRing.Encrypt([]byte("secret prompt"))
    rotated, _ := New("new", map[string][]byte{"old": oldKey, "new": newKey})
    second, _ := rotated.Encrypt([]byte("secret prompt"))
    if first.KeyID != "old" || second.KeyID != "new" || bytes.Equal(first.Nonce, second.Nonce) {
        t.Fatalf("first=%+v second=%+v", first, second)
    }
    got, err := rotated.Decrypt(first)
    if err != nil || string(got) != "secret prompt" { t.Fatalf("got=%q err=%v", got, err) }
    if bytes.Contains(first.Ciphertext, []byte("secret prompt")) { t.Fatal("plaintext leaked") }
}
```

- [ ] **Step 2: Run the test and verify failure**

Run: `devenv shell -- go test ./internal/contentcrypto -v`

Expected: FAIL because `Keyring` does not exist.

- [ ] **Step 3: Implement versioned AES-256-GCM envelopes**

```go
type Envelope struct {
    Version uint8
    KeyID string
    Nonce, Ciphertext []byte
}

func (k *Keyring) Encrypt(plaintext []byte) (Envelope, error) {
    nonce := make([]byte, k.aead.NonceSize())
    if _, err := io.ReadFull(rand.Reader, nonce); err != nil { return Envelope{}, err }
    return Envelope{Version: 1, KeyID: k.active, Nonce: nonce,
        Ciphertext: k.aead.Seal(nil, nonce, plaintext, []byte(k.active))}, nil
}
```

Bind the key ID as additional authenticated data, copy key bytes on construction, reject non-32-byte keys, and return a typed unknown-key error without exposing ciphertext.

- [ ] **Step 4: Run tests and commit**

Run: `devenv shell -- go test ./internal/contentcrypto -v`

Expected: PASS.

```bash
git add internal/contentcrypto
git commit -m "feat(storage): encrypt captured content with key rotation"
```

### Task 6: SQLite migrations and routing repositories

**Files:**

- Create: `migrations/001_core.sql`
- Create: `internal/storage/storage.go`
- Create: `internal/storage/sqlite/db.go`
- Create: `internal/storage/sqlite/migrate.go`
- Create: `internal/storage/sqlite/repository.go`
- Create: `internal/storage/sqlite/repository_test.go`

**Interfaces:**

- Consumes: `contentcrypto.Keyring`, decisions from Task 3, and Jev judgments from Task 4.
- Produces: `storage.Repository` with transactional decision/content writes, `sqlite.Open(context.Context, Options) (*DB, error)`, and `(*DB).Ready(context.Context) error`.

- [ ] **Step 1: Write the failing migration and encrypted-content test**

```go
func TestRepositoryPersistsDecisionWithoutPlaintext(t *testing.T) {
    dbPath := filepath.Join(t.TempDir(), "router.db")
    db := openTestDB(t, dbPath)
    record := storage.RequestRecord{ID: "req_1", Prompt: []byte("private prompt"), Decision: sampleDecision()}
    if err := db.WithTx(context.Background(), func(tx storage.Tx) error { return tx.InsertRequest(record) }); err != nil {
        t.Fatal(err)
    }
    raw, _ := os.ReadFile(dbPath)
    if bytes.Contains(raw, []byte("private prompt")) { t.Fatal("plaintext present in database") }
    got, err := db.GetRequest(context.Background(), "req_1")
    if err != nil || string(got.Prompt) != "private prompt" { t.Fatalf("got=%+v err=%v", got, err) }
}
```

- [ ] **Step 2: Write the WAL, foreign-key, rollback, and unlimited-retention tests**

```go
func TestOpenEnablesSQLiteSafetyPragmas(t *testing.T) {
    db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
    assertPragma(t, db.SQL(), "journal_mode", "wal")
    assertPragma(t, db.SQL(), "foreign_keys", "1")
    assertPositivePragma(t, db.SQL(), "busy_timeout")
}

func TestDeleteExpiredContentZeroRetentionDeletesNothing(t *testing.T) {
    db := seededDB(t)
    n, err := db.DeleteExpiredContent(context.Background(), 0, time.Now().Add(24*time.Hour))
    if err != nil || n != 0 { t.Fatalf("deleted=%d err=%v", n, err) }
}
```

- [ ] **Step 3: Run tests and observe failure**

Run: `devenv shell -- go test ./internal/storage/... -v`

Expected: FAIL with missing storage packages.

- [ ] **Step 4: Add the embedded schema and transaction API**

`001_core.sql` must create `schema_migrations`, `requests`, `routing_decisions`, `candidate_scores`, `jev_judgments`, and `content_blobs`. Content rows store `key_id`, `version`, `nonce`, and `ciphertext`; decision rows store the price/prior snapshot and ordered reasons required for replay.

```go
type Repository interface {
    WithTx(context.Context, func(Tx) error) error
    GetRequest(context.Context, string) (RequestRecord, error)
    DeleteExpiredContent(context.Context, time.Duration, time.Time) (int64, error)
    Ready(context.Context) error
}

func Open(ctx context.Context, opts Options) (*DB, error) {
    sqlDB, err := sql.Open("sqlite", opts.Path)
    if err != nil { return nil, err }
    sqlDB.SetMaxOpenConns(1)
    // Execute foreign_keys=ON, journal_mode=WAL, and busy_timeout before migrations.
    return migrateAndWrap(ctx, sqlDB, opts.Keyring)
}
```

- [ ] **Step 5: Add the rollback test**

```go
func TestWithTxRollsBackAllDecisionRows(t *testing.T) {
    db := seededDB(t)
    err := db.WithTx(context.Background(), func(tx storage.Tx) error {
        if err := tx.InsertRequest(sampleRecord()); err != nil { return err }
        return errors.New("abort")
    })
    if err == nil || countRequests(t, db.SQL()) != 0 { t.Fatalf("err=%v", err) }
}
```

- [ ] **Step 6: Run storage tests and commit**

Run:

```bash
devenv shell -- go test ./internal/storage/... -v
devenv shell -- go test -race ./internal/storage/...
```

Expected: PASS.

```bash
git add migrations internal/storage
git commit -m "feat(storage): persist encrypted routing decisions in SQLite"
```

### Task 7: Application bootstrap and operational endpoints

**Files:**

- Create: `internal/api/operations.go`
- Create: `internal/api/operations_test.go`
- Create: `internal/app/app.go`
- Create: `internal/app/app_test.go`
- Create: `cmd/mindctl/main.go`

**Interfaces:**

- Consumes: validated config, keyring, SQLite DB, Jev classifier, and policy from Tasks 1-6.
- Produces: `app.New(context.Context, config.Config) (*app.App, error)`, `(*app.App).Handler() http.Handler`, and `(*app.App).Close() error`.

- [ ] **Step 1: Write failing health/readiness tests**

```go
func TestHealthAndReadiness(t *testing.T) {
    ready := atomic.Bool{}
    ready.Store(true)
    h := api.Operations(func(context.Context) error {
        if ready.Load() { return nil }
        return errors.New("sqlite unavailable")
    })
    assertStatus(t, h, "/healthz", http.StatusOK)
    assertStatus(t, h, "/readyz", http.StatusOK)
    ready.Store(false)
    assertStatus(t, h, "/healthz", http.StatusOK)
    assertStatus(t, h, "/readyz", http.StatusServiceUnavailable)
}
```

- [ ] **Step 2: Run the tests and verify failure**

Run: `devenv shell -- go test ./internal/api ./internal/app -v`

Expected: FAIL because the app and handlers do not exist.

- [ ] **Step 3: Implement dependency wiring and graceful shutdown**

```go
type App struct {
    server *http.Server
    store  storage.Repository
    handler http.Handler
}

func (a *App) Handler() http.Handler { return a.handler }

func Operations(ready func(context.Context) error) http.Handler {
    mux := http.NewServeMux()
    mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status":"ok"}) })
    mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
        if err := ready(r.Context()); err != nil { writeJSON(w, 503, map[string]string{"status":"unavailable"}); return }
        writeJSON(w, 200, map[string]string{"status":"ready"})
    })
    return mux
}
```

`main` must accept `-config`, load secrets through `os.LookupEnv`, start the server, handle SIGINT/SIGTERM, and shut down with a bounded context. Startup errors go to stderr without secret values.

- [ ] **Step 4: Add the startup failure test**

```go
func TestNewFailsWhenEncryptionKeyIsMissing(t *testing.T) {
    cfg := validConfigWithMissingKey()
    _, err := app.New(context.Background(), cfg)
    if err == nil || !strings.Contains(err.Error(), "encryption") { t.Fatalf("err=%v", err) }
}
```

- [ ] **Step 5: Run the first-increment verification suite**

Run:

```bash
devenv shell -- go test ./...
devenv shell -- go test -race ./...
devenv shell -- go vet ./...
```

Expected: PASS.

- [ ] **Step 6: Smoke-test the binary with temporary configuration**

Run: `devenv shell -- go run ./cmd/mindctl -config config.example.yaml`

Expected: startup either succeeds with documented test environment values or fails once with a precise missing-environment-variable message; it must never print secret contents.

- [ ] **Step 7: Commit the runnable core**

```bash
git add cmd/mindctl internal/api internal/app
git commit -m "feat(router): bootstrap the Go gateway core"
```
