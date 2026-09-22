package app

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/router"
	"gopkg.in/yaml.v3"
)

// Exercise strict YAML and bootstrap together: dropping any configured capacity,
// estimate, budget or signal rule must change a real decision or reject startup.
func configuredApp(t *testing.T, routing, models string) *App {
	t.Helper()
	cfg, env := fixture(t)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for key, data := range map[string]string{"routing": routing, "models": models} {
		var value any
		if err := yaml.Unmarshal([]byte(data), &value); err != nil {
			t.Fatal(err)
		}
		doc[key] = value
	}
	raw, err = yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

const routingBounds = "min_tier: T0\nmax_tier: T6\n"
const policyModels = `
- id: cheap
  provider: openai
  tier: T4
  available: true
  capabilities: [text]
  context_window: 1000000
  max_output_tokens: 4096
  input_price: 0.1
  output_price: 1
  cached_input_price_usd_per_million: 0.01
  per_request_price_usd: 0.00001
  latency_p95: 2s
  success_prior: 0.75
- id: reliable
  provider: openai
  tier: T5
  available: true
  capabilities: [text]
  context_window: 1000000
  max_output_tokens: 8192
  input_price: 0.4
  output_price: 1
  per_request_price_usd: 0.00002
  latency_p95: 1s
  success_prior: 0.99
`

func decide(a *App, features domain.RequestFeatures, judgment *domain.JevJudgment) (router.Decision, error) {
	return a.policy.Decide(router.DecisionInput{Models: a.catalog, Features: features, Judgment: judgment,
		Floor: domain.T0, MinTier: &a.minTier, MaxTier: &a.maxTier,
		ProviderCredentials: a.providerCredentials, ProviderAvailability: a.providerAvailability})
}

func TestConfiguredOutputCapacity(t *testing.T) {
	a := configuredApp(t, routingBounds, policyModels)
	for _, tc := range []struct {
		output int64
		want   string
	}{{128, "cheap"}, {4096, "cheap"}, {4097, "reliable"}, {8193, ""}} {
		d, err := decide(a, domain.RequestFeatures{NeedsText: true, MaxOutputTokens: tc.output}, nil)
		if (err != nil) != (tc.want == "") || d.ModelID != tc.want {
			t.Fatalf("output=%d model=%s err=%v", tc.output, d.ModelID, err)
		}
	}
}

func TestConfiguredExpectedCostAndLimits(t *testing.T) {
	features := domain.RequestFeatures{NeedsText: true, InputTokens: 1000}
	for _, tc := range []struct{ name, setting, want string }{
		{"direct only", "", "cheap"},
		{"failure cost", "failure_escalation_cost_usd: 0.002\n", "reliable"},
		{"latency penalty", "latency_penalty_usd_per_second: 0.001\n", "reliable"},
		{"success minimum", "min_success_probability: 0.9\n", "reliable"},
		{"latency maximum", "max_latency: 1s\n", "reliable"},
		{"direct maximum", "max_direct_cost_usd: 0.00001\n", ""},
		{"expected maximum", "failure_escalation_cost_usd: 0.002\nmax_expected_cost_usd: 0.0001\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := configuredApp(t, routingBounds+tc.setting, policyModels)
			d, err := decide(a, features, nil)
			if (err != nil) != (tc.want == "") || d.ModelID != tc.want {
				t.Fatalf("model=%s err=%v", d.ModelID, err)
			}
		})
	}
	a := configuredApp(t, routingBounds, policyModels)
	d, err := decide(a, domain.RequestFeatures{InputTokens: 1000, CachedInputTokens: 500, MaxOutputTokens: 100}, nil)
	if err != nil || math.Abs(d.Candidates[0].DirectCost-.000165) > 1e-12 {
		t.Fatalf("cached/fixed/output estimate incorrect: %+v err=%v", d.Candidates, err)
	}
}

func TestBootstrapFallsBackToInputPriceForLegacyCachedPriceOmission(t *testing.T) {
	legacy := configuredApp(t, routingBounds, `
- id: legacy
  provider: openai
  tier: T4
  available: true
  capabilities: [text]
  context_window: 1000000
  max_output_tokens: 4096
  input_price: 2
  output_price: 0
  success_prior: 1
`)
	d, err := decide(legacy, domain.RequestFeatures{InputTokens: 1_000_000, CachedInputTokens: 1_000_000}, nil)
	if err != nil || len(d.Candidates) != 1 || math.Abs(d.Candidates[0].DirectCost-2) > 1e-12 {
		t.Fatalf("legacy cached-price fallback lost: decision=%+v err=%v", d, err)
	}

	explicitZero := configuredApp(t, routingBounds, `
- id: zero-cache
  provider: openai
  tier: T4
  available: true
  capabilities: [text]
  context_window: 1000000
  max_output_tokens: 4096
  input_price: 2
  cached_input_price_usd_per_million: 0
  output_price: 0
  success_prior: 1
`)
	d, err = decide(explicitZero, domain.RequestFeatures{InputTokens: 1_000_000, CachedInputTokens: 1_000_000}, nil)
	if err != nil || len(d.Candidates) != 1 || d.Candidates[0].DirectCost != 0 {
		t.Fatalf("explicit cached-price zero was not preserved: decision=%+v err=%v", d, err)
	}
}

