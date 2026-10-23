package accounts

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func account(id, provider string, weight, priority int) Account {
	return Account{ID: id, Provider: provider, Kind: KindAPIKey, Weight: weight, Priority: priority, Enabled: true}
}

func TestRoundRobinSkipsCoolingAccount(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	conductor := NewConductor(Settings{}, []Account{
		account("b", "openai", 1, 0),
		account("a", "openai", 1, 0),
	}, func() time.Time { return now })
	first, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai"})
	if err != nil || first.ID != "a" {
		t.Fatalf("first=%s err=%v", first.ID, err)
	}
	conductor.ReportFailure("session", first.ID, FailureQuota)
	second, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai", Tried: []string{first.ID}})
	if err != nil || second.ID != "b" {
		t.Fatalf("second=%s err=%v", second.ID, err)
	}
}

func TestWeightedRoundRobinUsesWeight(t *testing.T) {
	conductor := NewConductor(Settings{Strategy: StrategyWeighted}, []Account{
		account("light", "openai", 1, 0),
		account("heavy", "openai", 3, 0),
	}, nil)
	counts := map[string]int{}
	for range 4 {
		selected, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai"})
		if err != nil {
			t.Fatal(err)
		}
		counts[selected.ID]++
		conductor.ResetCooldown(selected.ID)
	}
	if counts["heavy"] != 3 || counts["light"] != 1 {
		t.Fatalf("counts=%v", counts)
	}
}

func TestFillFirstStaysUntilCooldown(t *testing.T) {
	conductor := NewConductor(Settings{Strategy: StrategyFillFirst}, []Account{
		account("later", "openai", 1, 2),
		account("first", "openai", 1, 1),
	}, nil)
	selected, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai"})
	if err != nil || selected.ID != "first" {
		t.Fatalf("selected=%s err=%v", selected.ID, err)
	}
	again, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai"})
	if err != nil || again.ID != "first" {
		t.Fatalf("again=%s err=%v", again.ID, err)
	}
	conductor.ReportFailure("", "first", FailureTransient)
	fallback, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai"})
	if err != nil || fallback.ID != "later" {
		t.Fatalf("fallback=%s err=%v", fallback.ID, err)
	}
}

func TestSessionAffinityRebindsOnlyForQuotaMode(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	conductor := NewConductor(Settings{SessionAffinity: true, RebindOn: RebindQuota, SubagentsConfigured: true, Subagents: false}, []Account{
		account("a", "openai", 1, 0),
		account("b", "openai", 1, 0),
	}, func() time.Time { return now })
	first, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai", SessionKey: "sess"})
	if err != nil {
		t.Fatal(err)
	}
	conductor.ReportFailure("sess", first.ID, FailureTransient)
	now = now.Add(DefaultTransientCooldown + time.Second)
	stuck, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai", SessionKey: "sess"})
	if err != nil || stuck.ID != first.ID {
		t.Fatalf("transient rebind stuck=%s want %s err=%v", stuck.ID, first.ID, err)
	}
	conductor.ReportFailure("sess", first.ID, FailureQuota)
	moved, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai", SessionKey: "sess", Tried: []string{first.ID}})
	if err != nil || moved.ID == first.ID {
		t.Fatalf("quota rebind moved=%s err=%v", moved.ID, err)
	}
}

func TestSubagentInheritsParentAccount(t *testing.T) {
	conductor := NewConductor(Settings{SessionAffinity: true}, []Account{
		account("parent-account", "openai", 1, 0),
		account("other", "openai", 1, 1),
	}, nil)
	if _, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai", SessionKey: "parent"}); err != nil {
		t.Fatal(err)
	}
	child, err := conductor.Select(SelectRequest{
		ModelID: "gpt", Provider: "openai", SessionKey: "child", ParentSessionKey: "parent",
	})
	if err != nil || child.ID != "parent-account" {
		t.Fatalf("child=%s err=%v", child.ID, err)
	}
}

func TestPrefixAndRetryRoundFilterAccounts(t *testing.T) {
	limited := account("limited", "openai", 1, 0)
	limited.RequestRetry = 0
	limited.RequestRetryConfigured = true
	limited.Prefix = "solo"
	prefixed := account("prefixed", "openai", 1, 0)
	prefixed.Prefix = "team"
	conductor := NewConductor(Settings{ForceModelPrefix: true}, []Account{limited, prefixed}, nil)
	if _, err := conductor.Select(SelectRequest{ModelID: "gpt", Provider: "openai", Round: 1}); !errors.Is(err, ErrNone) {
		t.Fatalf("round 1 err=%v", err)
	}
	selected, err := conductor.Select(SelectRequest{ModelID: "team/gpt", Provider: "openai"})
	if err != nil || selected.ID != "prefixed" {
		t.Fatalf("prefixed=%s err=%v", selected.ID, err)
	}
}

func TestPoolLoadsAuthFileWithoutExposingSecretToSelector(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteAuthFile(dir, Account{
		ID: "file-user", Provider: "openai", Protocol: "codex", Kind: KindOAuth, Enabled: true, Weight: 2,
	}, Secret{AccessToken: "secret-token", AccountID: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(Settings{}, dir, []StaticAccount{{
		Account: account("config:openai", "openai", 1, 0),
		Secret:  Secret{APIKey: "config-key"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := pool.Select(nil, SelectRequest{ModelID: "gpt", Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Account.ID != "config:openai" || selection.Secret.APIKey != "config-key" {
		t.Fatalf("selection=%+v secret=%+v", selection.Account, selection.Secret)
	}
	for _, listed := range pool.Accounts() {
		if listed.ID == "file-user" && filepath.Base(listed.ID) == "file-user" {
			return
		}
	}
	t.Fatal("auth file account was not loaded")
}

func TestResolveAlias(t *testing.T) {
	modelID, provider, ok := Resolve([]Alias{{Name: "gpt-upstream", Alias: "gpt-visible", Provider: "openai"}}, "gpt-visible")
	if !ok || modelID != "gpt-upstream" || provider != "openai" {
		t.Fatalf("model=%s provider=%s ok=%v", modelID, provider, ok)
	}
	if _, _, ok := Resolve(nil, "gpt-visible"); ok {
		t.Fatal("missing alias reported as resolved")
	}
}
