package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage"
)

func testKeyring(t *testing.T) *contentcrypto.Keyring {
	t.Helper()
	k, err := contentcrypto.New("key-1", map[string][]byte{"key-1": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func openTestDB(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(context.Background(), Options{Path: path, Keyring: testKeyring(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sampleRecord() storage.RequestRecord {
	return storage.RequestRecord{
		ID: "req_1", CreatedAt: time.Date(2026, 9, 21, 12, 0, 0, 123, time.UTC),
		Prompt: []byte("private prompt sentinel"), Answer: []byte("private answer sentinel"),
		RejectedOutputs: [][]byte{[]byte("private rejected sentinel"), []byte("private second rejection sentinel")},
		RawContent:      []byte("private classifier raw response sentinel"),
		Decision: router.Decision{
			Tier: domain.T3, ModelID: "winner", Provider: "provider",
			Reasons: []string{"request floor: T3", "selected provider/winner at T3"},
			Candidates: []router.CandidateScore{
				{ModelID: "winner", Provider: "provider", DirectCost: .04, SuccessProbability: .9, FailureProbability: .1, EscalationCost: .2, LatencyPenalty: .01, ExpectedTotalCost: .07, Latency: time.Second},
				{ModelID: "runner-up", Provider: "provider", DirectCost: .05, SuccessProbability: .8, FailureProbability: .2, EscalationCost: .2, LatencyPenalty: .02, ExpectedTotalCost: .11, Latency: 2 * time.Second},
			},
			Rejections: []router.Rejection{{ModelID: "small", Code: router.RejectTier, Codes: []router.RejectionCode{router.RejectTier, router.RejectContext}, Reasons: []string{"below floor", "context too small"}}},
		},
		Judgment: &domain.ClassifierJudgment{MinimumTier: domain.T3, TierConfidence: .9, TierProbabilities: map[domain.Tier]float64{domain.T3: .9, domain.T4: .1}, TaskType: domain.TaskCoding, TaskTypeConfidence: .8, TaskTypeProbabilities: map[domain.TaskType]float64{domain.TaskCoding: .8, domain.TaskReasoning: .2}, CodingScore: 3, ReasoningScore: 2, BlastRadius: 1, Underspecified: .1, Classifier: "jev", ResolvedModel: "jev-pinned", Latency: time.Millisecond},
		Replay: storage.ReplaySnapshot{
			Input: router.DecisionInput{
				Features: domain.RequestFeatures{InputTokens: 100, CachedInputTokens: 20, MaxOutputTokens: 50, NeedsText: true, HostedToolTypes: []string{"search"}},
				Floor:    domain.T3, TaskType: domain.TaskCoding,
				Models:              []domain.Model{{ID: "winner", Provider: "provider", Tier: domain.T3, Pricing: domain.Pricing{InputPerMillion: 1, CachedInputPerMillion: .1, OutputPerMillion: 3, PerRequestUSD: .01}, SuccessPriors: map[domain.TaskType]float64{domain.TaskCoding: .9}, DefaultSuccessPrior: .8, Capabilities: domain.Capabilities{Text: true, HostedTools: map[string]bool{"search": true}}, Available: true}},
				ProviderCredentials: map[string]bool{"provider": true}, ProviderAvailability: map[string]bool{"provider": true},
			},
			Policy: router.PolicyConfig{FailureEscalationCost: .2, LatencyPenaltyPerSecond: .01, ReasoningFloors: []router.SignalFloor{{Threshold: 4, Floor: domain.T4}}},
		},
	}
}

func insertRecord(t *testing.T, db *DB, record storage.RequestRecord) {
	t.Helper()
	if err := db.WithTx(context.Background(), func(tx storage.Tx) error { return tx.InsertRequest(record) }); err != nil {
		t.Fatal(err)
	}
}

// Removing encryption or serializing the entire record into telemetry must fail.
func TestRepositoryPersistsDecisionWithoutPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	db := openTestDB(t, path)
	record := sampleRecord()
	insertRecord(t, db, record)
	got, err := db.GetRequest(context.Background(), record.ID)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("round trip differs (content omitted): err=%v equal=%v", err, reflect.DeepEqual(got, record))
	}
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		for _, content := range [][]byte{record.Prompt, record.Answer, record.RawContent, record.RejectedOutputs[0], record.RejectedOutputs[1]} {
			if bytes.Contains(raw, content) {
				t.Fatalf("plaintext present in %q", suffix)
			}
		}
	}
	var key string
	var version, nonceSize, cipherSize int
	if err := db.SQL().QueryRow("SELECT key_id, version, length(nonce), length(ciphertext) FROM content_blobs WHERE kind = 'prompt'").Scan(&key, &version, &nonceSize, &cipherSize); err != nil {
		t.Fatal(err)
	}
	if key != "key-1" || version != 1 || nonceSize != 12 || cipherSize <= len(record.Prompt) {
		t.Fatal("content envelope was not stored")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestDB(t, path)
	got, err = reopened.GetRequest(context.Background(), record.ID)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("reopen lost data: %v", err)
	}
	assertCount(t, reopened.SQL(), "schema_migrations", 2)
}

func TestRepositoryRoundTripsClassifierScoreConfidences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	db := openTestDB(t, path)
	record := sampleRecord()
	record.Judgment.ReasoningConfidence = .61
	record.Judgment.CodingConfidence = .72
	record.Judgment.BlastRadiusConfidence = .83
	insertRecord(t, db, record)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestDB(t, path)
	got, err := reopened.GetRequest(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Judgment == nil || got.Judgment.ReasoningConfidence != .61 || got.Judgment.CodingConfidence != .72 || got.Judgment.BlastRadiusConfidence != .83 {
		t.Fatalf("score confidences were not preserved: %+v", got.Judgment)
	}
}

func TestRepositoryReplaysActualPolicyDecisionAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	db := openTestDB(t, path)
	minimum, maximum := domain.T3, domain.T6
	input := router.DecisionInput{
		Features: domain.RequestFeatures{InputTokens: 2_000, CachedInputTokens: 500, MaxOutputTokens: 200, NeedsText: true},
		Models: []domain.Model{
			{ID: "cheap", Provider: "provider", Tier: domain.T4, ContextWindow: 8_192, MaxOutputTokens: 1_024, Capabilities: domain.Capabilities{Text: true}, Pricing: domain.Pricing{InputPerMillion: .1, CachedInputPerMillion: .01, OutputPerMillion: .5, PerRequestUSD: .0001}, SuccessPriors: map[domain.TaskType]float64{domain.TaskCoding: .7}, DefaultSuccessPrior: .7, LatencyP95: 200 * time.Millisecond, Available: true, Order: 0},
			{ID: "reliable", Provider: "provider", Tier: domain.T5, ContextWindow: 8_192, MaxOutputTokens: 1_024, Capabilities: domain.Capabilities{Text: true}, Pricing: domain.Pricing{InputPerMillion: .2, CachedInputPerMillion: .02, OutputPerMillion: 1, PerRequestUSD: .0002}, SuccessPriors: map[domain.TaskType]float64{domain.TaskCoding: .99}, DefaultSuccessPrior: .99, LatencyP95: 100 * time.Millisecond, Available: true, Order: 1},
		},
		Floor: domain.T3, TaskType: domain.TaskCoding, MinTier: &minimum, MaxTier: &maximum,
		Judgment:            &domain.ClassifierJudgment{MinimumTier: domain.T4, TierConfidence: .8, ReasoningScore: 4, ReasoningConfidence: .61, CodingConfidence: .72, BlastRadiusConfidence: .83},
		ProviderCredentials: map[string]bool{"provider": true}, ProviderAvailability: map[string]bool{"provider": true},
	}
	policy := router.PolicyConfig{
		FailureEscalationCost: .01, LatencyPenaltyPerSecond: .001, MinClassifierConfidence: .7,
		ReasoningFloors: []router.SignalFloor{{Threshold: 4, Floor: domain.T5}},
	}
	decision, err := router.NewPolicy(policy).Decide(input)
	if err != nil {
		t.Fatal(err)
	}
	record := storage.RequestRecord{ID: "replay", Decision: decision, Replay: storage.ReplaySnapshot{Input: input, Policy: policy}}
	insertRecord(t, db, record)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestDB(t, path)
	got, err := reopened.GetRequest(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := router.NewPolicy(got.Replay.Policy).Decide(got.Replay.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, got.Decision) {
		t.Fatalf("replayed decision differs\n got: %+v\nwant: %+v", replayed, got.Decision)
	}
}

// Mutating callers' or readers' nested maps/slices must not change stored replay.
func TestGetRequestReturnsIndependentSnapshots(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
	record := sampleRecord()
	insertRecord(t, db, record)
	record.Prompt[0] = 'X'
	record.Decision.Reasons[0] = "changed"
	record.Replay.Input.Models[0].SuccessPriors[domain.TaskCoding] = 0
	got, err := db.GetRequest(context.Background(), record.ID)
	if err != nil || !reflect.DeepEqual(got, sampleRecord()) {
		t.Fatalf("caller mutation changed record: %v", err)
	}
	got.Prompt[0] = 'X'
	got.RejectedOutputs[0][0] = 'X'
	got.Decision.Rejections[0].Reasons[0] = "changed"
	got.Decision.Candidates[0].ExpectedTotalCost = 99
	got.Judgment.TierProbabilities[domain.T3] = 0
	got.Replay.Input.Models[0].SuccessPriors[domain.TaskCoding] = 0
	again, err := db.GetRequest(context.Background(), record.ID)
	if err != nil || !reflect.DeepEqual(again, sampleRecord()) {
		t.Fatalf("reader mutation changed record: %v", err)
	}
}

func assertCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var count int
	// table names are test-owned constants, never application input.
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
		t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
	}
}

func assertPragmas(t *testing.T, db *DB) {
	t.Helper()
	var mode string
	var fk, timeout int
	if err := db.SQL().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal=%q err=%v", mode, err)
	}
	if err := db.SQL().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys=%d err=%v", fk, err)
	}
	if err := db.SQL().QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout <= 0 {
		t.Fatalf("busy_timeout=%d err=%v", timeout, err)
	}
}

