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

	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/api"
	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/classifier/laya"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/debugtrace"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/forward"
	"github.com/storm-software/mindctl/internal/headroom"
	"github.com/storm-software/mindctl/internal/home"
	"github.com/storm-software/mindctl/internal/oauth"
	"github.com/storm-software/mindctl/internal/pluginhost"
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
	gatewayClaudeToken, gatewayCodexAuthFile := "", ""
	for _, providerConfig := range cfg.Providers {
		switch providerConfig.AuthMode() {
		case config.ProviderAuthAPIKey:
			value, exists := getenv(providerConfig.APIKeyEnv)
			a.providerCredentials[providerConfig.ID] = exists && value != ""
		case config.ProviderAuthChatGPTOAuthPassthrough:
			a.providerCredentials[providerConfig.ID] = true
			chatGPTOAuthProviders[providerConfig.ID] = true
			gatewayCodexAuthFile = providerConfig.CredentialFile
		case config.ProviderAuthClaudeOAuthPassthrough:
			a.providerCredentials[providerConfig.ID] = true
			claudeOAuthProviders[providerConfig.ID] = true
			if providerConfig.CredentialEnv != "" {
				gatewayClaudeToken, _ = getenv(providerConfig.CredentialEnv)
			}
		case config.ProviderAuthOAuth:
			value, exists := getenv(providerConfig.APIKeyEnv)
			a.providerCredentials[providerConfig.ID] = providerConfig.APIKeyEnv == "" || (exists && value != "")
		}
		a.providerAvailability[providerConfig.ID] = validEndpoint(providerConfig.BaseURL)
	}
	for index, model := range cfg.Models {
		tier, _ := domain.ParseTier(model.Tier)
		mapped := domain.Model{
			ID: model.ID, UpstreamID: model.ID, Provider: model.Provider, Tier: tier,
			ContextWindow: int64(model.ContextWindow), MaxOutputTokens: int64(model.MaxOutputTokens),
			LatencyP95: model.LatencyP95, Available: model.Available, ExplicitOnly: model.ExplicitOnly, Order: index,
			Pricing:             domain.Pricing{InputPerMillion: model.InputPrice, CachedInputPerMillion: model.CachedInputPrice(), CacheWriteInputPerMillion: model.CacheWriteInputPrice(), OutputPerMillion: model.OutputPrice, PerRequestUSD: model.PerRequestPriceUSD},
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
	var sessionIdleTTL time.Duration
	if cfg.Routing.Session.EnabledValue() {
		sessionIdleTTL = cfg.Routing.Session.IdleTTLValue()
	}
	a.maintenance = newRetentionMaintenance(cfg.SQLite.Retention, sessionIdleTTL, cfg.SQLite.RetentionMaintenanceInterval,
		a.store.DeleteExpiredContent, a.store.DeleteExpiredSessions, maintenanceOptions)
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
		// A failed start is sticky and surfaces per request as
		// headroom_unavailable; the trace keeps the gateway-authored reason.
		if err := a.headroomManager.Start(ctx); err != nil && debugLogger != nil {
			debugLogger.Debug("headroom.start.failed", "error", err.Error())
		}
		compressor = a.headroomManager
	}
	pool, err := accountPool(cfg, getenv)
	if err != nil {
		a.httpClient.CloseIdleConnections()
		_ = a.store.Close()
		return nil, err
	}
	selector := accounts.Selector(pool)
	if cfg.Home.JWT != "" {
		delegated, homeErr := home.Connect(ctx, cfg.Home.JWT, cfg.Home.DisableClusterDiscovery, a.httpClient)
		if homeErr != nil {
			a.httpClient.CloseIdleConnections()
			_ = a.store.Close()
			return nil, homeErr
		}
		selector = delegated
	}
	plugins := pluginhost.New(cfg.Plugins.Enabled)
	if cfg.Plugins.Enabled && cfg.Plugins.Path != "" {
		if err := plugins.Load(cfg.Plugins.Path); err != nil {
			a.httpClient.CloseIdleConnections()
			_ = a.store.Close()
			return nil, err
		}
	}
	aliases := make([]accounts.Alias, 0, len(cfg.Routing.Aliases))
	for _, alias := range cfg.Routing.Aliases {
		aliases = append(aliases, accounts.Alias{Name: alias.Name, Alias: alias.Alias, Provider: alias.Provider})
	}
	a.executor = executor.NewWithCompressor(
		a.classifier,
		a.policy,
		provider.NewRegistry(providers),
		conversation.NewWithOptions(a.store, conversation.Options{SessionIdleTTL: cfg.Routing.Session.IdleTTLValue()}),
		compressor,
		debugLogger,
	)
	a.executor.SetAccountRouting(selector, aliases)
	a.executor.SetProtocolClients(protocolFactory(a.httpClient))
	clientToken, _ := getenv(cfg.ClientAuth.TokenEnv)
	maxBodyBytes := cfg.ClientAuth.MaxBodyBytes
	if maxBodyBytes == 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}
	responsesConfig := api.ResponsesConfig{
		MaxBodyBytes: maxBodyBytes, Models: a.catalog, MinTier: a.minTier, MaxTier: a.maxTier, SafeFallbackTier: a.safeFallbackTier,
		ProviderCredentials: a.providerCredentials, ProviderAvailability: a.providerAvailability,
		ChatGPTOAuthProviders: chatGPTOAuthProviders, ClaudeOAuthProviders: claudeOAuthProviders,
		SavingsBaseline:      cfg.Savings.BaselineModel,
		SessionEnabled:       cfg.Routing.Session.EnabledValue(),
		ExpectedOutputTokens: cfg.Routing.ExpectedOutputTokens,
		Session: executor.SessionSettings{
			HorizonTurns:         cfg.Routing.Session.HorizonTurnsValue(),
			DefaultCacheHitRatio: cfg.Routing.Session.DefaultCacheHitRatioValue(),
		},
		Logger: debugLogger,
	}
	captureBodies := func(next http.Handler) http.Handler {
		if a.debugTrace == nil || !cfg.DebugBodies {
			return next
		}
		return api.CaptureFailedBodies(next, maxBodyBytes, a.debugTrace.SaveBody, debugLogger)
	}
	responses := api.NewResponsesHandler(a.executor, responsesConfig)
	responses = api.UseGatewayCredentials(responses, gatewayClaudeToken, gatewayCodexAuthFile)
	responses = api.CaptureClaudeOAuth(responses)
	responses = api.CaptureChatGPTOAuth(responses)
	responses = captureBodies(responses)
	mux := http.NewServeMux()
	operations := api.Operations(a.ready)
	mux.Handle("/healthz", operations)
	mux.Handle("/readyz", operations)
	named := &api.Named{
		Executor: a.executor,
		Forward: &forward.Forwarder{
			Selector: selector, Aliases: aliases, Models: append([]domain.Model(nil), a.catalog...), HTTP: a.httpClient,
		},
		Config: responsesConfig, Aliases: aliases, Models: append([]domain.Model(nil), a.catalog...),
	}
	clientAuth := api.Authenticate(named.Responses(responses), cfg.ClientAuth.HeaderName(), appTokens{{ID: "configured-client", Value: clientToken}})
	mux.Handle("/v1/responses", clientAuth)
	// Cursor's OpenAI override can only send Authorization. Keep that check on
	// this route so the configured header can stay a separate gateway token
	// for Claude and Codex.
	chat := captureBodies(api.NewChatHandler(a.executor, responsesConfig))
	mux.Handle("/v1/chat/completions", api.Authenticate(chat, api.ChatAuthorizationHeader, appTokens{{ID: "configured-client", Value: clientToken}}))
	wrapClient := func(handler http.Handler) http.Handler {
		return api.Authenticate(handler, cfg.ClientAuth.HeaderName(), appTokens{{ID: "configured-client", Value: clientToken}})
	}
	named.Mount(mux, wrapClient)
	mux.Handle("/auth/callback", oauth.Callbacks.Handler())
	mux.Handle("/callback", oauth.Callbacks.Handler())
	if cfg.Management.Enabled {
		managementToken, _ := getenv(cfg.Management.TokenEnv)
		management := &api.Management{
			ConfigPath: cfg.Path, AuthDir: cfg.OAuth.AuthDir, Config: cfg,
			Pool: pool, Plugins: plugins, Models: append([]domain.Model(nil), a.catalog...),
		}
		mux.Handle("/v8/management/", api.Authenticate(management.Handler(), "Authorization", appTokens{{ID: "management", Value: managementToken}}))
	}
	if cfg.ClaudeMessages.Enabled {
		messages := api.UseGatewayCredentials(api.NewMessagesHandler(a.executor, responsesConfig), gatewayClaudeToken, gatewayCodexAuthFile)
		messages = captureBodies(api.CaptureNativeClaudeOAuth(messages))
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
		key, _ := getenv(cfg.APIKeyEnv)
		client, err := providerClient(cfg, key, httpClient)
		if err != nil {
			return nil, err
		}
		entries[cfg.ID] = client
	}
	return entries, nil
}