func TestConfiguredSignalsAndConfidence(t *testing.T) {
	for _, signal := range []struct {
		field     string
		judgment  domain.JevJudgment
		threshold string
	}{
		{"reasoning_floors", domain.JevJudgment{ReasoningScore: 4}, "4"},
		{"coding_floors", domain.JevJudgment{CodingScore: 4}, "4"},
		{"risk_floors", domain.JevJudgment{BlastRadius: 3}, "3"},
		{"underspecification_floors", domain.JevJudgment{Underspecified: .8}, "0.8"},
	} {
		t.Run(signal.field, func(t *testing.T) {
			a := configuredApp(t, routingBounds+signal.field+": [{threshold: "+signal.threshold+", floor: T5}]\n", policyModels)
			d, err := decide(a, domain.RequestFeatures{}, &signal.judgment)
			if err != nil || d.ModelID != "reliable" {
				t.Fatalf("signal floor lost: model=%s err=%v", d.ModelID, err)
			}
		})
	}
	for _, tc := range []struct{ setting, want string }{{"", "reliable"}, {"min_jev_confidence: 0.9\n", "cheap"}, {"min_jev_confidence: 0\n", "reliable"}} {
		a := configuredApp(t, routingBounds+tc.setting, policyModels)
		d, err := decide(a, domain.RequestFeatures{}, &domain.JevJudgment{MinimumTier: domain.T5, TierConfidence: .8})
		if err != nil || d.ModelID != tc.want {
			t.Fatalf("confidence ignored: model=%s err=%v", d.ModelID, err)
		}
	}
}

func TestBootstrapRejectsInvalidCatalogNumbers(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, field := range []string{"input", "output", "prior", "task prior"} {
			t.Run(field, func(t *testing.T) {
				cfg, env := fixture(t)
				switch field {
				case "input":
					cfg.Models[0].InputPrice = value
				case "output":
					cfg.Models[0].OutputPrice = value
				case "prior":
					cfg.Models[0].SuccessPrior = value
				case "task prior":
					cfg.Models[0].TaskSuccessPriors["reasoning"] = value
				}
				assertBootstrapInvalid(t, cfg, env)
			})
		}
	}
	for _, mutate := range []func(*config.ModelConfig){
		func(m *config.ModelConfig) { m.ContextWindow = -1 },
		func(m *config.ModelConfig) { m.SuccessPrior = 1.01 },
		func(m *config.ModelConfig) { m.TaskSuccessPriors["reasoning"] = 1.01 },
	} {
		cfg, env := fixture(t)
		mutate(&cfg.Models[0])
		assertBootstrapInvalid(t, cfg, env)
	}
}

func TestBootstrapRejectsEveryInvalidCatalogValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.ModelConfig)
	}{
		{name: "negative context window", mutate: func(m *config.ModelConfig) { m.ContextWindow = -1 }},
		{name: "negative output capacity", mutate: func(m *config.ModelConfig) { m.MaxOutputTokens = -1 }},
		{name: "negative latency", mutate: func(m *config.ModelConfig) { m.LatencyP95 = -time.Nanosecond }},
		{name: "success prior above one", mutate: func(m *config.ModelConfig) { m.SuccessPrior = 1.01 }},
		{name: "task success prior above one", mutate: func(m *config.ModelConfig) { m.TaskSuccessPriors["reasoning"] = 1.01 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, env := fixture(t)
			tc.mutate(&cfg.Models[0])
			assertBootstrapInvalid(t, cfg, env)
		})
	}

	for _, value := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, tc := range []struct {
			name   string
			mutate func(*config.ModelConfig, float64)
		}{
			{name: "cached input price", mutate: func(m *config.ModelConfig, v float64) { m.CachedInputPriceUSDPerMillion = v }},
			{name: "per request price", mutate: func(m *config.ModelConfig, v float64) { m.PerRequestPriceUSD = v }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg, env := fixture(t)
				tc.mutate(&cfg.Models[0], value)
				assertBootstrapInvalid(t, cfg, env)
			})
		}
	}
}

func assertBootstrapInvalid(t *testing.T, cfg config.Config, env map[string]string) {
	t.Helper()
	a, err := newWithLookup(context.Background(), cfg, lookup(env))
	if a != nil {
		_ = a.Close()
		t.Fatal("invalid numeric config became ready")
	}
	if err == nil {
		t.Fatal("invalid config accepted")
	}
	for _, value := range env {
		if strings.Contains(err.Error(), value) {
			t.Fatal("secret in error")
		}
	}
}
