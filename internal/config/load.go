package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load decodes one strict YAML configuration document and validates its secret
// references through getenv.
func Load(path string, getenv func(string) (string, bool)) (Config, error) {
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
	if cfg.Routing.SafeFallbackTier == "" {
		cfg.Routing.SafeFallbackTier = "T4"
	}
	if err := cfg.Validate(getenv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
