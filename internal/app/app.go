// Package app owns the gateway's initialized dependencies and their lifecycle.
package app

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/storm-software/mindctl/internal/api"
	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/classifier/laya"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/debugtrace"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/headroom"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/provider/anthropic"
	"github.com/storm-software/mindctl/internal/provider/gemini"
	"github.com/storm-software/mindctl/internal/provider/openai"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
)

type App struct {
	store                *sqlite.DB
	keyring              *contentcrypto.Keyring
	classifier           classifier.Classifier
	httpClient           *http.Client
	executor             *executor.Service
	policy               *router.Policy
	catalog              []domain.Model
	minTier, maxTier     domain.Tier
	safeFallbackTier     domain.Tier
	providerCredentials  map[string]bool
	providerAvailability map[string]bool
	maintenance          *retentionMaintenance
	handler              http.Handler
	closeOnce            sync.Once
	closeErr             error
	debugTrace           *debugtrace.Trace
	headroomManager      *headroom.Manager
}

// New validates secret references again so callers need not use config.Load.
// Resolved values are never written back into the caller's configuration.
func New(ctx context.Context, cfg config.Config) (*App, error) {
	return newWithLookup(ctx, cfg, os.LookupEnv)
}

func newWithLookup(ctx context.Context, cfg config.Config, lookup func(string) (string, bool)) (*App, error) {
	return newWithLookupWithMaintenance(ctx, cfg, lookup, retentionMaintenanceOptions{})
}

