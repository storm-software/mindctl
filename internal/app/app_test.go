package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/router"
)

func fixture(t *testing.T) (config.Config, map[string]string) {
	t.Helper()
	return config.Config{
			Listen: "127.0.0.1:0", ClientAuth: config.ClientAuthConfig{TokenEnv: "TEST_GATEWAY_TOKEN"},
			Jev:        config.JevConfig{BaseURL: "https://jev.example.com", Model: "jev", APIKeyEnv: "TEST_JEV_KEY"},
			SQLite:     config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "gateway.db")},
			Encryption: config.EncryptionConfig{ActiveKeyID: "active", Keys: map[string]string{"active": "TEST_ENCRYPTION_KEY", "old": "TEST_OLD_KEY"}},
			Routing:    config.RoutingConfig{MinTier: "T0", MaxTier: "T6"},
			Providers:  []config.ProviderConfig{{ID: "provider", BaseURL: "https://provider.example.com", APIKeyEnv: "TEST_PROVIDER_KEY"}},
			Models:     []config.ModelConfig{{ID: "first", Provider: "provider", Tier: "T4", Available: true, Capabilities: []string{"chat", "tools", "images", "json_schema"}, ContextWindow: 32000, InputPrice: 2, OutputPrice: 8, SuccessPrior: .9, TaskSuccessPriors: map[string]float64{"coding": .95}}},
		}, map[string]string{
			"TEST_GATEWAY_TOKEN": "private-gateway-token", "TEST_JEV_KEY": "private-jev-token", "TEST_PROVIDER_KEY": "private-provider-token",
			"TEST_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
			"TEST_OLD_KEY":        base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		}
}

func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := env[name]; return value, ok }
}

func status(a *App, path string) int {
	r := httptest.NewRecorder()
	a.Handler().ServeHTTP(r, httptest.NewRequest("GET", path, nil))
	return r.Code
}

