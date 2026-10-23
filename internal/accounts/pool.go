package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Selector chooses an account. Home mode and the local pool both implement it.
type Selector interface {
	Select(context.Context, SelectRequest) (Selection, error)
	ReportSuccess(context.Context, string, string)
	ReportFailure(context.Context, string, string, FailureClass)
	HasEligible(SelectRequest) bool
	ResetCooldown(string)
}

// Pool loads local auth files, keeps their secrets, and selects through a Conductor.
type Pool struct {
	mu        sync.Mutex
	dir       string
	mod       time.Time
	settings  Settings
	static    []Account
	secrets   map[string]Secret
	conductor *Conductor
}

// StaticAccount is a configured provider credential joined to the pool.
type StaticAccount struct {
	Account Account
	Secret  Secret
}

// NewPool seeds the pool with configured credentials and an auth directory.
// An empty directory is allowed. Missing directories contribute no files.
func NewPool(settings Settings, dir string, static []StaticAccount) (*Pool, error) {
	secrets := make(map[string]Secret, len(static))
	accounts := make([]Account, 0, len(static))
	for _, item := range static {
		if item.Account.ID == "" || item.Account.Provider == "" {
			return nil, errors.New("accounts: configured account needs an id and provider")
		}
		item.Account.Configured = true
		item.Account.Enabled = true
		if item.Account.Weight == 0 {
			item.Account.Weight = 1
		}
		accounts = append(accounts, item.Account)
		secrets[item.Account.ID] = item.Secret
	}
	pool := &Pool{
		dir:      dir,
		settings: settings.Normalize(),
		static:   accounts,
		secrets:  secrets,
	}
	if err := pool.reload(); err != nil {
		return nil, err
	}
	return pool, nil
}

// Select implements Selector.
func (p *Pool) Select(_ context.Context, req SelectRequest) (Selection, error) {
	if p == nil {
		return Selection{}, ErrNone
	}
	if err := p.reloadIfChanged(); err != nil {
		return Selection{}, err
	}
	account, err := p.conductor.Select(req)
	if err != nil {
		return Selection{}, err
	}
	p.mu.Lock()
	secret := p.secrets[account.ID]
	p.mu.Unlock()
	return Selection{Account: account, Secret: secret, Round: req.Round}, nil
}

// ReportSuccess implements Selector.
func (p *Pool) ReportSuccess(_ context.Context, sessionKey, accountID string) {
	if p == nil || p.conductor == nil {
		return
	}
	p.conductor.ReportSuccess(sessionKey, accountID)
}

// ReportFailure implements Selector.
func (p *Pool) ReportFailure(_ context.Context, sessionKey, accountID string, class FailureClass) {
	if p == nil || p.conductor == nil {
		return
	}
	p.conductor.ReportFailure(sessionKey, accountID, class)
}

// HasEligible implements Selector.
func (p *Pool) HasEligible(req SelectRequest) bool {
	if p == nil || p.conductor == nil {
		return false
	}
	_ = p.reloadIfChanged()
	return p.conductor.HasEligible(req)
}

// Cooldowns returns account cooldown deadlines without secrets.
func (p *Pool) Cooldowns() map[string]time.Time {
	if p == nil || p.conductor == nil {
		return map[string]time.Time{}
	}
	return p.conductor.Cooldowns()
}

// Lookup returns one account and its secret. The secret must stay out of logs.
func (p *Pool) Lookup(id string) (Account, Secret, bool) {
	if p == nil {
		return Account{}, Secret{}, false
	}
	if err := p.reloadIfChanged(); err != nil {
		return Account{}, Secret{}, false
	}
	p.mu.Lock()
	secret, ok := p.secrets[id]
	p.mu.Unlock()
	if !ok {
		return Account{}, Secret{}, false
	}
	for _, account := range p.conductor.Accounts() {
		if account.ID == id {
			return account, secret, true
		}
	}
	return Account{}, Secret{}, false
}

