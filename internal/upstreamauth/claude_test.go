package upstreamauth

import (
	"context"
	"testing"
)

func TestClaudeCredentialIsRequestScoped(t *testing.T) {
	base := context.Background()
	want := ClaudeCredential{AccessToken: "oauth-token"}
	derived := WithClaude(base, want)
	if _, ok := Claude(base); ok {
		t.Fatal("credential leaked into parent context")
	}
	if got, ok := Claude(derived); !ok || got != want {
		t.Fatalf("credential=%+v present=%v", got, ok)
	}
}

func TestClaudeRejectsMalformedCredential(t *testing.T) {
	for _, credential := range []ClaudeCredential{
		{},
		{AccessToken: " "},
		{AccessToken: "oauth token"},
		{AccessToken: "oauth\ntoken"},
	} {
		if _, ok := Claude(WithClaude(context.Background(), credential)); ok {
			t.Fatalf("accepted %+v", credential)
		}
	}
}
