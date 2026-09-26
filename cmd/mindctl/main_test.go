package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
	"gopkg.in/yaml.v3"
)

func mainConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("MAIN_GATEWAY_TOKEN", "private-gateway-token")
	t.Setenv("MAIN_LAYA_TOKEN", "private-laya-token")
	t.Setenv("MAIN_PROVIDER_KEY", "private-provider-key")
	t.Setenv("MAIN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	cfg := config.Config{
		Listen: "127.0.0.1:0", ClientAuth: config.ClientAuthConfig{TokenEnv: "MAIN_GATEWAY_TOKEN"},
		Classifier: config.ClassifierConfig{Endpoint: "https://laya.example.com", TokenEnv: "MAIN_LAYA_TOKEN"},
		SQLite:     config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "app.db")},
		Encryption: config.EncryptionConfig{ActiveKeyID: "active", Keys: map[string]string{"active": "MAIN_ENCRYPTION_KEY"}},
		Routing:    config.RoutingConfig{MinTier: "T0", MaxTier: "T6"},
		Providers:  []config.ProviderConfig{{ID: "openai", BaseURL: "https://provider.example.com", APIKeyEnv: "MAIN_PROVIDER_KEY"}},
		Models:     []config.ModelConfig{{ID: "model", Provider: "openai", Tier: "T4", Available: true, SuccessPrior: 1}},
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func commandCatalogConfig(t *testing.T) string {
	t.Helper()
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "openai"}, {ID: "deepseek"}},
		Models: []config.ModelConfig{
			{ID: "gpt-alpha", Provider: "openai", Available: true},
			{ID: "gpt-beta", Provider: "openai", Available: false},
			{ID: "deepseek-chat", Provider: "deepseek", Available: true},
		},
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRejectsBadFlagsAndMissingSecrets(t *testing.T) {
	path := mainConfig(t)
	t.Setenv("MAIN_ENCRYPTION_KEY", "")
	for _, args := range [][]string{{"-unknown"}, {"unexpected"}, {"-config", path}} {
		var stderr bytes.Buffer
		err := run(context.Background(), args, io.Discard, &stderr)
		if err == nil {
			t.Fatalf("run(%v) succeeded", args)
		}
		if len(args) == 2 && !strings.Contains(err.Error(), "MAIN_ENCRYPTION_KEY") {
			t.Fatalf("missing key error = %v", err)
		}
		if strings.Contains(err.Error()+stderr.String(), "private-") {
			t.Fatal("startup exposed a secret")
		}
	}
}

func TestRunReadsXDGConfigWhenConfigFlagIsOmitted(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.Mkdir(filepath.Join(configHome, "mindctl"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "mindctl", "config.yaml"), []byte("invalid_xdg_config: true\n"), 0600); err != nil {
		t.Fatal(err)
	}

	err := run(context.Background(), nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "invalid_xdg_config") {
		t.Fatalf("omitted config error = %v; want XDG config validation error", err)
	}
}