// ResetCooldown implements Selector.
func (p *Pool) ResetCooldown(id string) {
	if p == nil || p.conductor == nil {
		return
	}
	p.conductor.ResetCooldown(id)
}

// Accounts returns the current non-secret account records.
func (p *Pool) Accounts() []Account {
	if p == nil || p.conductor == nil {
		return []Account{}
	}
	_ = p.reloadIfChanged()
	return p.conductor.Accounts()
}

// SetEnabled writes the enabled flag back to an auth file.
// Configured provider credentials are not stored as files and cannot be toggled here.
func (p *Pool) SetEnabled(id string, enabled bool) error {
	if p == nil || p.dir == "" {
		return errors.New("accounts: auth directory is not configured")
	}
	path := filepath.Join(p.dir, filepath.Base(id)+".json")
	if filepath.Base(id) != id {
		return errors.New("accounts: invalid account id")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return err
	}
	document["enabled"] = enabled
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writePrivateFile(path, encoded)
}

func (p *Pool) reloadIfChanged() error {
	if p.dir == "" {
		return nil
	}
	info, err := os.Stat(p.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	p.mu.Lock()
	changed := info.ModTime().After(p.mod)
	p.mu.Unlock()
	if !changed {
		return nil
	}
	return p.reload()
}

func (p *Pool) reload() error {
	accounts := append([]Account(nil), p.static...)
	secrets := make(map[string]Secret, len(p.secrets))
	for id, secret := range p.secrets {
		secrets[id] = secret
	}
	var mod time.Time
	if p.dir != "" {
		entries, err := os.ReadDir(p.dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if info, err := os.Stat(p.dir); err == nil {
			mod = info.ModTime()
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			account, secret, err := readAuthFile(filepath.Join(p.dir, entry.Name()))
			if err != nil {
				return err
			}
			accounts = append(accounts, account)
			secrets[account.ID] = secret
		}
	}
	conductor := NewConductor(p.settings, accounts, nil)
	p.mu.Lock()
	// Keep cooldown and bindings across a file reload by copying them forward
	// only when a conductor already exists. The first load has neither.
	if p.conductor != nil {
		conductor = p.conductor
		conductor.ReplaceAccounts(accounts)
	}
	p.secrets = secrets
	p.conductor = conductor
	p.mod = mod
	p.mu.Unlock()
	return nil
}

type authFile struct {
	ID               string            `json:"id"`
	Provider         string            `json:"provider"`
	Protocol         string            `json:"protocol"`
	Kind             Kind              `json:"kind"`
	BaseURL          string            `json:"base_url"`
	Prefix           string            `json:"prefix"`
	ProxyURL         string            `json:"proxy_url"`
	Weight           *int              `json:"weight"`
	Priority         int               `json:"priority"`
	RequestRetry     *int              `json:"request_retry"`
	ExcludedModels   []string          `json:"excluded_models"`
	Enabled          *bool             `json:"enabled"`
	Disabled         bool              `json:"disabled"`
	ClaudeProfile    bool              `json:"claude_profile"`
	ThinkingReplay   bool              `json:"thinking_replay"`
	NativeToolSearch bool              `json:"native_tool_search"`
	APIKey           string            `json:"api_key"`
	AccessToken      string            `json:"access_token"`
	RefreshToken     string            `json:"refresh_token"`
	AccountID        string            `json:"account_id"`
	ClientEmail      string            `json:"client_email"`
	PrivateKey       string            `json:"private_key"`
	TokenURI         string            `json:"token_uri"`
	Headers          map[string]string `json:"headers"`
}

func readAuthFile(path string) (Account, Secret, error) {
	file, err := os.Open(path)
	if err != nil {
		return Account{}, Secret{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var decoded authFile
	if err := decoder.Decode(&decoded); err != nil {
		return Account{}, Secret{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Account{}, Secret{}, errors.New("accounts: auth file must be one JSON document")
	}
	fallback := decoded.ID
	if fallback == "" {
		fallback = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return accountFromFile(decoded, fallback)
}

// StoreDocument validates one auth-file JSON document and writes it privately.
func StoreDocument(dir string, body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var decoded authFile
	if err := decoder.Decode(&decoded); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("accounts: auth file must be one JSON document")
	}
	account, secret, err := accountFromFile(decoded, decoded.ID)
	if err != nil {
		return "", err
	}
	if account.ID == "" {
		return "", errors.New("accounts: auth file missing id")
	}
	return WriteAuthFile(dir, account, secret)
}

func accountFromFile(decoded authFile, fallbackID string) (Account, Secret, error) {
	id := decoded.ID
	if id == "" {
		id = fallbackID
	}
	if decoded.Provider == "" {
		return Account{}, Secret{}, errors.New("accounts: auth file missing provider")
	}
	weight := 1
	if decoded.Weight != nil {
		weight = *decoded.Weight
	}
	enabled := !decoded.Disabled
	if decoded.Enabled != nil {
		enabled = *decoded.Enabled && !decoded.Disabled
	}
	account := Account{
		ID: id, Provider: decoded.Provider, Protocol: decoded.Protocol, Kind: decoded.Kind,
		BaseURL: decoded.BaseURL, Prefix: decoded.Prefix, ProxyURL: decoded.ProxyURL,
		Weight: weight, Priority: decoded.Priority, ExcludedModels: append([]string(nil), decoded.ExcludedModels...),
		Enabled: enabled, ClaudeProfile: decoded.ClaudeProfile, ThinkingReplay: decoded.ThinkingReplay,
		NativeToolSearch: decoded.NativeToolSearch,
	}
	if decoded.RequestRetry != nil {
		account.RequestRetry = *decoded.RequestRetry
		account.RequestRetryConfigured = true
	}
	if account.Kind == "" {
		account.Kind = KindOAuth
	}
	secret := Secret{
		APIKey: decoded.APIKey, AccessToken: decoded.AccessToken, RefreshToken: decoded.RefreshToken,
		AccountID: decoded.AccountID, ClientEmail: decoded.ClientEmail, PrivateKey: decoded.PrivateKey,
		TokenURI: decoded.TokenURI, Headers: decoded.Headers,
	}
	return account, secret, nil
}

// WriteAuthFile stores one account and its secret as a private JSON file.
func WriteAuthFile(dir string, account Account, secret Secret) (string, error) {
	if account.ID == "" || strings.Contains(account.ID, "/") || account.ID == "." || account.ID == ".." {
		return "", errors.New("accounts: invalid account id")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	enabled := account.Enabled
	weight := account.Weight
	payload := authFile{
		ID: account.ID, Provider: account.Provider, Protocol: account.Protocol, Kind: account.Kind,
		BaseURL: account.BaseURL, Prefix: account.Prefix, ProxyURL: account.ProxyURL,
		Weight: &weight, Priority: account.Priority, ExcludedModels: account.ExcludedModels,
		Enabled: &enabled, ClaudeProfile: account.ClaudeProfile, ThinkingReplay: account.ThinkingReplay,
		NativeToolSearch: account.NativeToolSearch, APIKey: secret.APIKey, AccessToken: secret.AccessToken,
		RefreshToken: secret.RefreshToken, AccountID: secret.AccountID, ClientEmail: secret.ClientEmail,
		PrivateKey: secret.PrivateKey, TokenURI: secret.TokenURI, Headers: secret.Headers,
	}
	if account.RequestRetryConfigured {
		retry := account.RequestRetry
		payload.RequestRetry = &retry
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	body = append(body, '\n')
	path := filepath.Join(dir, account.ID+".json")
	if err := writePrivateFile(path, body); err != nil {
		return "", err
	}
	return path, nil
}

func writePrivateFile(path string, body []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