func newWithLookupWithMaintenance(ctx context.Context, cfg config.Config, lookup func(string) (string, bool), maintenanceOptions retentionMaintenanceOptions) (*App, error) {
	// Validate and consume the same snapshot, even if the environment changes
	// during startup. The temporary map is not retained by the application.
	type secret struct {
		value  string
		exists bool
	}
	secrets := make(map[string]secret)
	getenv := func(name string) (string, bool) {
		if value, ok := secrets[name]; ok {
			return value.value, value.exists
		}
		value, exists := lookup(name)
		secrets[name] = secret{value, exists}
		return value, exists
	}
	if err := cfg.Validate(getenv); err != nil {
		return nil, err
	}
	keys := make(map[string][]byte, len(cfg.Encryption.Keys))
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	for id, envName := range cfg.Encryption.Keys {
		value, _ := getenv(envName)
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(decoded) != 32 {
			return nil, errors.New("invalid encryption key")
		}
		keys[id] = decoded
	}
	keyring, err := contentcrypto.New(cfg.Encryption.ActiveKeyID, keys)
	if err != nil {
		return nil, err
	}

	a := &App{
		keyring: keyring, policy: router.NewPolicy(policyConfig(cfg.Routing)),
		providerCredentials: make(map[string]bool), providerAvailability: make(map[string]bool),
		httpClient: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second,
		}},
	}
	a.minTier, _ = domain.ParseTier(cfg.Routing.MinTier)
	a.maxTier, _ = domain.ParseTier(cfg.Routing.MaxTier)
	fallback := cfg.Routing.SafeFallbackTier
	if fallback == "" {
		fallback = "T4"
	}
	a.safeFallbackTier, _ = domain.ParseTier(fallback)
	chatGPTOAuthProviders := make(map[string]bool)
	claudeOAuthProviders := make(map[string]bool)
	for _, providerConfig := range cfg.Providers {
		switch providerConfig.AuthMode() {
		case config.ProviderAuthAPIKey:
			value, exists := getenv(providerConfig.APIKeyEnv)
			a.providerCredentials[providerConfig.ID] = exists && value != ""
		case config.ProviderAuthChatGPTOAuthPassthrough:
			a.providerCredentials[providerConfig.ID] = true
			chatGPTOAuthProviders[providerConfig.ID] = true
		case config.ProviderAuthClaudeOAuthPassthrough:
			a.providerCredentials[providerConfig.ID] = true
			claudeOAuthProviders[providerConfig.ID] = true
		}
		a.providerAvailability[providerConfig.ID] = validEndpoint(providerConfig.BaseURL)
	}
	for index, model := range cfg.Models {
		tier, _ := domain.ParseTier(model.Tier)
		mapped := domain.Model{
			ID: model.ID, UpstreamID: model.ID, Provider: model.Provider, Tier: tier,
			ContextWindow: int64(model.ContextWindow), MaxOutputTokens: int64(model.MaxOutputTokens),
			LatencyP95: model.LatencyP95, Available: model.Available, ExplicitOnly: model.ExplicitOnly, Order: index,
			Pricing:             domain.Pricing{InputPerMillion: model.InputPrice, CachedInputPerMillion: model.CachedInputPrice(), OutputPerMillion: model.OutputPrice, PerRequestUSD: model.PerRequestPriceUSD},
			DefaultSuccessPrior: model.SuccessPrior, SuccessPriors: make(map[domain.TaskType]float64, len(model.TaskSuccessPriors)),
		}
		for _, capability := range model.Capabilities {
			switch capability {
			case "chat", "text":
				mapped.Capabilities.Text = true
			case "tools", "functions":
				mapped.Capabilities.Functions = true
			case "images":
				mapped.Capabilities.Images = true
			case "json_schema":
				mapped.Capabilities.JSONSchema = true
			case "web_search":
				if mapped.Capabilities.HostedTools == nil {
					mapped.Capabilities.HostedTools = make(map[string]bool)
				}
				mapped.Capabilities.HostedTools["web_search"] = true
			}
		}
		for task, prior := range model.TaskSuccessPriors {
			mapped.SuccessPriors[domain.TaskType(task)] = prior
		}
		a.catalog = append(a.catalog, mapped)
	}
	eligible, _ := router.EligibleModels(router.EligibilityInput{
		Models: a.catalog, Floor: domain.T0, MinTier: &a.minTier, MaxTier: &a.maxTier,
		ProviderCredentials: a.providerCredentials, ProviderAvailability: a.providerAvailability,
	})
	if len(eligible) == 0 {
		return nil, errors.New("at least one enabled model with a usable provider is required")
	}
	providers, err := configuredProviders(cfg.Providers, getenv, a.httpClient)
	if err != nil {
		a.httpClient.CloseIdleConnections()
		return nil, err
	}
	// Open owns cleanup on failure, including failed migrations. It is last so
	// there are no later fallible steps that could leak an initialized database.
	a.store, err = sqlite.Open(ctx, sqlite.Options{Path: cfg.SQLite.Path, Keyring: keyring})
	if err != nil {
		return nil, errors.New("initialize SQLite storage failed")
	}
	if cfg.Debug {
		a.debugTrace, err = debugtrace.Open()
		if err != nil {
			a.httpClient.CloseIdleConnections()
			_ = a.store.Close()
			return nil, err
		}
	}
	a.maintenance = newRetentionMaintenance(cfg.SQLite.Retention, cfg.SQLite.RetentionMaintenanceInterval, a.store.DeleteExpiredContent, maintenanceOptions)
	classifierToken, _ := getenv(cfg.Classifier.TokenEnv)
	a.classifier = laya.NewClient(cfg.Classifier, classifierToken, a.httpClient)
	var debugLogger *slog.Logger
	if a.debugTrace != nil {
		debugLogger = a.debugTrace.Logger()
	}
	var compressor headroom.Compressor
	if cfg.Headroom.Enabled {
		cacheDir, cacheErr := os.UserCacheDir()
		if cacheErr != nil {
			cacheDir = os.TempDir()
		}
		provisioner := headroom.NewProvisioner(filepath.Join(cacheDir, "mindctl"), a.httpClient, nil)
		a.headroomManager = headroom.NewManager(cfg.Headroom, provisioner, a.httpClient, headroom.NewExecRunner())
		_ = a.headroomManager.Start(ctx)
		compressor = a.headroomManager
	}
	a.executor = executor.NewWithCompressor(
		a.classifier,
		a.policy,
		provider.NewRegistry(providers),
		conversation.New(a.store),
		compressor,
		debugLogger,
	)
	clientToken, _ := getenv(cfg.ClientAuth.TokenEnv)
	maxBodyBytes := cfg.ClientAuth.MaxBodyBytes
	if maxBodyBytes == 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}
	responsesConfig := api.ResponsesConfig{
		MaxBodyBytes: maxBodyBytes, Models: a.catalog, MinTier: a.minTier, MaxTier: a.maxTier, SafeFallbackTier: a.safeFallbackTier,
		ProviderCredentials: a.providerCredentials, ProviderAvailability: a.providerAvailability,
		ChatGPTOAuthProviders: chatGPTOAuthProviders, ClaudeOAuthProviders: claudeOAuthProviders,
	}
	responses := api.NewResponsesHandler(a.executor, responsesConfig)
	responses = api.CaptureClaudeOAuth(responses)
	responses = api.CaptureChatGPTOAuth(responses)
	mux := http.NewServeMux()
	operations := api.Operations(a.ready)
	mux.Handle("/healthz", operations)
	mux.Handle("/readyz", operations)
	mux.Handle("/v1/responses", api.Authenticate(responses, cfg.ClientAuth.HeaderName(), appTokens{{ID: "configured-client", Value: clientToken}}))
	if cfg.ClaudeMessages.Enabled {
		messages := api.CaptureNativeClaudeOAuth(api.NewMessagesHandler(a.executor, responsesConfig))
		mux.Handle("/v1/messages", api.AuthenticateWithError(messages, cfg.ClientAuth.HeaderName(), appTokens{{ID: "configured-client", Value: clientToken}}, api.WriteMessagesError))
	}
	a.handler = mux
	return a, nil
}

