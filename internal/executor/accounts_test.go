package executor

import (
	"context"
	"testing"

	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/provider"
)

func TestNamedModelFailsOverAccountsAndAutomaticDoesNot(t *testing.T) {
	deps := fakeDeps()
	pool, err := accounts.NewPool(accounts.Settings{}, "", []accounts.StaticAccount{
		{Account: accounts.Account{ID: "config:a", Provider: "openai", Kind: accounts.KindAPIKey}},
		{Account: accounts.Account{ID: "config:b", Provider: "openai", Kind: accounts.KindAPIKey}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps.Executor.SetAccountRouting(pool, []accounts.Alias{{Name: "gpt-test", Alias: "fast", Provider: "openai"}})
	deps.OpenAI.Failures = 1

	got, err := deps.Executor.Execute(context.Background(), deps.input(concreteModelInput("fast")))
	if err != nil || got.Decision.ModelID != "gpt-test" || deps.Classifier.Calls != 0 || deps.OpenAI.Calls != 2 {
		t.Fatalf("named got=%+v calls=%d classifier=%d err=%v", got.Decision, deps.OpenAI.Calls, deps.Classifier.Calls, err)
	}
	if len(deps.Conversations.AccountIDs) != 2 || deps.Conversations.AccountIDs[0] == deps.Conversations.AccountIDs[1] {
		t.Fatalf("account attempts=%v", deps.Conversations.AccountIDs)
	}

	automatic := fakeDeps()
	automatic.Executor.SetAccountRouting(pool, nil)
	automatic.OpenAI.Err = &provider.Error{Kind: provider.ErrorRateLimit, Status: 429}
	if _, err := automatic.Executor.Execute(context.Background(), automatic.input(newAutomaticInput())); err == nil || automatic.OpenAI.Calls != 1 || automatic.Classifier.Calls != 1 {
		t.Fatalf("automatic calls=%d classifier=%d err=%v", automatic.OpenAI.Calls, automatic.Classifier.Calls, err)
	}
	if len(automatic.Conversations.AccountIDs) != 0 {
		t.Fatalf("automatic stored account ids %v", automatic.Conversations.AccountIDs)
	}
}

func TestNamedModelUsesAccountProtocolAdapter(t *testing.T) {
	deps := fakeDeps()
	special := &fakeProvider{Result: deps.OpenAI.Result}
	pool, err := accounts.NewPool(accounts.Settings{}, "", []accounts.StaticAccount{{
		Account: accounts.Account{ID: "config:a", Provider: "openai", Protocol: "anthropic", Kind: accounts.KindAPIKey},
	}})
	if err != nil {
		t.Fatal(err)
	}
	deps.Executor.SetAccountRouting(pool, nil)
	deps.Executor.SetProtocolClients(func(protocol string) (provider.Provider, bool) {
		if protocol == "anthropic" {
			return special, true
		}
		return nil, false
	})
	if _, err := deps.Executor.Execute(context.Background(), deps.input(concreteModelInput("gpt-test"))); err != nil || special.Calls != 1 || deps.OpenAI.Calls != 0 {
		t.Fatalf("special=%d openai=%d err=%v", special.Calls, deps.OpenAI.Calls, err)
	}
}
