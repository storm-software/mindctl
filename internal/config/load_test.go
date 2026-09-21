package config

import (
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShippedExampleReachesSecretValidation(t *testing.T) {
	_, err := Load("../../config.example.yaml", func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "MINDCTL_GATEWAY_TOKEN") {
		t.Fatalf("shipped example did not reach runtime secret validation: %v", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, "listen: ':8080'\nunknown: true\n")
	_, err := Load(path, func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoadRejectsSecondDocument(t *testing.T) {
	path := writeConfig(t, `
listen: ":8080"
client_auth:
  token_env: GATEWAY_TOKEN
jev:
  api_key_env: JEV_API_KEY
sqlite:
  path: mindctl.db
encryption:
  active_key_id: active
  keys:
    active: ENCRYPTION_KEY
routing:
  min_tier: T0
  max_tier: T6
providers:
  - id: openai
    api_key_env: OPENAI_API_KEY
models:
  - id: gpt-test
    provider: openai
    tier: T4
---
unknown: true
`)

	_, err := Load(path, testEnv)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("expected second-document error, got %v", err)
	}
}

func TestLoadDefaultsSafeFallbackToT4(t *testing.T) {
	path := writeConfig(t, `
listen: ":8080"
client_auth:
  token_env: GATEWAY_TOKEN
jev:
  api_key_env: JEV_API_KEY
sqlite:
  path: mindctl.db
encryption:
  active_key_id: active
  keys:
    active: ENCRYPTION_KEY
routing:
  min_tier: T0
  max_tier: T6
providers:
  - id: openai
    api_key_env: OPENAI_API_KEY
models:
  - id: gpt-test
    provider: openai
    tier: T4
`)

	cfg, err := Load(path, testEnv)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Routing.SafeFallbackTier != "T4" {
		t.Fatalf("safe fallback = %q, want T4", cfg.Routing.SafeFallbackTier)
	}
}

func TestValidateRejectsMissingSecretAndDuplicateModel(t *testing.T) {
	cfg := validConfig()
	cfg.Models = append(cfg.Models, cfg.Models[0])
	err := cfg.Validate(func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "duplicate model") ||
		!strings.Contains(err.Error(), "missing environment variable") {
		t.Fatalf("expected combined validation error, got %v", err)
	}
}

func TestValidateRejectsInvalidTierBounds(t *testing.T) {
	cfg := validConfig()
	cfg.Routing.MinTier, cfg.Routing.MaxTier = "T5", "T3"
	if err := cfg.Validate(testEnv); err == nil || !strings.Contains(err.Error(), "min_tier") {
		t.Fatalf("expected min tier above max tier to fail, got %v", err)
	}
}

func TestValidateAggregatesInvalidCatalogAndKeyErrors(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].Tier = "T9"
	cfg.Models[0].Provider = "missing"
	cfg.Models[0].InputPrice = -1
	cfg.Models[0].OutputPrice = -1
	cfg.Encryption.Keys["active"] = "INVALID_ENCRYPTION_KEY"

	err := cfg.Validate(testEnv)
	for _, want := range []string{
		"unknown tier", "missing provider", "negative input price", "negative output price", "base64-encoded 32-byte",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q in combined validation error, got %v", want, err)
		}
	}
}

func TestValidateAcceptsZeroRetentionAsUnlimited(t *testing.T) {
	cfg := validConfig()
	if cfg.SQLite.Retention != 0 {
		t.Fatalf("test setup retention = %v, want zero", cfg.SQLite.Retention)
	}
	if err := cfg.Validate(testEnv); err != nil {
		t.Fatalf("expected valid unlimited-retention config, got %v", err)
	}
}

func TestValidateRejectsNegativeRetentionMaintenanceInterval(t *testing.T) {
	cfg := validConfig()
	cfg.SQLite.RetentionMaintenanceInterval = -time.Second
	if err := cfg.Validate(testEnv); err == nil {
		t.Fatal("negative retention maintenance interval was accepted")
	}
}

func TestValidateRejectsInvalidConfiguredPolicyValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*RoutingConfig)
	}{
		{name: "negative failure escalation cost", mutate: func(r *RoutingConfig) { r.FailureEscalationCostUSD = -0.01 }},
		{name: "nonfinite latency penalty", mutate: func(r *RoutingConfig) { r.LatencyPenaltyUSDPerSecond = math.Inf(1) }},
		{name: "negative direct cost limit", mutate: func(r *RoutingConfig) { r.MaxDirectCostUSD = -0.01 }},
		{name: "nonfinite expected cost limit", mutate: func(r *RoutingConfig) { r.MaxExpectedCostUSD = math.NaN() }},
		{name: "success probability above one", mutate: func(r *RoutingConfig) { r.MinSuccessProbability = 1.01 }},
		{name: "Jev confidence below zero", mutate: func(r *RoutingConfig) { r.MinJevConfidence = -0.01 }},
		{name: "negative latency limit", mutate: func(r *RoutingConfig) { r.MaxLatency = -time.Second }},
		{name: "invalid reasoning threshold", mutate: func(r *RoutingConfig) { r.ReasoningFloors = []SignalFloorConfig{{Threshold: math.Inf(1), Floor: "T4"}} }},
		{name: "invalid coding floor", mutate: func(r *RoutingConfig) { r.CodingFloors = []SignalFloorConfig{{Threshold: 1, Floor: "T9"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg.Routing)
			if err := cfg.Validate(testEnv); err == nil {
				t.Fatal("invalid policy configuration was accepted")
			}
		})
	}
}

func validConfig() Config {
	return Config{
		Listen:     ":8080",
		ClientAuth: ClientAuthConfig{TokenEnv: "GATEWAY_TOKEN"},
		Jev: JevConfig{
			BaseURL:   "https://jev.example.test",
			Model:     "jev-system-one",
			APIKeyEnv: "JEV_API_KEY",
		},
		SQLite: SQLiteConfig{Path: "mindctl.db"},
		Encryption: EncryptionConfig{
			ActiveKeyID: "active",
			Keys:        map[string]string{"active": "ENCRYPTION_KEY"},
		},
		Routing: RoutingConfig{MinTier: "T0", MaxTier: "T6", SafeFallbackTier: "T4"},
		Providers: []ProviderConfig{{
			ID: "openai", BaseURL: "https://api.openai.com", APIKeyEnv: "OPENAI_API_KEY",
		}},
		Models: []ModelConfig{{
			ID: "gpt-test", Provider: "openai", Tier: "T4", InputPrice: 0.1, OutputPrice: 0.2,
		}},
	}
}

func testEnv(name string) (string, bool) {
	values := map[string]string{
		"GATEWAY_TOKEN":          "gateway-token",
		"JEV_API_KEY":            "jev-key",
		"OPENAI_API_KEY":         "provider-key",
		"ENCRYPTION_KEY":         base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"INVALID_ENCRYPTION_KEY": "not-base64",
	}
	value, ok := values[name]
	return value, ok
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
