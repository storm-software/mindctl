package config

import (
	"encoding/base64"
	"errors"
	"fmt"
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

// SQLiteConfig controls local durable storage. Zero retention means unlimited.
type SQLiteConfig struct {
	Path      string        `yaml:"path"`
	Retention time.Duration `yaml:"retention"`
}

// EncryptionConfig maps key IDs to environment-variable names.
type EncryptionConfig struct {
	ActiveKeyID string            `yaml:"active_key_id"`
	Keys        map[string]string `yaml:"keys"`
}

// RoutingConfig bounds automatic routing. An omitted safe fallback defaults to T4.
type RoutingConfig struct {
	MinTier          string `yaml:"min_tier"`
	MaxTier          string `yaml:"max_tier"`
	SafeFallbackTier string `yaml:"safe_fallback_tier"`
}

// ProviderConfig configures one provider endpoint and secret reference.
type ProviderConfig struct {
	ID        string `yaml:"id"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// ModelConfig describes one configured provider model.
type ModelConfig struct {
	ID                string             `yaml:"id"`
	Provider          string             `yaml:"provider"`
	Tier              string             `yaml:"tier"`
	Capabilities      []string           `yaml:"capabilities"`
	ContextWindow     int                `yaml:"context_window"`
	Available         bool               `yaml:"available"`
	InputPrice        float64            `yaml:"input_price"`
	OutputPrice       float64            `yaml:"output_price"`
	SuccessPrior      float64            `yaml:"success_prior"`
	TaskSuccessPriors map[string]float64 `yaml:"task_success_priors"`
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
		if model.InputPrice < 0 {
			errs = append(errs, fmt.Errorf("negative input price for model %s", model.ID))
		}
		if model.OutputPrice < 0 {
			errs = append(errs, fmt.Errorf("negative output price for model %s", model.ID))
		}
	}

	return errors.Join(errs...)
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