type appTokens []api.Token

func (tokens appTokens) Tokens() []api.Token { return tokens }

func configuredProviders(configs []config.ProviderConfig, getenv func(string) (string, bool), httpClient *http.Client) (map[string]provider.Provider, error) {
	entries := make(map[string]provider.Provider, len(configs))
	for _, cfg := range configs {
		switch cfg.ID {
		case "openai":
			if cfg.AuthMode() == config.ProviderAuthChatGPTOAuthPassthrough {
				entries[cfg.ID] = openai.NewChatGPTOAuthClient(cfg.BaseURL, httpClient)
			} else {
				key, _ := getenv(cfg.APIKeyEnv)
				entries[cfg.ID] = openai.NewClient(cfg.BaseURL, key, httpClient)
			}
		case "deepseek":
			key, _ := getenv(cfg.APIKeyEnv)
			entries[cfg.ID] = openai.NewClient(cfg.BaseURL, key, httpClient)
		case "meta":
			key, _ := getenv(cfg.APIKeyEnv)
			entries[cfg.ID] = openai.NewClient(cfg.BaseURL, key, httpClient)
		case "anthropic":
			if cfg.AuthMode() == config.ProviderAuthClaudeOAuthPassthrough {
				entries[cfg.ID] = anthropic.NewClaudeOAuthClient(cfg.BaseURL, httpClient)
			} else {
				key, _ := getenv(cfg.APIKeyEnv)
				entries[cfg.ID] = anthropic.NewClient(cfg.BaseURL, key, httpClient)
			}
		case "gemini":
			key, _ := getenv(cfg.APIKeyEnv)
			entries[cfg.ID] = gemini.NewClient(cfg.BaseURL, key, httpClient)
		default:
			return nil, errors.New("unsupported configured provider")
		}
	}
	return entries, nil
}

func policyConfig(cfg config.RoutingConfig) router.PolicyConfig {
	toSignalFloors := func(rules []config.SignalFloorConfig) []router.SignalFloor {
		mapped := make([]router.SignalFloor, len(rules))
		for index, rule := range rules {
			floor, _ := domain.ParseTier(rule.Floor)
			mapped[index] = router.SignalFloor{Threshold: rule.Threshold, Floor: floor}
		}
		return mapped
	}
	return router.PolicyConfig{
		FailureEscalationCost:    cfg.FailureEscalationCostUSD,
		LatencyPenaltyPerSecond:  cfg.LatencyPenaltyUSDPerSecond,
		MinSuccessProbability:    cfg.MinSuccessProbability,
		MaxDirectCost:            cfg.MaxDirectCostUSD,
		MaxExpectedCost:          cfg.MaxExpectedCostUSD,
		MaxLatency:               cfg.MaxLatency,
		MinClassifierConfidence:  cfg.MinClassifierConfidence,
		ReasoningFloors:          toSignalFloors(cfg.ReasoningFloors),
		CodingFloors:             toSignalFloors(cfg.CodingFloors),
		RiskFloors:               toSignalFloors(cfg.RiskFloors),
		UnderspecificationFloors: toSignalFloors(cfg.UnderspecificationFloors),
	}
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func (a *App) Handler() http.Handler { return a.handler }

func (a *App) ready(ctx context.Context) error {
	if err := a.store.Ready(ctx); err != nil {
		return err
	}
	if a.maintenance != nil {
		return a.maintenance.Ready()
	}
	return nil
}

// Close releases owned idle HTTP connections and SQLite exactly once, including
// when called concurrently. The HTTP server must drain requests first.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		if a.maintenance != nil {
			a.maintenance.Close()
		}
		if a.headroomManager != nil {
			a.closeErr = a.headroomManager.Close()
		}
		a.httpClient.CloseIdleConnections()
		a.closeErr = errors.Join(a.closeErr, a.store.Close())
		if a.debugTrace != nil {
			a.closeErr = errors.Join(a.closeErr, a.debugTrace.Close())
		}
	})
	return a.closeErr
}
