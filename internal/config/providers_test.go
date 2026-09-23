package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func providerStateCatalog() Config {
	return Config{
		Providers: []ProviderConfig{{ID: "openai"}, {ID: "deepseek"}},
		Models: []ModelConfig{
			{ID: "gpt-alpha", Provider: "openai", Available: true},
			{ID: "gpt-beta", Provider: "openai", Available: false},
			{ID: "deepseek-chat", Provider: "deepseek", Available: true},
		},
	}
}

func TestProvidersPathUsesXDGStateHomeAndStandardFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/custom-state")
	path, err := ProvidersPath()
	if err != nil || path != "/tmp/custom-state/mindctl/providers.yaml" {
		t.Fatalf("XDG path = %q, %v", path, err)
	}

	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/example-home")
	path, err = ProvidersPath()
	if err != nil || path != "/tmp/example-home/.local/state/mindctl/providers.yaml" {
		t.Fatalf("fallback path = %q, %v", path, err)
	}
}

func TestProvidersPathRejectsRelativeXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if _, err := ProvidersPath(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("ProvidersPath error = %v", err)
	}
}

func TestApplyProvidersFileMakesItsModelListAuthoritative(t *testing.T) {
	cfg := providerStateCatalog()
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  openai:\n    - gpt-beta\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := ApplyProvidersFile(path, &cfg); err != nil {
		t.Fatalf("ApplyProvidersFile: %v", err)
	}
	want := []bool{false, true, false}
	for index, model := range cfg.Models {
		if model.Available != want[index] {
			t.Errorf("model %s available = %v, want %v", model.ID, model.Available, want[index])
		}
	}
}

func TestMissingProvidersFilePreservesConfiguredAvailability(t *testing.T) {
	cfg := providerStateCatalog()
	if err := ApplyProvidersFile(filepath.Join(t.TempDir(), "missing.yaml"), &cfg); err != nil {
		t.Fatalf("ApplyProvidersFile: %v", err)
	}
	want := []bool{true, false, true}
	for index, model := range cfg.Models {
		if model.Available != want[index] {
			t.Errorf("model %s available = %v, want %v", model.ID, model.Available, want[index])
		}
	}
}

func TestSetModelEnabledInitializesFromCatalogAndWritesAtomically(t *testing.T) {
	cfg := providerStateCatalog()
	path := filepath.Join(t.TempDir(), "state", "mindctl", "providers.yaml")

	if err := SetModelEnabled(path, cfg, "openai", "gpt-beta", true); err != nil {
		t.Fatalf("SetModelEnabled: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"gpt-alpha", "gpt-beta", "deepseek-chat"} {
		if !strings.Contains(string(body), entry) {
			t.Errorf("providers file missing %q:\n%s", entry, body)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("mode = %o, want 600", got)
	}

	loaded := providerStateCatalog()
	if err := ApplyProvidersFile(path, &loaded); err != nil {
		t.Fatal(err)
	}
	for _, model := range loaded.Models {
		if !model.Available {
			t.Errorf("model %s was not enabled", model.ID)
		}
	}
}

func TestSetProviderEnabledUpdatesEveryConfiguredModel(t *testing.T) {
	cfg := providerStateCatalog()
	path := filepath.Join(t.TempDir(), "providers.yaml")

	if err := SetProviderEnabled(path, cfg, "openai", false); err != nil {
		t.Fatalf("SetProviderEnabled: %v", err)
	}
	loaded := providerStateCatalog()
	if err := ApplyProvidersFile(path, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Models[0].Available || loaded.Models[1].Available || !loaded.Models[2].Available {
		t.Fatalf("availability = [%v %v %v]", loaded.Models[0].Available, loaded.Models[1].Available, loaded.Models[2].Available)
	}
}

func TestSetProviderEnabledRejectsProviderWithoutModels(t *testing.T) {
	cfg := Config{Providers: []ProviderConfig{{ID: "empty"}}}
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := SetProviderEnabled(path, cfg, "empty", true); err == nil || !strings.Contains(err.Error(), "no configured models") {
		t.Fatalf("SetProviderEnabled error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("providers file exists after rejected update: %v", err)
	}
}

func TestSetModelEnabledRejectsInvalidCatalogRelationship(t *testing.T) {
	cfg := Config{Models: []ModelConfig{{ID: "orphan", Provider: "missing", Available: true}}}
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := SetModelEnabled(path, cfg, "missing", "orphan", true); err == nil || !strings.Contains(err.Error(), "missing provider") {
		t.Fatalf("SetModelEnabled error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("providers file exists after rejected update: %v", err)
	}
}

func TestProvidersFileRejectsUnknownAndDuplicateEntries(t *testing.T) {
	for name, body := range map[string]string{
		"unknown provider": "providers:\n  missing:\n    - model\n",
		"unknown model":    "providers:\n  openai:\n    - missing\n",
		"duplicate model":  "providers:\n  openai:\n    - gpt-alpha\n    - gpt-alpha\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers.yaml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := providerStateCatalog()
			if err := ApplyProvidersFile(path, &cfg); err == nil {
				t.Fatal("ApplyProvidersFile succeeded")
			}
		})
	}
}
