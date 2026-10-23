package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodeLoginWritesAuthFile(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-secret", "refresh_token": "refresh-secret", "account_id": "acct"})
	}))
	defer tokenServer.Close()
	registry := NewRegistry()
	callback := httptest.NewServer(registry.Handler())
	defer callback.Close()
	provider := Providers()["codex"]
	provider.TokenURL = tokenServer.URL
	provider.RedirectPath = ""
	dir := t.TempDir()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), "codex", Options{
			NoBrowser: true, PublicBase: callback.URL, Registry: registry, Provider: &provider,
			AuthDir: dir, Stdout: &output, HTTP: tokenServer.Client(),
		})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	var authURL string
	for time.Now().Before(deadline) && !strings.Contains(output.String(), "state=") {
		time.Sleep(10 * time.Millisecond)
	}
	authURL = output.String()
	parsed, err := url.Parse(strings.TrimSpace(strings.TrimPrefix(authURL, "open ")))
	if err != nil || parsed.Query().Get("state") == "" {
		t.Fatalf("output=%q err=%v", authURL, err)
	}
	response, err := http.Get(callback.URL + "?code=abc&state=" + parsed.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status=%d", response.StatusCode)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil || !strings.Contains(string(body), "access-secret") {
		t.Fatalf("body=%s err=%v", body, err)
	}
}

func TestCallbackWithoutLoginIsNotFound(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=missing", nil)
	response := httptest.NewRecorder()
	NewRegistry().Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestImportVertexWritesServiceAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.json")
	body := []byte(`{"type":"service_account","client_email":"vertex@example.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n","token_uri":"https://oauth2.googleapis.com/token","project_id":"demo"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := ImportVertex(dir, path, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(saved)
	if err != nil || !strings.Contains(string(stored), "vertex@example.iam.gserviceaccount.com") || strings.Contains(saved, "private_key") {
		t.Fatalf("saved=%s body=%s err=%v", saved, stored, err)
	}
}
