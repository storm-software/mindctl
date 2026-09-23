package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProvidersFile is the persisted allow-list of provider model IDs.
type ProvidersFile struct {
	Providers map[string][]string `yaml:"providers"`
}

// ProvidersPath resolves the per-user provider state file according to the
// XDG Base Directory specification's state directory convention.
func ProvidersPath() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome != "" && !filepath.IsAbs(stateHome) {
		return "", errors.New("XDG_STATE_HOME must be an absolute path")
	}
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "mindctl", "providers.yaml"), nil
}

// ApplyProvidersFile overlays the configured catalog with the persisted
// allow-list. A missing file preserves the catalog's configured availability.
func ApplyProvidersFile(path string, cfg *Config) error {
	if err := cfg.ValidateCatalog(); err != nil {
		return err
	}
	state, exists, err := readProvidersFile(path, *cfg)
	if err != nil || !exists {
		return err
	}
	enabled := enabledModels(state)
	for index := range cfg.Models {
		model := &cfg.Models[index]
		model.Available = enabled[modelIdentity(model.Provider, model.ID)]
	}
	return nil
}

// SetModelEnabled enables or disables one configured provider model.
func SetModelEnabled(path string, cfg Config, providerID, modelID string, enabled bool) error {
	if err := cfg.ValidateCatalog(); err != nil {
		return err
	}
	if !catalogHasModel(cfg, providerID, modelID) {
		return fmt.Errorf("unknown model %s.%s", providerID, modelID)
	}
	state, _, err := readProvidersFile(path, cfg)
	if err != nil {
		return err
	}
	setStateModel(&state, providerID, modelID, enabled)
	if err := validateProvidersFile(state, cfg); err != nil {
		return err
	}
	return writeProvidersFile(path, state)
}

// SetProviderEnabled enables or disables every configured model for a provider.
func SetProviderEnabled(path string, cfg Config, providerID string, enabled bool) error {
	if err := cfg.ValidateCatalog(); err != nil {
		return err
	}
	if !catalogHasProvider(cfg, providerID) {
		return fmt.Errorf("unknown provider %s", providerID)
	}
	state, _, err := readProvidersFile(path, cfg)
	if err != nil {
		return err
	}
	modelCount := 0
	for _, model := range cfg.Models {
		if model.Provider == providerID {
			modelCount++
			setStateModel(&state, providerID, model.ID, enabled)
		}
	}
	if enabled && modelCount == 0 {
		return fmt.Errorf("provider %s has no configured models", providerID)
	}
	if err := validateProvidersFile(state, cfg); err != nil {
		return err
	}
	return writeProvidersFile(path, state)
}

func readProvidersFile(path string, cfg Config) (ProvidersFile, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return providersFileFromCatalog(cfg), false, nil
	}
	if err != nil {
		return ProvidersFile{}, false, fmt.Errorf("open providers file: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var state ProvidersFile
	if err := decoder.Decode(&state); err != nil {
		return ProvidersFile{}, true, fmt.Errorf("decode providers file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return ProvidersFile{}, true, fmt.Errorf("decode providers file: %w", err)
		}
		return ProvidersFile{}, true, errors.New("decode providers file: multiple YAML documents are not allowed")
	}
	if state.Providers == nil {
		state.Providers = make(map[string][]string)
	}
	if err := validateProvidersFile(state, cfg); err != nil {
		return ProvidersFile{}, true, err
	}
	return state, true, nil
}

func providersFileFromCatalog(cfg Config) ProvidersFile {
	state := ProvidersFile{Providers: make(map[string][]string)}
	for _, provider := range cfg.Providers {
		state.Providers[provider.ID] = nil
	}
	for _, model := range cfg.Models {
		if model.Available {
			state.Providers[model.Provider] = append(state.Providers[model.Provider], model.ID)
		}
	}
	return state
}

func validateProvidersFile(state ProvidersFile, cfg Config) error {
	for providerID, models := range state.Providers {
		if !catalogHasProvider(cfg, providerID) {
			return fmt.Errorf("providers file references unknown provider %s", providerID)
		}
		seen := make(map[string]struct{}, len(models))
		for _, modelID := range models {
			if !catalogHasModel(cfg, providerID, modelID) {
				return fmt.Errorf("providers file references unknown model %s.%s", providerID, modelID)
			}
			if _, exists := seen[modelID]; exists {
				return fmt.Errorf("providers file contains duplicate model %s.%s", providerID, modelID)
			}
			seen[modelID] = struct{}{}
		}
	}
	return nil
}

func enabledModels(state ProvidersFile) map[string]bool {
	enabled := make(map[string]bool)
	for providerID, models := range state.Providers {
		for _, modelID := range models {
			enabled[modelIdentity(providerID, modelID)] = true
		}
	}
	return enabled
}

func setStateModel(state *ProvidersFile, providerID, modelID string, enabled bool) {
	models := state.Providers[providerID]
	position := -1
	for index, candidate := range models {
		if candidate == modelID {
			position = index
			break
		}
	}
	if enabled && position < 0 {
		state.Providers[providerID] = append(models, modelID)
	}
	if !enabled && position >= 0 {
		state.Providers[providerID] = append(models[:position], models[position+1:]...)
	}
}

func writeProvidersFile(path string, state ProvidersFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create providers directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".providers-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary providers file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return fmt.Errorf("set providers file permissions: %w", err)
	}
	encoder := yaml.NewEncoder(temporary)
	encoder.SetIndent(2)
	if err := encoder.Encode(state); err != nil {
		encoder.Close()
		temporary.Close()
		return fmt.Errorf("encode providers file: %w", err)
	}
	if err := encoder.Close(); err != nil {
		temporary.Close()
		return fmt.Errorf("close providers encoder: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync providers file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close providers file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace providers file: %w", err)
	}
	return nil
}

func catalogHasProvider(cfg Config, providerID string) bool {
	for _, provider := range cfg.Providers {
		if provider.ID == providerID {
			return true
		}
	}
	return false
}

func catalogHasModel(cfg Config, providerID, modelID string) bool {
	for _, model := range cfg.Models {
		if model.Provider == providerID && model.ID == modelID {
			return true
		}
	}
	return false
}

func modelIdentity(providerID, modelID string) string {
	return providerID + "\x00" + modelID
}