// DSN initialization must survive physical connection replacement, not just Open.
func TestOpenEnablesSQLiteSafetyPragmas(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "router ?# params.db"))
	assertPragmas(t, db)
	db.SQL().SetMaxIdleConns(0)
	assertPragmas(t, db)
	if err := db.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := db.SQL().Exec("INSERT INTO content_blobs (request_id, kind, position, key_id, version, nonce, ciphertext, created_at) VALUES ('missing', 'prompt', 0, 'key-1', 1, X'00', X'00', 0)")
	if err == nil {
		t.Fatal("orphan content accepted")
	}
}

// Readiness must report broken safety settings without silently repairing them.
func TestReadyChecksPragmasWithoutMutation(t *testing.T) {
	for _, pragma := range []string{"foreign_keys = OFF", "busy_timeout = 0", "journal_mode = DELETE"} {
		t.Run(pragma, func(t *testing.T) {
			db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
			if _, err := db.SQL().Exec("PRAGMA " + pragma); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := db.Ready(context.Background()); err == nil {
					t.Fatal("unsafe database reported ready")
				}
			}
		})
	}
	db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
	_ = db.Close()
	if err := db.Ready(context.Background()); err == nil {
		t.Fatal("closed database reported ready")
	}
}

func TestWithTxRollsBackAllDecisionRows(t *testing.T) {
	for _, failure := range []string{"callback", "insert", "commit", "cancel", "panic"} {
		t.Run(failure, func(t *testing.T) {
			db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
			if failure == "commit" {
				_, err := db.SQL().Exec(`CREATE TABLE deferred_check (request_id TEXT REFERENCES requests(id) DEFERRABLE INITIALLY DEFERRED);
					CREATE TRIGGER fail_commit AFTER INSERT ON requests BEGIN INSERT INTO deferred_check VALUES ('missing'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			abort := errors.New("private callback error sentinel")
			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				err = db.WithTx(ctx, func(tx storage.Tx) error {
					if err := tx.InsertRequest(sampleRecord()); err != nil {
						return err
					}
					switch failure {
					case "callback":
						return abort
					case "insert":
						// Even an ignored write failure must prevent a partial commit.
						_ = tx.InsertRequest(sampleRecord())
					case "cancel":
						cancel()
					case "panic":
						panic(abort)
					}
					return nil
				})
			}()
			if failure == "panic" {
				if recovered != abort {
					t.Fatal("callback panic was swallowed")
				}
			} else if err == nil {
				t.Fatal("transaction unexpectedly committed")
			}
			if failure == "callback" && (!errors.Is(err, abort) || strings.Contains(err.Error(), "private callback")) {
				t.Fatal("callback error was leaked or lost its identity")
			}
			for _, table := range []string{"requests", "routing_decisions", "candidate_scores", "jev_judgments", "content_blobs"} {
				assertCount(t, db.SQL(), table, 0)
			}
			if failure == "commit" {
				if _, err := db.SQL().Exec("DROP TRIGGER fail_commit"); err != nil {
					t.Fatal(err)
				}
			}
			insertRecord(t, db, sampleRecord())
		})
	}
}

// A later migration failure must undo its DDL and its ledger entry.
func TestMigrationsAreOrderedTransactionalAndIdempotent(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
	files := fstest.MapFS{
		"003_finish.sql": {Data: []byte("INSERT INTO second_migration VALUES (7)")},
		"002_second.sql": {Data: []byte("CREATE TABLE second_migration (value INTEGER)")},
	}
	for range 2 {
		if err := migrate(context.Background(), db.SQL(), files); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, db.SQL(), "second_migration", 1)
	assertCount(t, db.SQL(), "schema_migrations", 4)
	bad := fstest.MapFS{"004_bad.sql": {Data: []byte("CREATE TABLE incomplete (value INTEGER); INVALID SQL")}}
	if err := migrate(context.Background(), db.SQL(), bad); err == nil {
		t.Fatal("bad migration accepted")
	}
	assertCount(t, db.SQL(), "schema_migrations", 4)
	var count int
	if err := db.SQL().QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'incomplete'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration left DDL: count=%d err=%v", count, err)
	}
}

func TestOpenFailsOnMigrationError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE requests (wrong_column TEXT)"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	db, err := Open(context.Background(), Options{Path: path, Keyring: testKeyring(t)})
	if err == nil || db != nil {
		if db != nil {
			_ = db.Close()
		}
		t.Fatal("migration failure did not fail startup")
	}
}

// Strictly older than now-retention expires; zero retention is a no-op.
func TestDeleteExpiredContentPreservesTelemetryAndBoundary(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for index, age := range []time.Duration{25 * time.Hour, 24 * time.Hour, 23 * time.Hour} {
		record := sampleRecord()
		record.ID = fmt.Sprintf("req_%d", index)
		record.CreatedAt = now.Add(-age)
		insertRecord(t, db, record)
	}
	n, err := db.DeleteExpiredContent(context.Background(), 0, now.Add(100*24*time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("unlimited retention deleted=%d err=%v", n, err)
	}
	assertCount(t, db.SQL(), "content_blobs", 15)
	n, err = db.DeleteExpiredContent(context.Background(), 24*time.Hour, now)
	if err != nil || n != 5 {
		t.Fatalf("expired content deleted=%d err=%v", n, err)
	}
	assertCount(t, db.SQL(), "content_blobs", 10)
	for _, table := range []string{"requests", "routing_decisions", "jev_judgments"} {
		assertCount(t, db.SQL(), table, 3)
	}
	assertCount(t, db.SQL(), "candidate_scores", 6)
	got, err := db.GetRequest(context.Background(), "req_0")
	if err != nil || got.Prompt != nil || got.Answer != nil || got.RawContent != nil || len(got.RejectedOutputs) != 0 || !reflect.DeepEqual(got.Decision, sampleRecord().Decision) || !reflect.DeepEqual(got.Replay, sampleRecord().Replay) {
		t.Fatalf("retention damaged telemetry or retained content: %v", err)
	}
	for _, id := range []string{"req_1", "req_2"} {
		got, err := db.GetRequest(context.Background(), id)
		if err != nil || !bytes.Equal(got.Prompt, sampleRecord().Prompt) {
			t.Fatalf("retention removed boundary/new content: %v", err)
		}
	}
	if _, err := db.DeleteExpiredContent(context.Background(), -time.Second, now); err == nil {
		t.Fatal("negative retention accepted")
	}
}

func TestContentAuthenticationFailureReturnsNoPartialRecord(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
	insertRecord(t, db, sampleRecord())
	if _, err := db.SQL().Exec("UPDATE content_blobs SET ciphertext = X'00' WHERE kind = 'answer'"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRequest(context.Background(), "req_1")
	if err == nil || !reflect.DeepEqual(got, storage.RequestRecord{}) {
		t.Fatal("corrupted content returned a partial record")
	}
}

func TestRepositoryMissingRecordAndEscapedTransaction(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
	if _, err := db.GetRequest(context.Background(), "missing ' SQL sentinel"); !errors.Is(err, storage.ErrNotFound) || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("missing record error=%v", err)
	}
	var escaped storage.Tx
	if err := db.WithTx(context.Background(), func(tx storage.Tx) error { escaped = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := escaped.InsertRequest(sampleRecord()); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("escaped transaction err=%v", err)
	}
	assertCount(t, db.SQL(), "requests", 0)
}

func TestConcurrentTransactionsAndReads(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "router.db"))
	var wg sync.WaitGroup
	for index := range 12 {
		wg.Go(func() {
			record := sampleRecord()
			record.ID = fmt.Sprintf("req_%d", index)
			err := db.WithTx(context.Background(), func(tx storage.Tx) error { return tx.InsertRequest(record) })
			if err != nil {
				t.Error(err)
				return
			}
			got, err := db.GetRequest(context.Background(), record.ID)
			if err != nil || !reflect.DeepEqual(got, record) {
				t.Errorf("concurrent round trip failed: %v", err)
			}
		})
	}
	wg.Wait()
	assertCount(t, db.SQL(), "requests", 12)
}

func TestOpenRejectsMissingKeyAndUnusablePath(t *testing.T) {
	for _, opts := range []Options{{Path: filepath.Join(t.TempDir(), "no-key.db")}, {Keyring: testKeyring(t)}, {Path: t.TempDir(), Keyring: testKeyring(t)}} {
		db, err := Open(context.Background(), opts)
		if err == nil || db != nil {
			if db != nil {
				_ = db.Close()
			}
			t.Fatal("invalid options succeeded")
		}
	}
}
