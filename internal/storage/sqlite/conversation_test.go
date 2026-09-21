package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
)

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
