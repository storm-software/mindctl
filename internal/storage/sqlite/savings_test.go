package sqlite

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/savings"
	"github.com/storm-software/mindctl/internal/storage"
)

func succeededAttempt(t *testing.T, svc conversation.Service, model string, decision router.Decision) (conversation.Turn, conversation.Attempt) {
	t.Helper()
	ctx := context.Background()
	turn, err := svc.Start(ctx, "client", inference.Request{
		Model: model,
		Input: []inference.Item{{Type: "message", Role: "user", Text: "request"}},
	})

	if err != nil {
		t.Fatal(err)
	}

	attempt, err := svc.BeginAttempt(ctx, turn, decision)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.CommitResult(ctx, turn, router.Pin{Provider: decision.Provider, ModelID: decision.ModelID, Floor: decision.Tier},
		inference.Result{Status: "completed", Output: []inference.Item{{Type: "message", Role: "assistant", Text: "response"}}}); err != nil {
		t.Fatal(err)
	}

	return turn, attempt
}

func TestSummarizeSavingsAggregatesPerServingModel(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "savings.db"))
	svc := conversation.New(db)
	ctx := context.Background()
	small := router.Decision{Provider: "openai", ModelID: "gpt-small", Tier: domain.T2}
	large := router.Decision{Provider: "anthropic", ModelID: "claude-large", Tier: domain.T5}

	records := []struct {
		model    string
		decision router.Decision
		record   savings.Record
	}{
		{inference.AutomaticModel, small, savings.Record{
			InputTokens: 100, CachedInputTokens: 40, OutputTokens: 10, ActualCost: 1, BaselineCost: 5,
			BaselineProvider: "anthropic", BaselineModelID: "claude-large",
			CompressionTokensBefore: 200, CompressionTokensSaved: 50, CompressionSavings: 0.25,
		}},
		{inference.AutomaticModel, small, savings.Record{InputTokens: 50, OutputTokens: 5, ActualCost: 0.5, BaselineCost: 2}},
		{"claude-large", large, savings.Record{InputTokens: 10, OutputTokens: 1, ActualCost: 3, BaselineCost: 3}},
	}
	var attempts []conversation.Attempt
	for _, entry := range records {
		_, attempt := succeededAttempt(t, svc, entry.model, entry.decision)
		if err := svc.RecordSavings(ctx, attempt, entry.record); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, attempt)
	}
	// A retried telemetry write must not double count an attempt.
	if err := svc.RecordSavings(ctx, attempts[0], records[0].record); err != nil {
		t.Fatal(err)
	}

	summary, err := db.SummarizeSavings(ctx, storage.SavingsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	total := summary.Total
	if total.Attempts != 3 || total.AutomaticAttempts != 2 || total.InputTokens != 160 || total.CachedInputTokens != 40 ||
		total.OutputTokens != 16 || total.CompressionTokensBefore != 200 || total.CompressionTokensSaved != 50 {
		t.Fatalf("total = %+v", total)
	}
	if math.Abs(total.RoutingSavings()-5.5) > 1e-9 || math.Abs(total.TotalSavings()-5.75) > 1e-9 {
		t.Fatalf("savings = %v routing, %v total", total.RoutingSavings(), total.TotalSavings())
	}
	if len(summary.Models) != 2 || summary.Models[0].ModelID != "gpt-small" || summary.Models[0].Attempts != 2 ||
		summary.Models[1].ModelID != "claude-large" || summary.Models[1].RoutingSavings() != 0 {
		t.Fatalf("models = %+v", summary.Models)
	}

	explicit := true
	filtered, err := db.SummarizeSavings(ctx, storage.SavingsFilter{ExplicitModel: &explicit})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total.Attempts != 1 || len(filtered.Models) != 1 || filtered.Models[0].ModelID != "claude-large" {
		t.Fatalf("explicit summary = %+v", filtered)
	}
	filtered, err = db.SummarizeSavings(ctx, storage.SavingsFilter{Provider: "openai", Since: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total.Attempts != 0 || len(filtered.Models) != 0 {
		t.Fatalf("future summary = %+v", filtered)
	}

	// Savings are content-free telemetry and survive content retention.
	if _, err := db.DeleteExpiredContent(ctx, time.Nanosecond, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	retained, err := db.SummarizeSavings(ctx, storage.SavingsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if retained.Total != total {
		t.Fatalf("retained total = %+v, want %+v", retained.Total, total)
	}
}

func TestRecordSavingsRequiresSucceededAttemptOfClient(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "savings.db"))
	svc := conversation.New(db)
	ctx := context.Background()
	turn, err := svc.Start(ctx, "client", inference.Request{
		Model: inference.AutomaticModel,
		Input: []inference.Item{{Type: "message", Role: "user", Text: "request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.BeginAttempt(ctx, turn, router.Decision{Provider: "openai", ModelID: "gpt-small", Tier: domain.T2})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordSavings(ctx, attempt, savings.Record{}); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("started attempt savings error = %v", err)
	}

	_, succeeded := succeededAttempt(t, svc, inference.AutomaticModel, router.Decision{Provider: "openai", ModelID: "gpt-small", Tier: domain.T2})
	foreign := succeeded
	foreign.ClientID = "other-client"
	if err := svc.RecordSavings(ctx, foreign, savings.Record{}); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("foreign client savings error = %v", err)
	}
}
