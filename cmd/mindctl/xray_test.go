package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
)

func TestXrayCommandBreaksDownConversationRequests(t *testing.T) {
	path := mainConfig(t)
	cfg, err := config.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := contentcrypto.New("active", map[string][]byte{"active": bytes.Repeat([]byte{4}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.SQLite.Path, Keyring: keyring})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := conversation.New(db)

	tools := []inference.Tool{
		{Name: "Read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "mcp__github__search", Description: strings.Repeat("Search GitHub. ", 50)},
	}
	// Two stateless requests in one routing session, and one outside it.
	turn := func(sessionKey string, input []inference.Item) conversation.Turn {
		t.Helper()
		started, err := svc.Start(context.Background(), "client", inference.Request{
			Instructions: "You are a coding agent.", Tools: tools, Input: input, SessionKey: sessionKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.BeginAttempt(context.Background(), started, router.Decision{Provider: "openai", ModelID: "model", Tier: domain.T4}); err != nil {
			t.Fatal(err)
		}
		if err := svc.CommitResult(context.Background(), started, router.Pin{Provider: "openai", ModelID: "model", Floor: domain.T4}, inference.Result{
			Status: "completed",
			Output: []inference.Item{{Type: "message", Role: "assistant", Text: "done"}},
			Usage:  inference.Usage{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 50, Known: true},
		}); err != nil {
			t.Fatal(err)
		}
		return started
	}
	first := turn("session", []inference.Item{{Type: "message", Role: "user", Text: "Read main.go"}})
	second := turn("session", []inference.Item{
		{Type: "message", Role: "user", Text: "Read main.go"},
		{Type: "function_call", CallID: "call-1", Name: "Read", Arguments: json.RawMessage(`{"path":"main.go"}`)},
		{Type: "function_call_output", CallID: "call-1", Output: json.RawMessage(`"package main"`)},
	})
	turn("other", []inference.Item{{Type: "message", Role: "user", Text: "unrelated"}})

	var contexts int
	if err := db.SQL().QueryRow("SELECT count(*) FROM request_contexts").Scan(&contexts); err != nil {
		t.Fatal(err)
	}
	if contexts != 1 {
		t.Errorf("request_contexts has %d rows, want the repeated context stored once", contexts)
	}

	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "xray", "--conversation", first.ConversationID}, &stdout, io.Discard); err != nil {
		t.Fatalf("xray: %v", err)
	}
	rows := historyTableRows(stdout.String())
	row := func(first string, second ...string) []string {
		for _, row := range rows {
			if row[0] == first && (len(second) == 0 || row[1] == second[0]) {
				return row
			}
		}
		t.Fatalf("no %q row in:\n%s", first, stdout.String())
		return nil
	}
	if got := row("Requests")[1]; got != "2" {
		t.Errorf("requests = %q, want the two session requests", got)
	}
	if got := row("Input tokens")[1]; got != "2,000 (800 cache reads, 0 cache writes, 1,200 fresh)" {
		t.Errorf("input tokens = %q", got)
	}
	for _, category := range []string{"tool schemas", "instructions"} {
		row("static", category)
	}
	row("messages", "tool results")
	if got := row("mcp:github"); got[len(got)-1] != "unused" {
		t.Errorf("mcp:github row = %q, want unused", got)
	}
	if got := row("Read"); got[len(got)-1] != "" {
		t.Errorf("Read row = %q, want used", got)
	}

	stdout.Reset()
	if err := run(context.Background(), []string{"--config", path, "xray", second.ResponseID}, &stdout, io.Discard); err != nil {
		t.Fatalf("xray response: %v", err)
	}
	rows = historyTableRows(stdout.String())
	if got := row("Conversation"); !slices.Equal(got, []string{"Conversation", second.ConversationID}) {
		t.Errorf("conversation row = %q", got)
	}
	if got := row("Requests")[1]; got != "1" {
		t.Errorf("single response requests = %q", got)
	}

	if err := run(context.Background(), []string{"--config", path, "xray", "resp_missing"}, io.Discard, io.Discard); err == nil {
		t.Error("xray accepted an unknown response ID")
	}
}