func TestRunPrefersExplicitConfigOverXDGConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.Mkdir(filepath.Join(configHome, "mindctl"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "mindctl", "config.yaml"), []byte("invalid_xdg_config: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(t.TempDir(), "explicit.yaml")
	if err := os.WriteFile(explicit, []byte("invalid_explicit_config: true\n"), 0600); err != nil {
		t.Fatal(err)
	}

	err := run(context.Background(), []string{"--config", explicit}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "invalid_explicit_config") || strings.Contains(err.Error(), "invalid_xdg_config") {
		t.Fatalf("explicit config error = %v; want explicit config validation error rather than XDG config", err)
	}
}

func TestStatusCommand(t *testing.T) {
	for _, test := range []struct {
		name             string
		config           string
		claudeConfig     string
		claudeUnreadable bool
		args             []string
		want             string
		wantErr          string
	}{
		{name: "missing config table", want: "HARNESS  STATUS\ncodex    disconnected\nclaude   disconnected\n"},
		{name: "connected table", config: "model_provider = \"mindctl\"\n", want: "HARNESS  STATUS\ncodex    connected\nclaude   disconnected\n"},
		{name: "Claude configured", claudeConfig: `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8080"}}`, args: []string{"claude"}, want: "connected\n"},
		{name: "Claude trailing slash", claudeConfig: `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8080/"}}`, args: []string{"claude"}, want: "connected\n"},
		{name: "Claude disconnected", claudeConfig: `{"env":{"ANTHROPIC_BASE_URL":"https://other.example"}}`, args: []string{"claude"}, want: "disconnected\n"},
		{name: "Claude nested env ignored", claudeConfig: `{"project":{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8080"}}}`, args: []string{"claude"}, want: "disconnected\n"},
		{name: "both configured", config: "model_provider = \"mindctl\"\n", claudeConfig: `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8080"}}`, want: "HARNESS  STATUS\ncodex    connected\nclaude   connected\n"},
		{name: "malformed Claude", claudeConfig: `{`, args: []string{"claude"}, wantErr: "read Claude settings"},
		{name: "invalid Claude object", claudeConfig: `null`, args: []string{"claude"}, wantErr: "read Claude settings"},
		{name: "unreadable Claude", claudeUnreadable: true, args: []string{"claude"}, wantErr: "read Claude settings"},
		{name: "Codex ignores malformed Claude", config: "model_provider = \"mindctl\"\n", claudeConfig: `{`, args: []string{"codex"}, want: "connected\n"},
		{name: "connected codex", config: "model_provider = \"mindctl\"\n", args: []string{"codex"}, want: "connected\n"},
		{name: "different provider", config: "model_provider = \"openai\"\n", args: []string{"codex"}, want: "disconnected\n"},
		{name: "comment is not connection", config: "# model_provider = \"mindctl\"\n", args: []string{"codex"}, want: "disconnected\n"},
		{name: "nested setting is not connection", config: "[profile.test]\nmodel_provider = \"mindctl\"\n", args: []string{"codex"}, want: "disconnected\n"},
		{name: "invalid config", config: "model_provider = \"mindctl\n", args: []string{"codex"}, wantErr: "read Codex config"},
		{name: "unknown harness", args: []string{"other"}, wantErr: "unknown harness"},
		{name: "extra argument", args: []string{"codex", "other"}, wantErr: "at most 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "missing-gateway-config"))
			if test.config != "" {
				if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(test.config), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if test.claudeConfig != "" || test.claudeUnreadable {
				if err := os.Mkdir(filepath.Join(home, ".claude"), 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(home, ".claude", "settings.json")
				if test.claudeUnreadable {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(test.claudeConfig), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout bytes.Buffer
			err := run(context.Background(), append([]string{"status"}, test.args...), &stdout, io.Discard)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("status error = %v; want %q", err, test.wantErr)
				}
				if stdout.Len() != 0 {
					t.Fatalf("status wrote stdout on error: %q", stdout.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if got := stdout.String(); got != test.want {
				t.Fatalf("status output = %q; want %q", got, test.want)
			}
		})
	}
}

func TestResolveDebugUsesFlagThenEnvironmentThenConfig(t *testing.T) {
	tests := []struct {
		name                  string
		config, changed, flag bool
		environment           string
		environmentExists     bool
		want                  bool
	}{
		{name: "config", config: true, want: true},
		{name: "environment enables", environment: "true", environmentExists: true, want: true},
		{name: "environment disables config", config: true, environment: "false", environmentExists: true, want: false},
		{name: "flag enables", changed: true, flag: true, environment: "false", environmentExists: true, want: true},
		{name: "flag disables", config: true, changed: true, flag: false, environment: "true", environmentExists: true, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveDebug(test.config, test.changed, test.flag, func(string) (string, bool) {
				return test.environment, test.environmentExists
			})
			if err != nil || got != test.want {
				t.Fatalf("resolveDebug() = %v, %v; want %v, nil", got, err, test.want)
			}
		})
	}
}

func TestResolveDebugRejectsInvalidEnvironmentValue(t *testing.T) {
	_, err := resolveDebug(false, false, false, func(string) (string, bool) { return "sometimes", true })
	if err == nil || !strings.Contains(err.Error(), "MINDCTL_DEBUG") {
		t.Fatalf("resolveDebug error = %v", err)
	}
}

func TestLoadGatewayConfigCachesProvidersFileAvailability(t *testing.T) {
	path := mainConfig(t)
	cfg, err := config.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Models = append(cfg.Models, config.ModelConfig{ID: "disabled", Provider: "openai", Tier: "T4", Available: false, SuccessPrior: 1})
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	stateDir := filepath.Join(stateHome, "mindctl")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateDir, "providers.yaml")
	if err := os.WriteFile(statePath, []byte("providers:\n  openai:\n    - disabled\n"), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadGatewayConfig(path)
	if err != nil {
		t.Fatalf("loadGatewayConfig: %v", err)
	}
	if loaded.Models[0].Available || !loaded.Models[1].Available {
		t.Fatalf("availability = [%v %v], want [false true]", loaded.Models[0].Available, loaded.Models[1].Available)
	}
	if err := os.WriteFile(statePath, []byte("providers:\n  openai:\n    - model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if loaded.Models[0].Available || !loaded.Models[1].Available {
		t.Fatal("loaded catalog changed after the providers file was rewritten")
	}
}

func TestModelListGroupsEnabledModelsAndAllShowsStatuses(t *testing.T) {
	path := commandCatalogConfig(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "model", "list"}, &stdout, io.Discard); err != nil {
		t.Fatalf("model list: %v", err)
	}
	if got, want := stdout.String(), "openai\n  gpt-alpha\ndeepseek\n  deepseek-chat\n"; got != want {
		t.Fatalf("model list output = %q; want %q", got, want)
	}

	stdout.Reset()
	if err := run(context.Background(), []string{"--config", path, "model", "list", "--all"}, &stdout, io.Discard); err != nil {
		t.Fatalf("model list --all: %v", err)
	}
	if got, want := stdout.String(), "openai\n  gpt-alpha (enabled)\n  gpt-beta (disabled)\ndeepseek\n  deepseek-chat (enabled)\n"; got != want {
		t.Fatalf("model list --all output = %q; want %q", got, want)
	}
}

func TestModelEnableAndDisablePersistExactOrProviderWideChanges(t *testing.T) {
	path := commandCatalogConfig(t)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	if err := run(context.Background(), []string{"--config", path, "model", "enable", "openai.gpt-beta"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("model enable: %v", err)
	}
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "model", "list", "--all"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "gpt-beta (enabled)") {
		t.Fatalf("enabled model missing:\n%s", stdout.String())
	}

	if err := run(context.Background(), []string{"--config", path, "model", "disable", "openai"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("model disable provider: %v", err)
	}
	stdout.Reset()
	if err := run(context.Background(), []string{"--config", path, "model", "list"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "deepseek\n  deepseek-chat\n"; got != want {
		t.Fatalf("model list output = %q; want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(stateHome, "mindctl", "providers.yaml")); err != nil {
		t.Fatalf("providers file: %v", err)
	}
}

func TestProviderCommandsAliasProviderWideModelChanges(t *testing.T) {
	path := commandCatalogConfig(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := run(context.Background(), []string{"--config", path, "provider", "disable", "openai"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("provider disable: %v", err)
	}
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "provider", "list", "--all"}, &stdout, io.Discard); err != nil {
		t.Fatalf("provider list --all: %v", err)
	}
	if got, want := stdout.String(), "openai (disabled)\ndeepseek (enabled)\n"; got != want {
		t.Fatalf("provider list --all output = %q; want %q", got, want)
	}

	if err := run(context.Background(), []string{"--config", path, "provider", "enable", "openai"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("provider enable: %v", err)
	}
	stdout.Reset()
	if err := run(context.Background(), []string{"--config", path, "provider", "list"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "openai\ndeepseek\n"; got != want {
		t.Fatalf("provider list output = %q; want %q", got, want)
	}
}

func TestHistoryCommandListsAndFiltersRequests(t *testing.T) {
	path := mainConfig(t)
	cfg, err := config.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := contentcrypto.New("active", map[string][]byte{
		"active": bytes.Repeat([]byte{4}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), sqlite.Options{
		Path: cfg.SQLite.Path, Keyring: keyring,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := conversation.New(db)

	first, err := svc.Start(context.Background(), "client-a", inference.Request{
		Input: []inference.Item{{Type: "message", Role: "user", Text: "hidden request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstAttempt, err := svc.BeginAttempt(context.Background(), first, router.Decision{
		Provider: "anthropic", ModelID: "claude-small", Tier: domain.T2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.FailAttempt(
		context.Background(),
		firstAttempt,
		"upstream-first",
		context.DeadlineExceeded,
	); err != nil {
		t.Fatal(err)
	}

	second, err := svc.Start(context.Background(), "client-b", inference.Request{
		Input: []inference.Item{{Type: "message", Role: "user", Text: "visible request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.BeginAttempt(context.Background(), second, router.Decision{
		Provider: "openai", ModelID: "gpt-large", Tier: domain.T4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CommitResult(context.Background(), second, router.Pin{
		Provider: "openai", ModelID: "gpt-large", Floor: domain.T4,
	}, inference.Result{
		ProviderRequestID: "upstream-second",
		Status:            "completed",
		Output: []inference.Item{{
			Type: "message", Role: "assistant", Text: "visible response",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	firstCreated := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	secondCreated := firstCreated.Add(time.Hour)
	for _, update := range []struct {
		responseID string
		created    time.Time
	}{{first.ResponseID, firstCreated}, {second.ResponseID, secondCreated}} {
		if _, err := db.SQL().Exec(
			"UPDATE responses SET created_at = ? WHERE id = ?",
			update.created.UnixNano(),
			update.responseID,
		); err != nil {
			t.Fatal(err)
		}
	}

	var stdout bytes.Buffer
	err = run(context.Background(), []string{
		"--config", path,
		"history",
		"--provider", "openai",
		"--model", "gpt-large",
		"--status", "succeeded",
		"--since", "2026-09-23T12:30:00Z",
		"--until", "2026-09-23T14:00:00Z",
		"--limit", "1",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	output := stdout.String()
	for _, want := range []string{
		"2026-09-23T13:00:00Z  " + second.ResponseID + "  completed",
		"request:\n    visible request",
		"attempt: openai/gpt-large (T4, succeeded)",
		"response:\n    visible response",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("history output missing %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{"hidden request", "claude-small"} {
		if strings.Contains(output, unwanted) {
			t.Errorf("history output unexpectedly contains %q:\n%s", unwanted, output)
		}
	}
}

func TestHistoryCommandRejectsInvalidFilters(t *testing.T) {
	path := mainConfig(t)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"negative limit", []string{"history", "--limit", "-1"}, "limit must be nonnegative"},
		{"invalid since", []string{"history", "--since", "yesterday"}, "parse --since"},
		{
			"reversed range",
			[]string{"history", "--since", "2026-09-24T00:00:00Z", "--until", "2026-09-23T00:00:00Z"},
			"since must not be after until",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--config", path}, test.args...)
			err := run(context.Background(), args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("history error = %v; want %q", err, test.want)
			}
		})
	}
}

func TestModelCommandsRejectUnknownTargets(t *testing.T) {
	path := commandCatalogConfig(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, target := range []string{"missing", "openai.missing"} {
		err := run(context.Background(), []string{"--config", path, "model", "enable", target}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("target %q error = %v", target, err)
		}
	}
}

func TestModelCommandRejectsInvalidCatalogBeforeWritingState(t *testing.T) {
	cfg := config.Config{Models: []config.ModelConfig{{ID: "orphan", Provider: "missing", Available: true}}}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	err = run(context.Background(), []string{"--config", path, "model", "enable", "missing.orphan"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "missing provider") {
		t.Fatalf("model enable error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateHome, "mindctl", "providers.yaml")); !os.IsNotExist(err) {
		t.Fatalf("providers file exists after rejected command: %v", err)
	}
}

func TestConfigListReadsXDGConfigWithNestedGroups(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "mindctl")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("listen: :8080\nclient_auth:\n  token_env: MINDCTL_GATEWAY_TOKEN\nproviders:\n  - id: openai\n    auth: chatgpt_oauth_passthrough\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"config", "list"}, &stdout, &stderr); err != nil {
		t.Fatalf("config list: %v", err)
	}
	if got, want := stdout.String(), "listen: :8080\nclient_auth:\n  token_env: MINDCTL_GATEWAY_TOKEN\nproviders:\n  - id: openai\n    auth: chatgpt_oauth_passthrough\n"; got != want {
		t.Errorf("config list output = %q; want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("config list wrote stderr: %q", stderr.String())
	}
}

func TestBareManagementCommandsList(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "mindctl")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("listen: :8080\n"), 0600); err != nil {
		t.Fatal(err)
	}

	catalogPath := commandCatalogConfig(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "config", args: []string{"config"}, want: "listen: :8080\n"},
		{name: "model", args: []string{"--config", catalogPath, "model"}, want: "openai\n  gpt-alpha\ndeepseek\n  deepseek-chat\n"},
		{name: "provider", args: []string{"--config", catalogPath, "provider"}, want: "openai\ndeepseek\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := run(context.Background(), test.args, &stdout, io.Discard); err != nil {
				t.Fatalf("run(%v): %v", test.args, err)
			}
			if got := stdout.String(); got != test.want {
				t.Fatalf("run(%v) output = %q; want %q", test.args, got, test.want)
			}
		})
	}
}

func TestConfigGetReadsXDGConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "mindctl")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("routing:\n  min_tier: T0\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"config", "get", "routing.min_tier"}, &stdout, io.Discard); err != nil {
		t.Fatalf("config get: %v", err)
	}
	if got, want := stdout.String(), "T0\n"; got != want {
		t.Errorf("config get output = %q; want %q", got, want)
	}
}

func TestConfigSetWritesTypedNestedValueToXDGConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "mindctl")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(path, []byte("routing:\n  min_tier: T0\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), []string{"config", "set", "routing.max_direct_cost_usd", "42"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("config set: %v", err)
	}
	var doc map[string]any
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	routing, ok := doc["routing"].(map[string]any)
	if !ok {
		t.Fatalf("routing = %#v; want mapping", doc["routing"])
	}
	if got, want := routing["max_direct_cost_usd"], 42; got != want {
		t.Errorf("written value = %#v; want %#v", got, want)
	}
}

func TestRootHelpDocumentsVersionAndConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run help: %v", err)
	}
	for _, want := range []string{"version", "status", "--config", "--debug"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help output missing %q:\n%s", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("help wrote to stderr: %q", stderr.String())
	}
}

func TestBinaryVersionUsesDevelopmentMetadataWithoutConfiguration(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mindctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	cmd := exec.Command(binary, "version")
	cmd.Dir = t.TempDir()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if got, want := string(output), "version=dev\ncommit=none\ndate=unknown\n"; got != want {
		t.Fatalf("version output=%q want=%q", got, want)
	}
}

func TestBinaryVersionRejectsAdditionalArguments(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mindctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	output, err := exec.Command(binary, "version", "extra").CombinedOutput()
	if err == nil {
		t.Fatal("version with an argument succeeded")
	}
	if got, want := string(output), "mindctl: unexpected arguments for version\n"; got != want {
		t.Fatalf("stderr=%q want=%q", got, want)
	}
}

func TestBinaryVersionUsesLinkerInjectedMetadata(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mindctl")
	build := exec.Command(
		"go",
		"build",
		"-ldflags",
		"-X main.version=1.2.3 -X main.commit=deadbeef -X main.date=2026-09-22T12:00:00Z",
		"-o",
		binary,
		".",
	)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	cmd := exec.Command(binary, "version")
	cmd.Dir = t.TempDir()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if got, want := string(output), "version=1.2.3\ncommit=deadbeef\ndate=2026-09-22T12:00:00Z\n"; got != want {
		t.Fatalf("version output=%q want=%q", got, want)
	}
}

func TestServeGracefullyWaitsForActiveRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, shuttingDown := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})}
	server.RegisterOnShutdown(func() { close(shuttingDown) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, time.Second) }()
	response := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			response <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		response <- string(body)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-shuttingDown:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown returned while request was active: %v", err)
	default:
	}
	close(release)
	select {
	case body := <-response:
		if body != "finished" {
			t.Fatalf("response = %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestServeShutdownDeadlineAndServeFailureAreErrors(t *testing.T) {
	t.Run("serve failure", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_ = listener.Close()
		if err := serve(context.Background(), &http.Server{}, listener, time.Second); err == nil {
			t.Fatal("serve failure was ignored")
		}
	})
	t.Run("shutdown deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(started); <-release })}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		done := make(chan error, 1)
		go func() { done <- serve(ctx, server, listener, 20*time.Millisecond) }()
		go func() {
			resp, err := http.Get("http://" + listener.Addr().String())
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not start")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "shutdown") {
				t.Fatalf("shutdown error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown was unbounded")
		}
	})
}

func TestBinarySignalsAndContentFreeStartup(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mindctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			path := mainConfig(t)
			cmd := exec.Command(binary, "-config", path)
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			lines := make(chan string, 8)
			go func() {
				scanner := bufio.NewScanner(stderr)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
				close(lines)
			}()
			var logs, address string
			deadline := time.After(5 * time.Second)
			for address == "" {
				select {
				case line, ok := <-lines:
					if !ok {
						t.Fatalf("process stopped before listening: %s", logs)
					}
					logs += line + "\n"
					if strings.HasPrefix(line, "mindctl: listening on ") {
						address = strings.TrimPrefix(line, "mindctl: listening on ")
					}
				case <-deadline:
					t.Fatal("process did not start")
				}
			}
			if !strings.Contains(logs, "unlimited") || !strings.Contains(logs, "disk") || !strings.Contains(logs, "governance") || strings.Contains(logs, "private-") {
				t.Fatalf("startup logs = %q", logs)
			}
			client := &http.Client{Timeout: time.Second}
			for _, route := range []string{"/healthz", "/readyz"} {
				resp, err := client.Get("http://" + address + route)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("%s status = %d", route, resp.StatusCode)
				}
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(7 * time.Second):
				t.Fatal("process ignored shutdown signal")
			}
		})
	}
	t.Run("startup failure exits nonzero", func(t *testing.T) {
		path := mainConfig(t)
		t.Setenv("MAIN_ENCRYPTION_KEY", "")
		cmd := exec.Command(binary, "-config", path)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("missing key exited successfully")
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "MAIN_ENCRYPTION_KEY") || strings.Contains(stderr.String(), "private-") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})
}
