package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/debugtrace"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/oauth"
	"github.com/storm-software/mindctl/internal/pluginhost"
	"github.com/storm-software/mindctl/internal/storage"
	"gopkg.in/yaml.v3"
)

// Management is the authenticated control API. Dashboard is its HTML client.
type Management struct {
	ConfigPath string
	AuthDir    string
	Config     config.Config
	Pool       *accounts.Pool
	Plugins    *pluginhost.Host
	Models     []domain.Model
	Usage      func(context.Context, storage.UsageFilter) (storage.UsageReport, error)

	mu     sync.Mutex
	logins map[string]*loginSession
}

type loginSession struct {
	ID, Provider, URL, Error, Path string
	Done                           bool
}

// Handler returns the /v8/management/ subtree.
func (m *Management) Handler() http.Handler {
	if m.logins == nil {
		m.logins = map[string]*loginSession{}
	}
	return http.HandlerFunc(m.serve)
}

func (m *Management) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v8/management"), "/")
	parts := []string{}
	if path != "" {
		parts = strings.Split(path, "/")
	}
	switch {
	case path == "config" && r.Method == http.MethodGet:
		m.readConfig(w, r)
	case path == "config" && r.Method == http.MethodPut:
		m.writeConfig(w, r)
	case path == "credentials" && r.Method == http.MethodGet:
		m.listCredentials(w, r)
	case path == "credentials" && r.Method == http.MethodPost:
		m.uploadCredential(w, r)
	case len(parts) == 3 && parts[0] == "credentials" && parts[2] == "refresh" && r.Method == http.MethodPost:
		m.refreshCredential(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "credentials" && parts[2] == "enable" && r.Method == http.MethodPost:
		m.setCredential(w, parts[1], true)
	case len(parts) == 3 && parts[0] == "credentials" && parts[2] == "disable" && r.Method == http.MethodPost:
		m.setCredential(w, parts[1], false)
	case path == "oauth/start" && r.Method == http.MethodPost:
		m.startOAuth(w, r)
	case path == "oauth/status" && r.Method == http.MethodGet:
		m.oauthStatus(w, r)
	case path == "logs" && r.Method == http.MethodGet:
		m.listLogs(w, r)
	case len(parts) == 2 && parts[0] == "logs" && r.Method == http.MethodGet:
		m.readLog(w, r, parts[1])
	case path == "cooldown/reset" && r.Method == http.MethodPost:
		m.resetCooldown(w, r)
	case path == "models" && r.Method == http.MethodGet:
		m.listModels(w, r)
	case path == "plugins" && r.Method == http.MethodGet:
		m.listPlugins(w, r)
	case path == "plugins/install" && r.Method == http.MethodPost:
		m.installPlugin(w, r)
	case path == "quota" && r.Method == http.MethodGet:
		m.quota(w, r)
	case path == "usage" && r.Method == http.MethodGet:
		m.usage(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *Management) readConfig(w http.ResponseWriter, _ *http.Request) {
	encoded, err := yaml.Marshal(m.Config)
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(encoded)
}

func (m *Management) writeConfig(w http.ResponseWriter, r *http.Request) {
	if m.ConfigPath == "" {
		WriteError(w, errManagement("config path is not set"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		WriteError(w, err)
		return
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var cfg config.Config
	if err := decoder.Decode(&cfg); err != nil {
		WriteError(w, err)
		return
	}
	if err := cfg.Validate(os.LookupEnv); err != nil {
		WriteError(w, err)
		return
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		WriteError(w, err)
		return
	}
	if err := os.WriteFile(m.ConfigPath, encoded, 0o600); err != nil {
		WriteError(w, err)
		return
	}
	m.Config = cfg
	w.WriteHeader(http.StatusNoContent)
}

func (m *Management) listCredentials(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID, Provider, Kind, Protocol string
		Enabled                      bool
	}
	items := []item{}
	if m.Pool != nil {
		for _, account := range m.Pool.Accounts() {
			items = append(items, item{
				ID: account.ID, Provider: account.Provider, Kind: string(account.Kind),
				Protocol: account.Protocol, Enabled: account.Enabled,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": items})
}

func (m *Management) uploadCredential(w http.ResponseWriter, r *http.Request) {
	if m.AuthDir == "" {
		WriteError(w, errManagement("auth directory is not set"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		WriteError(w, err)
		return
	}
	path, err := accounts.StoreDocument(m.AuthDir, body)
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": filepath.Base(path)})
}

func (m *Management) setCredential(w http.ResponseWriter, id string, enabled bool) {
	if m.Pool == nil {
		WriteError(w, errManagement("account pool is unavailable"))
		return
	}
	if err := m.Pool.SetEnabled(id, enabled); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Management) refreshCredential(w http.ResponseWriter, r *http.Request, id string) {
	if m.Pool == nil || m.AuthDir == "" {
		WriteError(w, errManagement("account pool is unavailable"))
		return
	}
	account, secret, ok := m.Pool.Lookup(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if account.Kind == accounts.KindServiceAccount {
		token, err := oauth.GoogleAccessToken(r.Context(), oauth.ServiceAccount{
			ClientEmail: secret.ClientEmail, PrivateKey: secret.PrivateKey, TokenURI: secret.TokenURI,
		})
		if err != nil {
			WriteError(w, err)
			return
		}
		secret.AccessToken = token
	} else {
		access, refresh, err := oauth.Refresh(r.Context(), nil, refreshName(account.Provider), secret.RefreshToken)
		if err != nil {
			WriteError(w, err)
			return
		}
		secret.AccessToken = access
		secret.RefreshToken = refresh
	}
	if _, err := accounts.WriteAuthFile(m.AuthDir, account, secret); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Management) startOAuth(w http.ResponseWriter, r *http.Request) {
	var decoded struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&decoded); err != nil {
		WriteError(w, err)
		return
	}
	if _, ok := oauth.Lookup(decoded.Provider); !ok {
		WriteError(w, errManagement("unknown oauth provider"))
		return
	}
	id := randomToken()
	session := &loginSession{ID: id, Provider: decoded.Provider}
	m.mu.Lock()
	if m.logins == nil {
		m.logins = map[string]*loginSession{}
	}
	m.logins[id] = session
	m.mu.Unlock()
	go m.runLogin(id, decoded.Provider, publicBase(r))
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (m *Management) runLogin(id, provider, base string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	path, err := oauth.Login(ctx, provider, oauth.Options{
		NoBrowser: true, PublicBase: base, Registry: oauth.Callbacks, AuthDir: m.AuthDir,
		Stdout: writerFunc(func(chunk []byte) {
			m.mu.Lock()
			if session, ok := m.logins[id]; ok {
				session.URL += string(chunk)
			}
			m.mu.Unlock()
		}),
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.logins[id]
	if !ok {
		return
	}
	session.Done = true
	session.Path = filepath.Base(path)
	if err != nil {
		session.Error = err.Error()
	}
}

func (m *Management) oauthStatus(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	session := m.logins[r.URL.Query().Get("id")]
	var copy loginSession
	if session != nil {
		copy = *session
	}
	m.mu.Unlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, copy)
}

func (m *Management) listLogs(w http.ResponseWriter, _ *http.Request) {
	directory, err := debugtrace.LogDirectory()
	if err != nil {
		WriteError(w, err)
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil && !os.IsNotExist(err) {
		WriteError(w, err)
		return
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		names = append(names, entry.Name())
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": names})
}

func (m *Management) readLog(w http.ResponseWriter, r *http.Request, name string) {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".jsonl") {
		http.NotFound(w, r)
		return
	}
	directory, err := debugtrace.LogDirectory()
	if err != nil {
		WriteError(w, err)
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write(body)
}

func (m *Management) resetCooldown(w http.ResponseWriter, r *http.Request) {
	if m.Pool == nil {
		WriteError(w, errManagement("account pool is unavailable"))
		return
	}
	var decoded struct {
		AccountID string `json:"account_id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&decoded)
	}
	m.Pool.ResetCooldown(decoded.AccountID)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Management) listModels(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID, Provider, Tier string
	}
	items := make([]item, 0, len(m.Models))
	for _, model := range m.Models {
		items = append(items, item{ID: model.ID, Provider: model.Provider, Tier: model.Tier.String()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": items})
}

func (m *Management) listPlugins(w http.ResponseWriter, _ *http.Request) {
	items := []pluginhost.Info{}
	if m.Plugins != nil {
		items = m.Plugins.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": items, "enabled": m.Plugins != nil && m.Plugins.Enabled()})
}

func (m *Management) installPlugin(w http.ResponseWriter, r *http.Request) {
	if m.Plugins == nil || !m.Plugins.Enabled() {
		WriteError(w, errManagement("plugins are disabled"))
		return
	}
	var decoded struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&decoded); err != nil {
		WriteError(w, err)
		return
	}
	if err := m.Plugins.Load(decoded.Path); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Management) quota(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID, Provider string
		Cooling      bool
		Until        string
	}
	items := []item{}
	cooldowns := map[string]time.Time{}
	if m.Pool != nil {
		cooldowns = m.Pool.Cooldowns()
		for _, account := range m.Pool.Accounts() {
			until := cooldowns[account.ID]
			entry := item{ID: account.ID, Provider: account.Provider}
			if until.After(time.Now()) {
				entry.Cooling = true
				entry.Until = until.UTC().Format(time.RFC3339)
			}
			items = append(items, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": items})
}

// usage reports attempts in the last range, bucketed by bucket and aligned to
// the viewer's UTC offset in seconds east.
func (m *Management) usage(w http.ResponseWriter, r *http.Request) {
	if m.Usage == nil {
		WriteError(w, errManagement("usage storage is unavailable"))
		return
	}
	query := r.URL.Query()
	window, err := time.ParseDuration(query.Get("range"))
	if err != nil || window <= 0 || window > 366*24*time.Hour {
		WriteError(w, inference.Invalid("range", "must be a positive duration of at most 366 days"))
		return
	}
	bucket, err := time.ParseDuration(query.Get("bucket"))
	if err != nil || bucket < time.Minute || window/bucket > 2000 {
		WriteError(w, inference.Invalid("bucket", "must be at least 1m and split range into at most 2000 buckets"))
		return
	}
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < -14*3600 || offset > 14*3600 {
			WriteError(w, inference.Invalid("offset", "must be a UTC offset in seconds"))
			return
		}
	}
	report, err := m.Usage(r.Context(), storage.UsageFilter{
		Since: time.Now().Add(-window), Bucket: bucket, Offset: time.Duration(offset) * time.Second, Recent: 100,
	})
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func refreshName(providerID string) string {
	switch providerID {
	case "openai":
		return "codex"
	case "anthropic":
		return "claude"
	default:
		return providerID
	}
}

func publicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func randomToken() string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

type managementError string

func (e managementError) Error() string { return string(e) }

func errManagement(message string) error { return managementError(message) }

type writerFunc func([]byte)

func (w writerFunc) Write(chunk []byte) (int, error) {
	w(chunk)
	return len(chunk), nil
}
