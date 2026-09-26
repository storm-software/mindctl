package config

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Read decodes one strict YAML configuration document without resolving or
// validating its referenced secrets. It is used by offline catalog commands.
func Read(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("decode config: %w", err)
		}
		return Config{}, fmt.Errorf("decode config: multiple YAML documents are not allowed")
	}
	if cfg.Routing.SafeFallbackTier == "" {
		cfg.Routing.SafeFallbackTier = "T4"
	}
	if cfg.ClientAuth.MaxBodyBytes == 0 {
		cfg.ClientAuth.MaxBodyBytes = DefaultMaxBodyBytes
	}
	for _, provider := range cfg.Providers {
		if provider.ID != "openai" || provider.AuthMode() != ProviderAuthChatGPTOAuthPassthrough {
			continue
		}
		found := false
		for _, model := range cfg.Models {
			if model.ID != "codex-auto-review" {
				continue
			}
			if model.Provider != provider.ID || !model.ExplicitOnly {
				return Config{}, fmt.Errorf("model codex-auto-review must belong to openai and set explicit_only: true for ChatGPT OAuth")
			}
			found = true
		}
		if !found {
			cfg.Models = append(cfg.Models, ModelConfig{
				ID: "codex-auto-review", Provider: provider.ID, Tier: "T4",
				Capabilities:  []string{"chat", "tools", "images", "json_schema"},
				ContextWindow: 272000, MaxOutputTokens: 16384,
				Available: true, ExplicitOnly: true, SuccessPrior: 1,
			})
		}
	}
	return cfg, nil
}

// Load decodes one strict YAML configuration document and validates its secret
// references through getenv.
func Load(path string, getenv func(string) (string, bool)) (Config, error) {
	cfg, err := Read(path)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(getenv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
