package upstreamauth

import (
	"context"
	"os"
	"path/filepath"
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

func TestReadCodexAuthFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"tokens":{"access_token":"oauth.jwt","account_id":"account-1","refresh_token":"refresh"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCodexAuthFile(path)
	if err != nil || got != (ChatGPTCredential{AccessToken: "oauth.jwt", AccountID: "account-1"}) {
		t.Fatalf("credential=%+v err=%v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"OPENAI_API_KEY":"sk-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCodexAuthFile(path); err == nil {
		t.Fatal("accepted auth file without a ChatGPT login")
	}
	if _, err := ReadCodexAuthFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("accepted missing auth file")
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
