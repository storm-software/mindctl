package conversation_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
)

func testConversationService(t *testing.T) conversation.Service {
	t.Helper()
	keys, err := contentcrypto.New("test", map[string][]byte{"test": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: filepath.Join(t.TempDir(), "conversation.db"), Keyring: keys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return conversation.New(db)
}

func requestWithText(text string) inference.Request {
	return inference.Request{Model: "mindctl-auto", Input: []inference.Item{{Type: "message", Role: "user", Text: text}}}
}

func resultWithText(text string) inference.Result {
	return inference.Result{Model: "gpt-test", ProviderRequestID: "upstream-1", Status: "completed", Output: []inference.Item{{Type: "message", Role: "assistant", Text: text}}}
}

func pin(provider, model string, floor domain.Tier) router.Pin {
	return router.Pin{Provider: provider, ModelID: model, Floor: floor}
}

func transcriptText(items []inference.Item) string {
	text := make([]string, 0, len(items))
	for _, item := range items {
		if item.Text != "" {
			text = append(text, item.Text)
		}
	}
	return strings.Join(text, "\n")
}

func startAndCommit(t *testing.T, svc conversation.Service, client string) conversation.Turn {
	t.Helper()
	turn, err := svc.Start(context.Background(), client, requestWithText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginAttempt(context.Background(), turn, router.Decision{Provider: "openai", ModelID: "gpt-test", Tier: domain.T3}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(context.Background(), turn, pin("openai", "gpt-test", domain.T3), resultWithText("hi")); err != nil {
		t.Fatal(err)
	}
	return turn
}

func TestBeginAttemptPersistsFirstPinBeforeProviderIO(t *testing.T) {
	svc := testConversationService(t)
	turn, err := svc.Start(context.Background(), "client-a", requestWithText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginAttempt(context.Background(), turn, router.Decision{Provider: "openai", ModelID: "gpt-test", Tier: domain.T3}); err != nil {
		t.Fatal(err)
	}
	resumed, err := svc.Resume(context.Background(), "client-a", turn.ResponseID)
	if err != nil || resumed.Pin != pin("openai", "gpt-test", domain.T3) || resumed.Floor != domain.T3 {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
}

func TestCommitRejectsLowerTierPinAndRollsBackTranscript(t *testing.T) {
	svc := testConversationService(t)
	first, err := svc.Start(context.Background(), "client-a", requestWithText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginAttempt(context.Background(), first, router.Decision{Provider: "openai", ModelID: "gpt-t4", Tier: domain.T4}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(context.Background(), first, pin("openai", "gpt-t4", domain.T4), resultWithText("hi")); err != nil {
		t.Fatal(err)
	}
	next, err := svc.Start(context.Background(), "client-a", inference.Request{Model: "mindctl-auto", PreviousResponseID: first.ResponseID, Input: []inference.Item{{Type: "message", Role: "user", Text: "again"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginAttempt(context.Background(), next, router.Decision{Provider: "openai", ModelID: "gpt-t4", Tier: domain.T4}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(context.Background(), next, pin("openai", "gpt-t3", domain.T3), resultWithText("must not persist")); err == nil {
		t.Fatal("lower-tier replacement committed")
	}
	resumed, err := svc.Resume(context.Background(), "client-a", next.ResponseID)
	if err != nil || resumed.Pin != pin("openai", "gpt-t4", domain.T4) || transcriptText(resumed.Transcript) != "hello\nhi\nagain" {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
}

func TestCommitRequiresStartedAttempt(t *testing.T) {
	svc := testConversationService(t)
	turn, err := svc.Start(context.Background(), "client-a", requestWithText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(context.Background(), turn, pin("openai", "gpt-test", domain.T3), resultWithText("must not persist")); err == nil {
		t.Fatal("commit without attempt succeeded")
	}
	resumed, err := svc.Resume(context.Background(), "client-a", turn.ResponseID)
	if err != nil || transcriptText(resumed.Transcript) != "hello" {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
}

func TestStartDropsCallerProviderData(t *testing.T) {
	svc := testConversationService(t)
	turn, err := svc.Start(context.Background(), "client-a", inference.Request{Model: "mindctl-auto", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello", ProviderData: []byte(`{"untrusted":true}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"openai", "anthropic", "gemini"} {
		if got := turn.TranscriptFor(provider)[0].ProviderData; got != nil {
			t.Fatalf("provider %q received caller data: %s", provider, got)
		}
	}
}

func TestResumeRejectsUnknownAndForeignResponse(t *testing.T) {
	svc := testConversationService(t)
	if _, err := svc.Resume(context.Background(), "client-a", "resp_missing"); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	conv := startAndCommit(t, svc, "client-a")
	if _, err := svc.Resume(context.Background(), "client-b", conv.ResponseID); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestCommitAndResumePreservesCanonicalTranscriptAndPin(t *testing.T) {
	svc := testConversationService(t)
	started, err := svc.Start(context.Background(), "client-a", requestWithText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginAttempt(context.Background(), started, router.Decision{Provider: "openai", ModelID: "gpt-test", Tier: domain.T3}); err != nil {
		t.Fatal(err)
	}
	err = svc.CommitResult(context.Background(), started, pin("openai", "gpt-test", domain.T3), resultWithText("hi"))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := svc.Resume(context.Background(), "client-a", started.ResponseID)
	if err != nil || resumed.Pin.ModelID != "gpt-test" || transcriptText(resumed.Transcript) != "hello\nhi" {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
}
