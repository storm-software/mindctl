// Package app owns the gateway's initialized dependencies and their lifecycle.
package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/storm-software/mindctl/internal/api"
	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/classifier/jev"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
)

type App struct {
	store                *sqlite.DB
	keyring              *contentcrypto.Keyring
	classifier           classifier.Classifier
	httpClient           *http.Client
	policy               *router.Policy
	catalog              []domain.Model
	minTier, maxTier     domain.Tier
	safeFallbackTier     domain.Tier
	providerCredentials  map[string]bool
	providerAvailability map[string]bool
	handler              http.Handler
	closeOnce            sync.Once
	closeErr             error
}

// New validates secret references again so callers need not use config.Load.
// Resolved values are never written back into the caller's configuration.
func New(ctx context.Context, cfg config.Config) (*App, error) {
	return newWithLookup(ctx, cfg, os.LookupEnv)
}

func newWithLookup(ctx context.Context, cfg config.Config, lookup func(string) (string, bool)) (*App, error) {
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
	if !validEndpoint(cfg.Jev.BaseURL) || cfg.Jev.Model == "" || cfg.Jev.Timeout < 0 || cfg.Jev.MaxRetries < 0 {
		return nil, errors.New("invalid Jev configuration")
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
		keyring: keyring, policy: router.NewPolicy(router.PolicyConfig{}),
		providerCredentials: make(map[string]bool), providerAvailability: make(map[string]bool),
	}
	a.minTier, _ = domain.ParseTier(cfg.Routing.MinTier)
	a.maxTier, _ = domain.ParseTier(cfg.Routing.MaxTier)
	fallback := cfg.Routing.SafeFallbackTier
	if fallback == "" {
		fallback = "T4"
	}
	a.safeFallbackTier, _ = domain.ParseTier(fallback)
	for _, provider := range cfg.Providers {
		value, exists := getenv(provider.APIKeyEnv)
		a.providerCredentials[provider.ID] = exists && value != ""
		a.providerAvailability[provider.ID] = validEndpoint(provider.BaseURL)
	}
	for index, model := range cfg.Models {
		tier, _ := domain.ParseTier(model.Tier)
		mapped := domain.Model{
			ID: model.ID, UpstreamID: model.ID, Provider: model.Provider, Tier: tier,
			ContextWindow: int64(model.ContextWindow), Available: model.Available, Order: index,
			Pricing:             domain.Pricing{InputPerMillion: model.InputPrice, CachedInputPerMillion: model.InputPrice, OutputPerMillion: model.OutputPrice},
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
	// Open owns cleanup on failure, including failed migrations. It is last so
	// there are no later fallible steps that could leak an initialized database.
	a.store, err = sqlite.Open(ctx, sqlite.Options{Path: cfg.SQLite.Path, Keyring: keyring})
	if err != nil {
		return nil, errors.New("initialize SQLite storage failed")
	}
	a.httpClient = &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second,
	}}
	jevKey, _ := getenv(cfg.Jev.APIKeyEnv)
	a.classifier = jev.NewClient(cfg.Jev, jevKey, a.httpClient)
	a.handler = api.Operations(a.store.Ready)
	return a, nil
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func (a *App) Handler() http.Handler { return a.handler }

// Close releases owned idle HTTP connections and SQLite exactly once, including
// when called concurrently. The HTTP server must drain requests first.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		a.httpClient.CloseIdleConnections()
		a.closeErr = a.store.Close()
	})
	return a.closeErr
}