func TestNewInitializesStorageKeyringAndStableHandler(t *testing.T) {
	cfg, env := fixture(t)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Handler() != a.Handler() || status(a, "/healthz") != 200 || status(a, "/readyz") != 200 {
		t.Fatal("operational handler is not stable and ready")
	}
	var migrations int
	if err := a.store.SQL().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil || migrations != 1 {
		t.Fatalf("migrations=%d err=%v", migrations, err)
	}
	old, err := contentcrypto.New("old", map[string][]byte{"old": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := old.Encrypt([]byte("rotation proof"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := a.keyring.Decrypt(envelope)
	if err != nil || string(plaintext) != "rotation proof" {
		t.Fatalf("old key decrypt: %v", err)
	}
	envelope, err = a.keyring.Encrypt([]byte("active proof"))
	if err != nil || envelope.KeyID != "active" {
		t.Fatalf("active encryption: %v", err)
	}
}

func TestSQLiteFailureChangesReadinessButNotHealth(t *testing.T) {
	cfg, env := fixture(t)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if _, err := a.store.SQL().Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatal(err)
	}
	if status(a, "/readyz") != 503 || status(a, "/healthz") != 200 {
		t.Fatal("SQLite failure did not affect readiness independently")
	}
}

func TestCloseIsConcurrentIdempotentAndReleasesSQLite(t *testing.T) {
	cfg, env := fixture(t)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := a.store.SQL().Ping(); err == nil {
		t.Fatal("database remains open")
	}
	if status(a, "/readyz") != 503 || status(a, "/healthz") != 200 {
		t.Fatal("closed app operational state is incorrect")
	}
}

func TestNewFailsWhenEncryptionKeyIsMissing(t *testing.T) {
	cfg, env := fixture(t)
	for name, value := range env {
		t.Setenv(name, value)
	}
	t.Setenv("TEST_ENCRYPTION_KEY", "")
	a, err := New(context.Background(), cfg)
	if a != nil || err == nil || !strings.Contains(err.Error(), "encryption") || !strings.Contains(err.Error(), "TEST_ENCRYPTION_KEY") {
		t.Fatalf("app=%v err=%v", a, err)
	}
}

func TestNewRejectsInvalidRuntimeConfigWithoutSecretValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config, map[string]string)
	}{
		{"missing client token", func(_ *config.Config, env map[string]string) { delete(env, "TEST_GATEWAY_TOKEN") }},
		{"missing Jev token", func(_ *config.Config, env map[string]string) { delete(env, "TEST_JEV_KEY") }},
		{"missing provider token", func(_ *config.Config, env map[string]string) { delete(env, "TEST_PROVIDER_KEY") }},
		{"malformed active key", func(_ *config.Config, env map[string]string) {
			env["TEST_ENCRYPTION_KEY"] = "not-base64-private-secret"
		}},
		{"malformed old key", func(_ *config.Config, env map[string]string) { env["TEST_OLD_KEY"] = "not-base64-private-secret" }},
		{"unknown active key", func(cfg *config.Config, _ map[string]string) { cfg.Encryption.ActiveKeyID = "absent" }},
		{"bad bounds", func(cfg *config.Config, _ map[string]string) { cfg.Routing.MinTier = "T9" }},
		{"no models", func(cfg *config.Config, _ map[string]string) { cfg.Models = nil }},
		{"all models disabled", func(cfg *config.Config, _ map[string]string) { cfg.Models[0].Available = false }},
		{"unusable provider", func(cfg *config.Config, _ map[string]string) { cfg.Providers[0].BaseURL = "not-a-url" }},
		{"invalid Jev endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Jev.BaseURL = "not-a-url" }},
		{"whitespace Jev model", func(cfg *config.Config, _ map[string]string) { cfg.Jev.Model = " \t\n" }},
		{"query Jev endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Jev.BaseURL = "https://jev.example.com?x=1" }},
		{"empty query Jev endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Jev.BaseURL = "https://jev.example.com?" }},
		{"fragment Jev endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Jev.BaseURL = "https://jev.example.com#section" }},
		{"empty fragment Jev endpoint", func(cfg *config.Config, _ map[string]string) { cfg.Jev.BaseURL = "https://jev.example.com#" }},
		{"invalid Jev retry count", func(cfg *config.Config, _ map[string]string) { cfg.Jev.MaxRetries = -1 }},
		{"SQLite cannot open", func(cfg *config.Config, _ map[string]string) {
			cfg.SQLite.Path = filepath.Join(t.TempDir(), "missing", "db")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, env := fixture(t)
			tc.mutate(&cfg, env)
			a, err := newWithLookup(context.Background(), cfg, lookup(env))
			if a != nil {
				_ = a.Close()
				t.Fatal("returned app on failure")
			}
			if err == nil {
				t.Fatal("invalid bootstrap succeeded")
			}
			for _, value := range env {
				if strings.Contains(err.Error(), value) {
					t.Fatal("error exposed a resolved secret")
				}
			}
		})
	}
}

func TestNewSnapshotsEachResolvedSecretOnce(t *testing.T) {
	cfg, env := fixture(t)
	reads := map[string]int{}
	a, err := newWithLookup(context.Background(), cfg, func(name string) (string, bool) {
		reads[name]++
		if reads[name] > 1 {
			return "changed-private-secret", true
		}
		value, ok := env[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if cfg.Jev.APIKeyEnv != "TEST_JEV_KEY" || cfg.Encryption.Keys["active"] != "TEST_ENCRYPTION_KEY" {
		t.Fatal("config mutated to resolved secrets")
	}
}

func TestBootstrapCatalogFeedsDeterministicPolicy(t *testing.T) {
	cfg, env := fixture(t)
	second := cfg.Models[0]
	second.ID = "second"
	cfg.Models = append(cfg.Models, second)
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	cfg.Models[0].TaskSuccessPriors["coding"] = 0
	cfg.Models[0].Capabilities[0] = "mutated"
	decision, err := a.policy.Decide(router.DecisionInput{
		Models: a.catalog, Floor: domain.T4, TaskType: domain.TaskCoding,
		Features: domain.RequestFeatures{NeedsText: true, NeedsImages: true, NeedsFunctions: true, NeedsJSONSchema: true, InputTokens: 1000, ContextTokens: 32000},
		MinTier:  &a.minTier, MaxTier: &a.maxTier, ProviderCredentials: a.providerCredentials, ProviderAvailability: a.providerAvailability,
	})
	if err != nil || decision.ModelID != "first" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if len(decision.Candidates) != 2 || decision.Candidates[0].SuccessProbability != .95 || decision.Candidates[0].DirectCost != .002 {
		t.Fatalf("candidate wiring = %+v", decision.Candidates)
	}
}

func TestClassifierUsesResolvedSecretAndCloseReleasesItsConnections(t *testing.T) {
	cfg, env := fixture(t)
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-jev-token" || r.URL.Path != "/v1/systemone" {
			t.Error("classifier was not wired to its endpoint and resolved credential")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	cfg.Jev.BaseURL = server.URL
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	_, err = a.classifier.Classify(context.Background(), classifier.Input{Prompt: "test"})
	if !errors.Is(err, classifier.ErrUnavailable) {
		t.Fatalf("classification error = %v", err)
	}
	if status(a, "/readyz") != 200 {
		t.Fatal("remote classification failure changed local readiness")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("App.Close left its classifier connection open")
	}
}
