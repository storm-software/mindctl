package forward

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/domain"
)

func TestNamedForwardUsesAccountAndSkipsAutomatic(t *testing.T) {
	var sawModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawModel = string(body)
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	pool, err := accounts.NewPool(accounts.Settings{}, "", []accounts.StaticAccount{{
		Account: accounts.Account{
			ID: "config:openai", Provider: "openai", Protocol: "openai", Kind: accounts.KindAPIKey,
			BaseURL: upstream.URL, Enabled: true,
		},
		Secret: accounts.Secret{APIKey: "secret-token"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &Forwarder{
		Selector: pool, HTTP: upstream.Client(),
		Models: []domain.Model{{ID: "gpt-test", Provider: "openai"}},
	}
	result, err := forwarder.Do(context.Background(), Call{
		Method: http.MethodPost, Model: "gpt-test", CallerProtocol: "openai", Path: "/v1/images/generations",
		Body: []byte(`{"model":"gpt-test"}`),
	})
	if err != nil || result.Status != http.StatusOK || !strings.Contains(sawModel, "gpt-test") {
		t.Fatalf("result=%+v saw=%s err=%v", result, sawModel, err)
	}
	if _, err := forwarder.Do(context.Background(), Call{Model: "mindctl-auto", Path: "/v1/images/generations"}); err == nil {
		t.Fatal("automatic model was forwarded")
	}
}
