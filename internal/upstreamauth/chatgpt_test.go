package upstreamauth

import (
	"context"
	"testing"
)

func TestChatGPTCredentialIsRequestScoped(t *testing.T) {
	base := context.Background()
	want := ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"}
	derived := WithChatGPT(base, want)
	if _, ok := ChatGPT(base); ok {
		t.Fatal("credential leaked into parent context")
	}
	if got, ok := ChatGPT(derived); !ok || got != want {
		t.Fatalf("credential=%+v present=%v", got, ok)
	}
}

func TestChatGPTRejectsIncompleteCredential(t *testing.T) {
	for _, credential := range []ChatGPTCredential{
		{AccessToken: "token"},
		{AccountID: "account"},
		{AccessToken: " ", AccountID: "account"},
		{AccessToken: "oauth token", AccountID: "account"},
		{AccessToken: "token", AccountID: " "},
	} {
		if _, ok := ChatGPT(WithChatGPT(context.Background(), credential)); ok {
			t.Fatalf("accepted %+v", credential)
		}
	}
}
