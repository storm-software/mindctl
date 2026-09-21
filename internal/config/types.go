package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"time"
)

// Config is the complete gateway configuration loaded from YAML.
type Config struct {
	Listen     string           `yaml:"listen"`
	ClientAuth ClientAuthConfig `yaml:"client_auth"`
	Jev        JevConfig        `yaml:"jev"`
	SQLite     SQLiteConfig     `yaml:"sqlite"`
	Encryption EncryptionConfig `yaml:"encryption"`
	Routing    RoutingConfig    `yaml:"routing"`
	Providers  []ProviderConfig `yaml:"providers"`
	Models     []ModelConfig    `yaml:"models"`
}

// ClientAuthConfig configures gateway authentication using a referenced secret.
type ClientAuthConfig struct {
	TokenEnv string `yaml:"token_env"`
}

// JevConfig configures the Jev classifier using a referenced secret.
type JevConfig struct {
	BaseURL    string        `yaml:"base_url"`
	Model      string        `yaml:"model"`
	APIKeyEnv  string        `yaml:"api_key_env"`
	Timeout    time.Duration `yaml:"timeout"`
	MaxRetries int           `yaml:"max_retries"`
}

// SQLiteConfig controls local durable storage. Zero retention means unlimited
// and starts no retention maintenance. For finite retention, a zero maintenance
// interval uses the application's bounded default; nonzero intervals are Go
// duration strings such as "15m".
type SQLiteConfig struct {
	Path                         string        `yaml:"path"`
	Retention                    time.Duration `yaml:"retention"`
	RetentionMaintenanceInterval time.Duration `yaml:"retention_maintenance_interval"`
}

// EncryptionConfig maps key IDs to environment-variable names.
type EncryptionConfig struct {
	ActiveKeyID string            `yaml:"active_key_id"`
	Keys        map[string]string `yaml:"keys"`
}

// RoutingConfig supplies the immutable deterministic routing policy. Dollar
// values are USD, probability values are fractions in [0, 1], and durations
// use Go duration strings such as "500ms" or "2s". Zero cost/limit values
// disable the corresponding budget or penalty; zero MinJevConfidence retains
// the policy's safe default.
type RoutingConfig struct {
	MinTier                    string              `yaml:"min_tier"`
	MaxTier                    string              `yaml:"max_tier"`
	SafeFallbackTier           string              `yaml:"safe_fallback_tier"`
	FailureEscalationCostUSD   float64             `yaml:"failure_escalation_cost_usd"`
	LatencyPenaltyUSDPerSecond float64             `yaml:"latency_penalty_usd_per_second"`
	MinSuccessProbability      float64             `yaml:"min_success_probability"`
	MaxDirectCostUSD           float64             `yaml:"max_direct_cost_usd"`
	MaxExpectedCostUSD         float64             `yaml:"max_expected_cost_usd"`
	MaxLatency                 time.Duration       `yaml:"max_latency"`
	MinJevConfidence           float64             `yaml:"min_jev_confidence"`
	ReasoningFloors            []SignalFloorConfig `yaml:"reasoning_floors"`
	CodingFloors               []SignalFloorConfig `yaml:"coding_floors"`
	RiskFloors                 []SignalFloorConfig `yaml:"risk_floors"`
	UnderspecificationFloors   []SignalFloorConfig `yaml:"underspecification_floors"`
}

// SignalFloorConfig raises, but never lowers, the deterministic policy floor
// when a configured Jev signal reaches Threshold.
type SignalFloorConfig struct {
	Threshold float64 `yaml:"threshold"`
	Floor     string  `yaml:"floor"`
}