func providerClient(cfg config.ProviderConfig, key string, httpClient *http.Client) (provider.Provider, error) {
	protocol := cfg.Protocol
	if protocol == "" {
		switch cfg.ID {
		case "anthropic":
			protocol = "anthropic"
		case "gemini":
			protocol = "gemini"
		case "openai", "deepseek", "meta":
			protocol = "openai"
		default:
			return nil, errors.New("unsupported configured provider")
		}
	}
	switch accounts.ProtocolFamily(protocol) {
	case "anthropic":
		if cfg.AuthMode() == config.ProviderAuthClaudeOAuthPassthrough {
			return anthropic.NewClaudeOAuthClient(cfg.BaseURL, httpClient), nil
		}
		return anthropic.NewClient(cfg.BaseURL, key, httpClient), nil
	case "gemini":
		return gemini.NewClient(cfg.BaseURL, key, httpClient), nil
	case "openai":
		if cfg.AuthMode() == config.ProviderAuthChatGPTOAuthPassthrough {
			return openai.NewChatGPTOAuthClient(cfg.BaseURL, httpClient), nil
		}
		return openai.NewClient(cfg.BaseURL, key, httpClient), nil
	default:
		return nil, errors.New("unsupported configured provider")
	}
}

func protocolFactory(httpClient *http.Client) func(string) (provider.Provider, bool) {
	return func(protocol string) (provider.Provider, bool) {
		switch accounts.ProtocolFamily(protocol) {
		case "anthropic":
			return anthropic.NewClient("", "", httpClient), true
		case "gemini":
			return gemini.NewClient("", "", httpClient), true
		case "openai":
			return openai.NewClient("", "", httpClient), true
		default:
			return nil, false
		}
	}
}

