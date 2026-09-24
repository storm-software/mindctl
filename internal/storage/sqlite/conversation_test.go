package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage"
)

func TestListHistoryReturnsNewestRequestsWithAttemptsAndDecryptedContent(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "history.db"))
	svc := conversation.New(db)
	ctx := context.Background()

	first, err := svc.Start(ctx, "client-a", inference.Request{
		Input: []inference.Item{{Type: "message", Role: "user", Text: "first request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := svc.BeginAttempt(ctx, first, router.Decision{
		Provider: "openai", ModelID: "gpt-small", Tier: domain.T2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.FailAttempt(ctx, failed, "upstream-failed", errors.New("retryable failure")); err != nil {
		t.Fatal(err)
	}
	succeeded, err := svc.BeginAttempt(ctx, first, router.Decision{
		Provider: "anthropic", ModelID: "claude-large", Tier: domain.T4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(ctx, first, router.Pin{
		Provider: "anthropic", ModelID: "claude-large", Floor: domain.T4,
	}, inference.Result{
		ProviderRequestID: "upstream-success",
		Status:            "completed",
		Output: []inference.Item{{
			Type: "message", Role: "assistant", Text: "first response",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	second, err := svc.Start(ctx, "client-b", inference.Request{
		Input: []inference.Item{{Type: "message", Role: "user", Text: "second request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondAttempt, err := svc.BeginAttempt(ctx, second, router.Decision{
		Provider: "openai", ModelID: "gpt-large", Tier: domain.T4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(ctx, second, router.Pin{
		Provider: "openai", ModelID: "gpt-large", Floor: domain.T4,
	}, inference.Result{
		ProviderRequestID: "upstream-second",
		Status:            "completed",
		Output: []inference.Item{{
			Type: "message", Role: "assistant", Text: "second response",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	firstCreated := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	secondCreated := firstCreated.Add(time.Hour)
	for _, update := range []struct {
		responseID string
		created    time.Time
	}{{first.ResponseID, firstCreated}, {second.ResponseID, secondCreated}} {
		if _, err := db.SQL().Exec(
			"UPDATE responses SET created_at = ? WHERE id = ?",
			update.created.UnixNano(),
			update.responseID,
		); err != nil {
			t.Fatal(err)
		}
	}

	records, err := db.ListHistory(ctx, storage.HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("history length = %d; want 2", len(records))
	}
	if records[0].ResponseID != second.ResponseID || records[1].ResponseID != first.ResponseID {
		t.Fatalf("response order = [%q %q]", records[0].ResponseID, records[1].ResponseID)
	}
	if got := records[1].Request[0].Text; got != "first request" {
		t.Fatalf("request text = %q", got)
	}
	if len(records[1].Attempts) != 2 {
		t.Fatalf("attempt count = %d; want 2", len(records[1].Attempts))
	}
	if got := records[1].Attempts[0]; got.ID != failed.ID || got.Status != "failed" ||
		got.Provider != "openai" || got.ModelID != "gpt-small" || string(got.Error) != "retryable failure" {
		t.Fatalf("failed attempt = %#v", got)
	}
	if got := records[1].Attempts[1]; got.ID != succeeded.ID || got.Status != "succeeded" ||
		got.Result == nil || got.Result.Output[0].Text != "first response" {
		t.Fatalf("successful attempt = %#v", got)
	}
	if got := records[0].Attempts[0]; got.ID != secondAttempt.ID || got.Result == nil ||
		got.Result.Output[0].Text != "second response" {
		t.Fatalf("second attempt = %#v", got)
	}
}

func TestListHistoryCombinesAttemptAndTimeFiltersAndLimit(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "history-filter.db"))
	ctx := context.Background()
	created := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	for index, fixture := range []struct {
		responseID string
		provider   string
		model      string
		status     string
	}{
		{"resp_old", "openai", "gpt-test", "failed"},
		{"resp_match", "openai", "gpt-test", "succeeded"},
		{"resp_other", "anthropic", "claude-test", "succeeded"},
	} {
		conversationID := "conv_" + fixture.responseID
		attemptID := "attempt_" + fixture.responseID
		stamp := created.Add(time.Duration(index) * time.Hour).UnixNano()
		if _, err := db.SQL().Exec(
			"INSERT INTO conversations (id, client_id, created_at, escalation_floor) VALUES (?, 'client', ?, 0)",
			conversationID,
			stamp,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL().Exec(
			"INSERT INTO responses (id, conversation_id, sequence, created_at, status) VALUES (?, ?, 0, ?, 'completed')",
			fixture.responseID,
			conversationID,
			stamp,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL().Exec(
			`INSERT INTO provider_attempts
				(id, response_id, sequence, provider, model_id, tier, decision_json, status, created_at)
				VALUES (?, ?, 0, ?, ?, 2, '{}', ?, ?)`,
			attemptID,
			fixture.responseID,
			fixture.provider,
			fixture.model,
			fixture.status,
			stamp,
		); err != nil {
			t.Fatal(err)
		}
	}

	records, err := db.ListHistory(ctx, storage.HistoryFilter{
		Provider: "openai",
		ModelID:  "gpt-test",
		Status:   "succeeded",
		Since:    created.Add(30 * time.Minute),
		Until:    created.Add(3 * time.Hour),
		Limit:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ResponseID != "resp_match" {
		t.Fatalf("filtered history = %#v", records)
	}
}

func TestConversationPersistsEncryptedProviderBodiesAndFiltersOpaqueData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.db")
	db := openTestDB(t, path)
	svc := conversation.New(db)
	request := inference.Request{Model: "mindctl-auto", Input: []inference.Item{{Type: "message", Role: "user", Text: "private transcript sentinel"}}}
	turn, err := svc.Start(context.Background(), "client-a", request)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.BeginAttempt(context.Background(), turn, router.Decision{Provider: "openai", ModelID: "gpt-test", Tier: domain.T3})
	if err != nil {
		t.Fatal(err)
	}
	opaque := json.RawMessage(`{"provider":"opaque continuation sentinel"}`)
	result := inference.Result{ProviderRequestID: "upstream-1", Output: []inference.Item{{Type: "message", Role: "assistant", Text: "private result sentinel", ProviderData: opaque}}}
	if err := svc.CommitResult(context.Background(), turn, router.Pin{Provider: "openai", ModelID: "gpt-test", Floor: domain.T3}, result); err != nil {
		t.Fatal(err)
	}
	var status, keyID string
	var cipherSize int
	if err := db.SQL().QueryRow("SELECT status, key_id, length(ciphertext) FROM provider_attempts WHERE id = ?", attempt.ID).Scan(&status, &keyID, &cipherSize); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || keyID != "key-1" || cipherSize == 0 {
		t.Fatalf("attempt status=%q key=%q ciphertext=%d", status, keyID, cipherSize)
	}
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		for _, sentinel := range []string{"private transcript sentinel", "private result sentinel", "opaque continuation sentinel"} {
			if bytes.Contains(raw, []byte(sentinel)) {
				t.Fatalf("plaintext %q present in %s", sentinel, suffix)
			}
		}
	}
	resumed, err := svc.Resume(context.Background(), "client-a", turn.ResponseID)
	if err != nil {
		t.Fatal(err)
	}
	if got := resumed.Transcript[1].ProviderData; got != nil {
		t.Fatalf("portable transcript exposed opaque data: %s", got)
	}
	if got := resumed.TranscriptFor("openai")[1].ProviderData; !bytes.Equal(got, opaque) {
		t.Fatalf("same-provider data=%s", got)
	}
	if got := resumed.TranscriptFor("anthropic")[1].ProviderData; got != nil {
		t.Fatalf("cross-provider opaque data leaked: %s", got)
	}
}

func TestFailedAttemptPersistsProviderRequestID(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "failed-attempt.db"))
	svc := conversation.New(db)
	turn, err := svc.Start(context.Background(), "client-a", inference.Request{Model: "mindctl-auto", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.BeginAttempt(context.Background(), turn, router.Decision{Provider: "openai", ModelID: "gpt-test", Tier: domain.T3})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.FailAttempt(context.Background(), attempt, "upstream-failed-1", context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	var requestID string
	if err := db.SQL().QueryRow("SELECT provider_request_id FROM provider_attempts WHERE id = ?", attempt.ID).Scan(&requestID); err != nil || requestID != "upstream-failed-1" {
		t.Fatalf("request_id=%q err=%v", requestID, err)
	}
}

func TestDeleteExpiredContentRemovesConversationCiphertextButRetainsAttemptTelemetry(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "retention.db"))
	now := time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)

	legacy := sampleRecord()
	legacy.ID = "req_retention"
	legacy.CreatedAt = old
	insertRecord(t, db, legacy)

	svc := conversation.New(db)
	turn, err := svc.Start(context.Background(), "client-a", inference.Request{Model: "mindctl-auto", Input: []inference.Item{{Type: "message", Role: "user", Text: "expired transcript"}}})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.BeginAttempt(context.Background(), turn, router.Decision{Provider: "openai", ModelID: "gpt-test", Tier: domain.T3})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(context.Background(), turn, router.Pin{Provider: "openai", ModelID: "gpt-test", Floor: domain.T3}, inference.Result{ProviderRequestID: "upstream-retention", Status: "completed", Output: []inference.Item{{Type: "message", Role: "assistant", Text: "expired result"}}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"transcript_items", "provider_attempts"} {
		if _, err := db.SQL().Exec("UPDATE "+table+" SET created_at = ?", old.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := db.DeleteExpiredContent(context.Background(), time.Hour, now)
	if err != nil || removed != 8 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	assertCount(t, db.SQL(), "content_blobs", 0)
	assertCount(t, db.SQL(), "transcript_items", 0)

	var status, requestID, decision string
	var keyMissing, versionMissing, nonceMissing, ciphertextMissing int
	if err := db.SQL().QueryRow(`SELECT status, provider_request_id, decision_json,
		key_id IS NULL, version IS NULL, nonce IS NULL, ciphertext IS NULL
		FROM provider_attempts WHERE id = ?`, attempt.ID).Scan(
		&status, &requestID, &decision, &keyMissing, &versionMissing, &nonceMissing, &ciphertextMissing,
	); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || requestID != "upstream-retention" || decision == "" || keyMissing != 1 || versionMissing != 1 || nonceMissing != 1 || ciphertextMissing != 1 {
		t.Fatalf("status=%q requestID=%q decision=%q content-null=%d/%d/%d/%d", status, requestID, decision, keyMissing, versionMissing, nonceMissing, ciphertextMissing)
	}
}