// ProviderConfig configures one provider endpoint and secret reference.
type ProviderConfig struct {
	ID        string `yaml:"id"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// ModelConfig describes one configured provider model. InputPrice and
// OutputPrice are USD per million tokens; CachedInputPriceUSDPerMillion and
// PerRequestPriceUSD make their units explicit. Zero MaxOutputTokens advertises
// no positive output capacity, and zero LatencyP95 means no latency prior.
type ModelConfig struct {
	ID                            string             `yaml:"id"`
	Provider                      string             `yaml:"provider"`
	Tier                          string             `yaml:"tier"`
	Capabilities                  []string           `yaml:"capabilities"`
	ContextWindow                 int                `yaml:"context_window"`
	MaxOutputTokens               int                `yaml:"max_output_tokens"`
	Available                     bool               `yaml:"available"`
	InputPrice                    float64            `yaml:"input_price"`
	CachedInputPriceUSDPerMillion float64            `yaml:"cached_input_price_usd_per_million"`
	OutputPrice                   float64            `yaml:"output_price"`
	PerRequestPriceUSD            float64            `yaml:"per_request_price_usd"`
	LatencyP95                    time.Duration      `yaml:"latency_p95"`
	SuccessPrior                  float64            `yaml:"success_prior"`
	TaskSuccessPriors             map[string]float64 `yaml:"task_success_priors"`
}

// Validate checks configuration and referenced environment variables without
// placing secret values in YAML. All independent faults are returned together.
func (cfg Config) Validate(getenv func(string) (string, bool)) error {
	var errs []error

	requireEnv := func(label, name string) {
		value, ok := getenv(name)
		if name == "" || !ok || value == "" {
			errs = append(errs, fmt.Errorf("missing environment variable for %s: %s", label, name))
		}
	}

	requireEnv("client authentication", cfg.ClientAuth.TokenEnv)
	requireEnv("Jev", cfg.Jev.APIKeyEnv)

	providerIDs := make(map[string]struct{}, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		providerIDs[provider.ID] = struct{}{}
		requireEnv("provider "+provider.ID, provider.APIKeyEnv)
	}

	for keyID, envName := range cfg.Encryption.Keys {
		value, ok := getenv(envName)
		if envName == "" || !ok || value == "" {
			errs = append(errs, fmt.Errorf("missing environment variable for encryption key %s: %s", keyID, envName))
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(decoded) != 32 {
			errs = append(errs, fmt.Errorf("encryption key %s must be a base64-encoded 32-byte value", keyID))
		}
	}
	if cfg.Encryption.ActiveKeyID == "" {
		errs = append(errs, errors.New("missing active encryption key ID"))
	} else if _, ok := cfg.Encryption.Keys[cfg.Encryption.ActiveKeyID]; !ok {
		errs = append(errs, fmt.Errorf("missing encryption key reference for active key %s", cfg.Encryption.ActiveKeyID))
	}

	minRank, minOK := tierRank(cfg.Routing.MinTier)
	if !minOK {
		errs = append(errs, fmt.Errorf("unknown tier for min_tier: %s", cfg.Routing.MinTier))
	}
	maxRank, maxOK := tierRank(cfg.Routing.MaxTier)
	if !maxOK {
		errs = append(errs, fmt.Errorf("unknown tier for max_tier: %s", cfg.Routing.MaxTier))
	}
	safeFallback := cfg.Routing.SafeFallbackTier
	if safeFallback == "" {
		safeFallback = "T4"
	}
	if _, ok := tierRank(safeFallback); !ok {
		errs = append(errs, fmt.Errorf("unknown tier for safe_fallback_tier: %s", safeFallback))
	}
	if minOK && maxOK && minRank > maxRank {
		errs = append(errs, fmt.Errorf("min_tier %s exceeds max_tier %s", cfg.Routing.MinTier, cfg.Routing.MaxTier))
	}
	if cfg.SQLite.Retention < 0 {
		errs = append(errs, errors.New("SQLite retention must not be negative"))
	}
	if cfg.SQLite.RetentionMaintenanceInterval < 0 {
		errs = append(errs, errors.New("SQLite retention maintenance interval must not be negative"))
	}
	if cfg.Jev.Timeout < 0 {
		errs = append(errs, errors.New("Jev timeout must not be negative"))
	}
	if cfg.Jev.MaxRetries < 0 {
		errs = append(errs, errors.New("Jev max retries must not be negative"))
	}
	validateRouting := func() {
		for _, value := range []struct {
			name  string
			value float64
		}{
			{"failure escalation cost", cfg.Routing.FailureEscalationCostUSD},
			{"latency penalty", cfg.Routing.LatencyPenaltyUSDPerSecond},
			{"maximum direct cost", cfg.Routing.MaxDirectCostUSD},
			{"maximum expected cost", cfg.Routing.MaxExpectedCostUSD},
		} {
			if !nonnegativeFinite(value.value) {
				errs = append(errs, fmt.Errorf("%s must be finite and nonnegative", value.name))
			}
		}
		for _, value := range []struct {
			name  string
			value float64
		}{
			{"minimum success probability", cfg.Routing.MinSuccessProbability},
			{"minimum Jev confidence", cfg.Routing.MinJevConfidence},
		} {
			if !probability(value.value) {
				errs = append(errs, fmt.Errorf("%s must be finite and in [0, 1]", value.name))
			}
		}
		if cfg.Routing.MaxLatency < 0 {
			errs = append(errs, errors.New("maximum latency must not be negative"))
		}
		for _, rules := range []struct {
			name  string
			rules []SignalFloorConfig
		}{
			{"reasoning", cfg.Routing.ReasoningFloors},
			{"coding", cfg.Routing.CodingFloors},
			{"risk", cfg.Routing.RiskFloors},
			{"underspecification", cfg.Routing.UnderspecificationFloors},
		} {
			for _, rule := range rules.rules {
				if !nonnegativeFinite(rule.Threshold) {
					errs = append(errs, fmt.Errorf("%s signal threshold must be finite and nonnegative", rules.name))
				}
				if _, ok := tierRank(rule.Floor); !ok {
					errs = append(errs, fmt.Errorf("unknown %s signal floor tier: %s", rules.name, rule.Floor))
				}
			}
		}
	}
	validateRouting()

	modelIDs := make(map[string]struct{}, len(cfg.Models))
	for _, model := range cfg.Models {
		if _, exists := modelIDs[model.ID]; exists {
			errs = append(errs, fmt.Errorf("duplicate model ID: %s", model.ID))
		} else {
			modelIDs[model.ID] = struct{}{}
		}
		if _, ok := providerIDs[model.Provider]; !ok {
			errs = append(errs, fmt.Errorf("model %s references missing provider %s", model.ID, model.Provider))
		}
		if _, ok := tierRank(model.Tier); !ok {
			errs = append(errs, fmt.Errorf("unknown tier for model %s: %s", model.ID, model.Tier))
		}
		for _, value := range []struct {
			name  string
			value float64
		}{
			{"input price", model.InputPrice},
			{"cached input price", model.CachedInputPriceUSDPerMillion},
			{"output price", model.OutputPrice},
			{"per-request price", model.PerRequestPriceUSD},
		} {
			if value.value < 0 {
				errs = append(errs, fmt.Errorf("negative %s for model %s", value.name, model.ID))
				continue
			}
			if !nonnegativeFinite(value.value) {
				errs = append(errs, fmt.Errorf("%s for model %s must be finite and nonnegative", value.name, model.ID))
			}
		}
		if model.ContextWindow < 0 {
			errs = append(errs, fmt.Errorf("context window for model %s must not be negative", model.ID))
		}
		if model.MaxOutputTokens < 0 {
			errs = append(errs, fmt.Errorf("maximum output tokens for model %s must not be negative", model.ID))
		}
		if model.LatencyP95 < 0 {
			errs = append(errs, fmt.Errorf("P95 latency for model %s must not be negative", model.ID))
		}
		if !probability(model.SuccessPrior) {
			errs = append(errs, fmt.Errorf("success prior for model %s must be finite and in [0, 1]", model.ID))
		}
		for task, prior := range model.TaskSuccessPriors {
			if !probability(prior) {
				errs = append(errs, fmt.Errorf("success prior for model %s task %s must be finite and in [0, 1]", model.ID, task))
			}
		}
	}

	return errors.Join(errs...)
}

func nonnegativeFinite(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func probability(value float64) bool {
	return nonnegativeFinite(value) && value <= 1
}

func tierRank(tier string) (int, bool) {
	switch tier {
	case "T0":
		return 0, true
	case "T1":
		return 1, true
	case "T2":
		return 2, true
	case "T3":
		return 3, true
	case "T4":
		return 4, true
	case "T5":
		return 5, true
	case "T6":
		return 6, true
	default:
		return 0, false
	}
}
