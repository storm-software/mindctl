package config

import (
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestShippedExampleReachesSecretValidation(t *testing.T) {
	_, err := Load("../../config.example.yaml", func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "MINDCTL_GATEWAY_TOKEN") {
		t.Fatalf("shipped example did not reach runtime secret validation: %v", err)
	}
}

func TestHeadroomConfigDefaultsAndValidation(t *testing.T) {
	defaultConfig, err := Read(writeConfig(t, "debug: false\n"))
	if err != nil {
		t.Fatalf("Read default config: %v", err)
	}
	if defaultConfig.Headroom.Enabled || defaultConfig.Headroom.ModeValue() != "cache" {
		t.Fatalf("default Headroom config = %+v", defaultConfig.Headroom)
	}

	for _, mode := range []string{"cache", "token"} {
		t.Run(mode, func(t *testing.T) {
			cfg := validConfig()
			cfg.Headroom = HeadroomConfig{Enabled: true, Mode: mode}
			if err := cfg.Validate(testEnv); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	invalid := validConfig()
	invalid.Headroom.Mode = "invalid"
	if err := invalid.Validate(testEnv); err == nil || !strings.Contains(err.Error(), "headroom mode") {
		t.Fatalf("Validate() error = %v; want invalid Headroom mode", err)
	}
	if _, err := Read("../../config.example.yaml"); err != nil {
		t.Fatalf("shipped example is not readable: %v", err)
	}
}

func TestShippedExampleIncludesChatGPTProModelCatalog(t *testing.T) {
	cfg, err := Read("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Read shipped example: %v", err)
	}

	wantPrices := map[string]struct {
		input, cachedInput, output float64
		capabilities               []string
		available                  bool
	}{
		"gpt-6-astra":         {input: 10, cachedInput: 1, output: 50, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: true},
		"gpt-6-sol":           {input: 2, cachedInput: .2, output: 10, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: true},
		"gpt-6-luna":          {input: .1, cachedInput: .01, output: .5, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: true},
		"gpt-5.6-sol":         {input: 4, cachedInput: .4, output: 20, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: true},
		"gpt-5.6-terra":       {input: 2, cachedInput: .2, output: 12, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: true},
		"gpt-5.6-luna":        {input: .2, cachedInput: .02, output: 1.2, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: true},
		"gpt-5.3-codex":       {input: 3.5, cachedInput: .35, output: 28, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: true},
		"gpt-5.3-codex-spark": {input: 0, cachedInput: 0, output: 0, capabilities: []string{"chat", "tools", "images", "json_schema", "web_search"}, available: false},
	}
	wantModelCount := len(wantPrices)
	matchedModels := 0
	for _, model := range cfg.Models {
		want, ok := wantPrices[model.ID]
		if !ok {
			continue
		}
		matchedModels++
		delete(wantPrices, model.ID)
		if model.Provider != "openai" || model.Available != want.available {
			t.Errorf("model %s provider/available = %q/%t, want openai/%t", model.ID, model.Provider, model.Available, want.available)
		}
		if model.InputPrice != want.input || model.CachedInputPrice() != want.cachedInput || model.OutputPrice != want.output {
			t.Errorf("model %s prices = input %g cached %g output %g, want input %g cached %g output %g", model.ID, model.InputPrice, model.CachedInputPrice(), model.OutputPrice, want.input, want.cachedInput, want.output)
		}
		if !slices.Equal(model.Capabilities, want.capabilities) {
			t.Errorf("model %s capabilities = %v, want %v", model.ID, model.Capabilities, want.capabilities)
		}
	}
	if matchedModels != wantModelCount {
		t.Fatalf("shipped OpenAI model count = %d, want %d", matchedModels, wantModelCount)
	}
	for modelID := range wantPrices {
		t.Errorf("missing shipped model %q", modelID)
	}
	reviewers := 0
	for _, model := range cfg.Models {
		if model.ID == "codex-auto-review" {
			reviewers++
			if model.Provider != "openai" || !model.Available || !model.ExplicitOnly {
				t.Errorf("shipped reviewer = %#v", model)
			}
		}
	}
	if reviewers != 1 {
		t.Errorf("shipped reviewer count = %d, want 1", reviewers)
	}
}

func TestShippedExampleIncludesClaudeSubscriptionModelCatalog(t *testing.T) {
	cfg, err := Read("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Read shipped example: %v", err)
	}

	providers := make(map[string]ProviderConfig, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		providers[provider.ID] = provider
	}
	provider, ok := providers["anthropic"]
	if !ok || provider.BaseURL != "https://api.anthropic.com" || provider.AuthMode() != ProviderAuthClaudeOAuthPassthrough {
		t.Fatalf("Anthropic provider = %#v, want Claude OAuth provider", provider)
	}

	wantModels := map[string]struct {
		tier                       string
		context, outputLimit       int
		input, cachedInput, output float64
	}{
		"claude-fable-5-1": {tier: "T6", context: 1_000_000, outputLimit: 128_000, input: 10, cachedInput: .25, output: 50},
		"claude-opus-5-5":  {tier: "T5", context: 1_000_000, outputLimit: 128_000, input: 4, cachedInput: .2, output: 20},
		"claude-sonnet-5":  {tier: "T4", context: 1_000_000, outputLimit: 128_000, input: 2, cachedInput: .2, output: 10},
		"claude-haiku-4-5": {tier: "T2", context: 200_000, outputLimit: 64_000, input: 1, cachedInput: .1, output: 5},
	}
	for _, model := range cfg.Models {
		want, ok := wantModels[model.ID]
		if !ok {
			continue
		}
		delete(wantModels, model.ID)
		if model.Provider != "anthropic" || model.Tier != want.tier || !model.Available {
			t.Errorf("model %s provider/tier/available = %q/%q/%t", model.ID, model.Provider, model.Tier, model.Available)
		}
		if model.ContextWindow != want.context || model.MaxOutputTokens != want.outputLimit {
			t.Errorf("model %s context/output = %d/%d", model.ID, model.ContextWindow, model.MaxOutputTokens)
		}
		if model.InputPrice != want.input || model.CachedInputPrice() != want.cachedInput || model.OutputPrice != want.output {
			t.Errorf("model %s prices = input %g cached %g output %g", model.ID, model.InputPrice, model.CachedInputPrice(), model.OutputPrice)
		}
		if !slices.Equal(model.Capabilities, []string{"chat", "tools", "images", "json_schema"}) {
			t.Errorf("model %s capabilities = %v", model.ID, model.Capabilities)
		}
	}
	for modelID := range wantModels {
		t.Errorf("missing shipped Claude model %q", modelID)
	}
}

func TestShippedExampleIncludesMetaMuseSparkModelCatalog(t *testing.T) {
	cfg, err := Read("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Read shipped example: %v", err)
	}

	wantPrices := map[string]struct {
		input, cachedInput, output float64
	}{
		"muse-spark-1.3":             {input: 1.25, cachedInput: .15, output: 4.25},
		"muse-spark-1.3-contributor": {input: .1, cachedInput: .002, output: .2},
	}
	for _, model := range cfg.Models {
		want, ok := wantPrices[model.ID]
		if !ok {
			continue
		}
		delete(wantPrices, model.ID)
		if model.Provider != "meta" || !model.Available {
			t.Errorf("model %s provider/available = %q/%t, want meta/true", model.ID, model.Provider, model.Available)
		}
		if model.InputPrice != want.input || model.CachedInputPrice() != want.cachedInput || model.OutputPrice != want.output {
			t.Errorf("model %s prices = input %g cached %g output %g, want input %g cached %g output %g", model.ID, model.InputPrice, model.CachedInputPrice(), model.OutputPrice, want.input, want.cachedInput, want.output)
		}
	}
	for modelID := range wantPrices {
		t.Errorf("missing shipped Meta model %q", modelID)
	}
}

func TestShippedExampleIncludesEnabledDeepSeekV4ModelCatalog(t *testing.T) {
	cfg, err := Read("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Read shipped example: %v", err)
	}

	providers := make(map[string]ProviderConfig, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		providers[provider.ID] = provider
	}
	deepSeek, ok := providers["deepseek"]
	if !ok {
		t.Fatal("shipped example does not enable the DeepSeek provider")
	}
	if deepSeek.BaseURL != "https://api.deepseek.com" || deepSeek.AuthMode() != ProviderAuthAPIKey || deepSeek.APIKeyEnv != "DEEPSEEK_API_TOKEN" {
		t.Errorf("DeepSeek provider = %#v, want api-key provider at https://api.deepseek.com using DEEPSEEK_API_TOKEN", deepSeek)
	}

	wantModels := map[string]struct {
		tier                       string
		input, cachedInput, output float64
		capabilities               []string
	}{
		"deepseek-v4-flash": {tier: "T3", input: .14, cachedInput: .028, output: .28, capabilities: []string{"chat", "tools", "images", "json_schema"}},
		"deepseek-v4-pro":   {tier: "T5", input: 1.74, cachedInput: .145, output: 3.48, capabilities: []string{"chat", "tools", "json_schema"}},
	}
	for _, model := range cfg.Models {
		want, ok := wantModels[model.ID]
		if !ok {
			continue
		}
		delete(wantModels, model.ID)
		if model.Provider != "deepseek" || model.Tier != want.tier || !model.Available {
			t.Errorf("model %s provider/tier/available = %q/%q/%t, want deepseek/%s/true", model.ID, model.Provider, model.Tier, model.Available, want.tier)
		}
		if model.ContextWindow != 1_000_000 || model.MaxOutputTokens != 384_000 {
			t.Errorf("model %s context/output = %d/%d, want 1000000/384000", model.ID, model.ContextWindow, model.MaxOutputTokens)
		}
		if !slices.Equal(model.Capabilities, want.capabilities) {
			t.Errorf("model %s capabilities = %v, want %v", model.ID, model.Capabilities, want.capabilities)
		}
		if model.InputPrice != want.input || model.CachedInputPrice() != want.cachedInput || model.OutputPrice != want.output {
			t.Errorf("model %s prices = input %g cached %g output %g, want input %g cached %g output %g", model.ID, model.InputPrice, model.CachedInputPrice(), model.OutputPrice, want.input, want.cachedInput, want.output)
		}
	}
	for modelID := range wantModels {
		t.Errorf("missing shipped DeepSeek model %q", modelID)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, "listen: ':8080'\nunknown: true\n")
	_, err := Load(path, func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoadRejectsLegacyJevConfiguration(t *testing.T) {
	body, err := yaml.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(writeConfig(t, string(body)+"jev:\n  api_key_env: JEV_API_KEY\n"), testEnv)
	if err == nil || !strings.Contains(err.Error(), "field jev not found") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadRejectsSecondDocument(t *testing.T) {
	path := writeConfig(t, `
listen: ":8080"
client_auth:
  token_env: GATEWAY_TOKEN
classifier:
  endpoint: https://laya.example.test
  token_env: LAYA_CLASSIFIER_TOKEN
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

func TestReadDecodesCatalogWithoutResolvingSecrets(t *testing.T) {
	body, err := yaml.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, string(body))

	cfg, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(cfg.Models) != 1 || cfg.Models[0].ID != "gpt-test" {
		t.Fatalf("models = %#v", cfg.Models)
	}
	if cfg.ClientAuth.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("max body bytes = %d, want %d", cfg.ClientAuth.MaxBodyBytes, DefaultMaxBodyBytes)
	}
}

func TestReadAddsExplicitOnlyReviewerForChatGPTOAuth(t *testing.T) {
	cfg := validConfig()
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
	cfg.Providers[0].APIKeyEnv = ""
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, string(body))

	for _, read := range []struct {
		name string
		load func() (Config, error)
	}{
		{name: "offline", load: func() (Config, error) { return Read(path) }},
		{name: "gateway", load: func() (Config, error) { return Load(path, testEnv) }},
	} {
		t.Run(read.name, func(t *testing.T) {
			loaded, err := read.load()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Models) != 2 {
				t.Fatalf("models = %#v, want original and reviewer", loaded.Models)
			}
			reviewer := loaded.Models[1]
			if reviewer.ID != "codex-auto-review" || reviewer.Provider != "openai" || !reviewer.ExplicitOnly || !reviewer.Available {
				t.Fatalf("reviewer = %#v", reviewer)
			}
			if reviewer.Tier == "" || reviewer.ContextWindow <= 0 || reviewer.MaxOutputTokens <= 0 || reviewer.SuccessPrior <= 0 || !slices.Contains(reviewer.Capabilities, "chat") {
				t.Fatalf("reviewer missing routing attributes: %#v", reviewer)
			}
		})
	}
}

func TestReadDoesNotAddReviewerForAPIKeyProvider(t *testing.T) {
	cfg := validConfig()
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(writeConfig(t, string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Models) != 1 || loaded.Models[0].ID != cfg.Models[0].ID {
		t.Fatalf("API-key catalog changed: %#v", loaded.Models)
	}
}

func TestReadPreservesExplicitOnlyReviewerAndRejectsConflicts(t *testing.T) {
	for _, test := range []struct {
		name      string
		provider  string
		explicit  bool
		wantError bool
	}{
		{name: "explicit OAuth entry", provider: "openai", explicit: true},
		{name: "nonexplicit OAuth entry", provider: "openai", wantError: true},
		{name: "wrong provider entry", provider: "other", explicit: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
			cfg.Providers[0].APIKeyEnv = ""
			cfg.Providers = append(cfg.Providers, ProviderConfig{ID: "other", APIKeyEnv: "OPENAI_API_KEY"})
			cfg.Models = append(cfg.Models, ModelConfig{ID: "codex-auto-review", Provider: test.provider, Tier: "T3", ExplicitOnly: test.explicit})
			body, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := Read(writeConfig(t, string(body)))
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "codex-auto-review") {
					t.Fatalf("Read error = %v; want reviewer conflict", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Models) != len(cfg.Models) || loaded.Models[1].ID != "codex-auto-review" || loaded.Models[1].Tier != "T3" || !loaded.Models[1].ExplicitOnly {
				t.Fatalf("configured reviewer was replaced or duplicated: %#v", loaded.Models)
			}
		})
	}
}

func TestReadReviewerRespectsPersistedAllowList(t *testing.T) {
	cfg := validConfig()
	cfg.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
	cfg.Providers[0].APIKeyEnv = ""
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(writeConfig(t, string(body)))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(statePath, []byte("providers:\n  openai:\n    - gpt-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyProvidersFile(statePath, &loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Models) != 2 || loaded.Models[1].Available {
		t.Fatalf("persisted allow-list enabled reviewer: %#v", loaded.Models)
	}
}

func TestReadDecodesDebugSetting(t *testing.T) {
	cfg := validConfig()
	cfg.Debug = true
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := Read(writeConfig(t, string(body)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !loaded.Debug {
		t.Fatal("debug setting was not decoded")
	}
}

func TestModelConfigDecodesExplicitOnly(t *testing.T) {
	cfg := validConfig()
	cfg.Models[0].ExplicitOnly = true
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "explicit_only: true") {
		t.Fatal("explicit_only setting was not encoded")
	}
	loaded, err := Read(writeConfig(t, string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Models[0].ExplicitOnly {
		t.Fatal("explicit_only setting was not decoded")
	}
}

func TestValidateCatalogRejectsBrokenIdentitiesWithoutResolvingSecrets(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{{ID: "duplicate"}, {ID: "duplicate"}},
		Models: []ModelConfig{
			{ID: "same", Provider: "duplicate"},
			{ID: "same", Provider: "missing"},
		},
	}
	err := cfg.ValidateCatalog()
	if err == nil {
		t.Fatal("ValidateCatalog succeeded")
	}
	for _, want := range []string{"duplicate provider ID", "duplicate model ID", "references missing provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateCatalog error %q missing %q", err, want)
		}
	}
}

func TestLoadDefaultsSafeFallbackToT4(t *testing.T) {
	path := writeConfig(t, `
listen: ":8080"
client_auth:
  token_env: GATEWAY_TOKEN
classifier:
  endpoint: https://laya.example.test
  token_env: LAYA_CLASSIFIER_TOKEN
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
	if cfg.ClientAuth.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("max body bytes = %d, want %d", cfg.ClientAuth.MaxBodyBytes, DefaultMaxBodyBytes)
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

func TestValidateRejectsEmptyClassifierFragment(t *testing.T) {
	cfg := validConfig()
	cfg.Classifier.Endpoint = "https://laya.example.test#"
	if err := cfg.Validate(testEnv); err == nil || !strings.Contains(err.Error(), "invalid classifier endpoint") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateRejectsNegativeRetentionMaintenanceInterval(t *testing.T) {
	cfg := validConfig()
	cfg.SQLite.RetentionMaintenanceInterval = -time.Second
	if err := cfg.Validate(testEnv); err == nil {
		t.Fatal("negative retention maintenance interval was accepted")
	}
}

func TestValidateRejectsNegativeMaximumBodyBytes(t *testing.T) {
	cfg := validConfig()
	cfg.ClientAuth.MaxBodyBytes = -1
	if err := cfg.Validate(testEnv); err == nil || !strings.Contains(err.Error(), "maximum body bytes") {
		t.Fatalf("negative maximum body bytes was accepted: %v", err)
	}
}

func TestProviderAuthenticationDefaultsRemainCompatible(t *testing.T) {
	cfg := validConfig()
	if cfg.ClientAuth.HeaderName() != "Authorization" || cfg.Providers[0].AuthMode() != ProviderAuthAPIKey {
		t.Fatalf("header=%q auth=%q", cfg.ClientAuth.HeaderName(), cfg.Providers[0].AuthMode())
	}
	if err := cfg.Validate(testEnv); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAcceptsChatGPTOAuthWithoutAPIKey(t *testing.T) {
	cfg := validConfig()
	cfg.ClientAuth.Header = "X-Mindctl-Token"
	cfg.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
	cfg.Providers[0].BaseURL = "https://chatgpt.com/backend-api/codex"
	cfg.Providers[0].APIKeyEnv = ""
	if err := cfg.Validate(testEnv); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAcceptsClaudeOAuthWithoutAPIKey(t *testing.T) {
	cfg := validConfig()
	cfg.Providers[0] = ProviderConfig{
		ID:      "anthropic",
		BaseURL: "https://api.anthropic.com",
		Auth:    string(ProviderAuthClaudeOAuthPassthrough),
	}
	cfg.Models[0].Provider = "anthropic"
	if err := cfg.Validate(testEnv); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsInvalidAuthenticationCombinations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"unknown mode", func(c *Config) { c.Providers[0].Auth = "unknown" }, "unknown provider authentication mode"},
		{"oauth with api key", func(c *Config) {
			c.ClientAuth.Header = "X-Mindctl-Token"
			c.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
		}, "must not configure api_key_env"},
		{"oauth on anthropic", func(c *Config) {
			c.ClientAuth.Header = "X-Mindctl-Token"
			c.Providers[0] = ProviderConfig{ID: "anthropic", Auth: string(ProviderAuthChatGPTOAuthPassthrough)}
		}, "only supported for openai"},
		{"Claude OAuth on OpenAI", func(c *Config) {
			c.Providers[0].Auth = string(ProviderAuthClaudeOAuthPassthrough)
			c.Providers[0].APIKeyEnv = ""
		}, "only supported for anthropic"},
		{"Claude OAuth with API key", func(c *Config) {
			c.Providers[0] = ProviderConfig{
				ID:        "anthropic",
				Auth:      string(ProviderAuthClaudeOAuthPassthrough),
				APIKeyEnv: "ANTHROPIC_API_KEY",
			}
		}, "must not configure api_key_env"},
		{"Claude token header conflict", func(c *Config) {
			c.ClientAuth.Header = "x-mindctl-claude-token"
			c.Providers[0] = ProviderConfig{ID: "anthropic", Auth: string(ProviderAuthClaudeOAuthPassthrough)}
		}, "conflicts with Claude OAuth"},
		{"authorization conflict", func(c *Config) {
			c.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
			c.Providers[0].APIKeyEnv = ""
		}, "conflicts with ChatGPT OAuth"},
		{"account conflict", func(c *Config) {
			c.ClientAuth.Header = "chatgpt-account-id"
			c.Providers[0].Auth = string(ProviderAuthChatGPTOAuthPassthrough)
			c.Providers[0].APIKeyEnv = ""
		}, "conflicts with ChatGPT OAuth"},
		{"invalid header", func(c *Config) { c.ClientAuth.Header = "Bad Header" }, "invalid client authentication header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(testEnv); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
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
		{name: "classifier confidence below zero", mutate: func(r *RoutingConfig) { r.MinClassifierConfidence = -0.01 }},
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

func TestClaudeMessagesRequiresSeparateClientHeaderAndOAuthProvider(t *testing.T) {
	for _, test := range []struct {
		name      string
		enabled   bool
		header    string
		withOAuth bool
		wantError string
	}{
		{name: "disabled preserves default"},
		{name: "enabled with dedicated header and OAuth", enabled: true, header: "X-Mindctl-Token", withOAuth: true},
		{name: "enabled with Authorization", enabled: true, withOAuth: true, wantError: "Authorization"},
		{name: "enabled without Anthropic OAuth", enabled: true, header: "X-Mindctl-Token", wantError: "Claude OAuth"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ClaudeMessages.Enabled = test.enabled
			cfg.ClientAuth.Header = test.header
			if test.withOAuth {
				cfg.Providers = append(cfg.Providers, ProviderConfig{
					ID: "anthropic", BaseURL: "https://api.anthropic.com", Auth: string(ProviderAuthClaudeOAuthPassthrough),
				})
			}
			err := cfg.Validate(testEnv)
			if test.wantError == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("Validate() error = %v; want %q", err, test.wantError)
			}
		})
	}
	path := writeConfig(t, "claude_messages:\n  enabled: true\n")
	cfg, err := Read(path)
	if err != nil || !cfg.ClaudeMessages.Enabled {
		t.Fatalf("Read() = %+v, %v; want enabled", cfg.ClaudeMessages, err)
	}
	path = writeConfig(t, "claude_messages:\n  unknowable: true\n")
	if _, err := Read(path); err == nil {
		t.Fatal("Read() accepted unknown Claude Messages config")
	}
}

func validConfig() Config {
	return Config{
		Listen:     ":8080",
		ClientAuth: ClientAuthConfig{TokenEnv: "GATEWAY_TOKEN"},
		Classifier: ClassifierConfig{Endpoint: "https://laya.example.test", TokenEnv: "LAYA_CLASSIFIER_TOKEN"},
		SQLite:     SQLiteConfig{Path: "mindctl.db"},
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
		"LAYA_CLASSIFIER_TOKEN":  "laya-token",
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