func accountPool(cfg config.Config, getenv func(string) (string, bool)) (*accounts.Pool, error) {
	static := make([]accounts.StaticAccount, 0, len(cfg.Providers))
	for _, providerConfig := range cfg.Providers {
		if account, ok := staticAccount(providerConfig, getenv); ok {
			static = append(static, account)
		}
	}
	return accounts.NewPool(accountSettings(cfg.Routing.Accounts), cfg.OAuth.AuthDir, static)
}

func staticAccount(cfg config.ProviderConfig, getenv func(string) (string, bool)) (accounts.StaticAccount, bool) {
	key, _ := getenv(cfg.APIKeyEnv)
	kind := accounts.KindAPIKey
	secret := accounts.Secret{}
	switch cfg.AuthMode() {
	case config.ProviderAuthChatGPTOAuthPassthrough, config.ProviderAuthClaudeOAuthPassthrough:
		kind = accounts.KindPassthrough
	case config.ProviderAuthOAuth:
		kind = accounts.KindOAuth
		if key == "" {
			return accounts.StaticAccount{}, false
		}
		secret.APIKey = key
	default:
		if key == "" {
			return accounts.StaticAccount{}, false
		}
		secret.APIKey = key
	}
	protocol := cfg.Protocol
	if protocol == "" {
		protocol = nativeProviderProtocol(cfg.ID)
	}
	return accounts.StaticAccount{Account: accounts.Account{
		ID: "config:" + cfg.ID, Provider: cfg.ID, Protocol: protocol, Kind: kind,
		BaseURL: cfg.BaseURL, Prefix: cfg.Prefix, Enabled: true, Weight: 1,
	}, Secret: secret}, true
}

func nativeProviderProtocol(providerID string) string {
	switch providerID {
	case "anthropic":
		return "anthropic"
	case "gemini":
		return "gemini"
	default:
		return "openai"
	}
}

func accountSettings(cfg config.AccountRoutingConfig) accounts.Settings {
	settings := accounts.Settings{
		Strategy: accounts.Strategy(cfg.Strategy), SessionAffinity: cfg.SessionAffinity,
		SessionAffinityTTL: cfg.SessionAffinityTTL, RebindOn: accounts.RebindMode(cfg.RebindOn),
		MaxRetryCredentials: cfg.MaxRetryCredentials, MaxRetryInterval: cfg.MaxRetryInterval,
		DisableCooling: cfg.DisableCooling, ForceModelPrefix: cfg.ForceModelPrefix,
	}
	if cfg.RequestRetry != nil {
		settings.RequestRetry = *cfg.RequestRetry
		settings.RequestRetryConfigured = true
	}
	if cfg.Subagents != nil {
		settings.Subagents = *cfg.Subagents
		settings.SubagentsConfigured = true
	}
	return settings
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
