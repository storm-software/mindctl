package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/storm-software/mindctl/internal/accounts"
)

// Options controls one local login. PublicBase, when set, means the existing
// gateway listener receives the callback and no extra port is opened.
// Provider overrides the built-in client, which tests use to point at a local token URL.
type Options struct {
	NoBrowser  bool
	Port       int
	AuthDir    string
	PublicBase string
	Registry   *Registry
	Provider   *Provider
	HTTP       *http.Client
	Stdout     io.Writer
	Opener     func(string) error
}

// Login runs one named flow and writes a private auth file.
func Login(ctx context.Context, name string, opts Options) (string, error) {
	provider, ok := Lookup(name)
	if opts.Provider != nil {
		provider, ok = *opts.Provider, true
	}
	if !ok {
		return "", fmt.Errorf("oauth: unknown login %s", name)
	}
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	if opts.Registry == nil {
		opts.Registry = NewRegistry()
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	var token accessToken
	var err error
	switch {
	case provider.Devin:
		token, err = devinLogin(ctx, provider, opts)
	case provider.Device || provider.Discovery:
		token, err = deviceLogin(ctx, provider, opts)
	default:
		token, err = codeLogin(ctx, provider, opts)
	}
	if err != nil {
		return "", err
	}
	return writeToken(opts.AuthDir, provider, token)
}

type accessToken struct {
	Access, Refresh, Account, UserCode string
}

func codeLogin(ctx context.Context, provider Provider, opts Options) (accessToken, error) {
	verifier, challenge, err := pkce()
	if err != nil {
		return accessToken{}, err
	}
	state, err := randomID()
	if err != nil {
		return accessToken{}, err
	}
	port := opts.Port
	if port == 0 {
		port = provider.DefaultPort
	}
	redirect := opts.PublicBase
	var listener net.Listener
	if redirect == "" {
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return accessToken{}, err
		}
		defer listener.Close()
		redirect = "http://" + listener.Addr().String()
		server := &http.Server{Handler: opts.Registry.Handler()}
		go func() { _ = server.Serve(listener) }()
		defer func() { _ = server.Close() }()
	}
	redirectURI := strings.TrimRight(redirect, "/") + provider.RedirectPath
	ready := opts.Registry.Begin(state)
	authURL := authorizeURL(provider, redirectURI, state, challenge)
	fmt.Fprintf(opts.Stdout, "open %s\n", authURL)
	if !opts.NoBrowser {
		opener := opts.Opener
		if opener == nil {
			opener = openBrowser
		}
		_ = opener(authURL)
	}
	code, err := opts.Registry.wait(ctx, state, ready)
	if err != nil {
		return accessToken{}, err
	}
	return exchangeCode(ctx, opts.HTTP, provider, redirectURI, verifier, code)
}

func deviceLogin(ctx context.Context, provider Provider, opts Options) (accessToken, error) {
	client := opts.HTTP
	authURL, tokenURL := provider.DeviceAuthURL, provider.DeviceTokenURL
	if provider.Discovery {
		var err error
		authURL, tokenURL, err = discoverDevice(ctx, client, provider.AuthURL)
		if err != nil {
			return accessToken{}, err
		}
	}
	body := map[string]string{"client_id": provider.ClientID}
	if provider.Name == "meta" || provider.Name == "kimi" || provider.Name == "kimi-ai" || provider.Discovery {
		form := url.Values{"client_id": {provider.ClientID}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
		if err != nil {
			return accessToken{}, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return pollDevice(ctx, client, opts.Stdout, tokenURL, provider, request)
	}
	encoded, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(string(encoded)))
	if err != nil {
		return accessToken{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	return pollDevice(ctx, client, opts.Stdout, tokenURL, provider, request)
}

func pollDevice(ctx context.Context, client *http.Client, stdout io.Writer, tokenURL string, provider Provider, request *http.Request) (accessToken, error) {
	response, err := client.Do(request)
	if err != nil {
		return accessToken{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return accessToken{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return accessToken{}, fmt.Errorf("oauth: device authorization returned %d", response.StatusCode)
	}
	var started struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		DeviceCode   string `json:"device_code"`
		Verification string `json:"verification_uri"`
		Complete     string `json:"verification_uri_complete"`
		Interval     int    `json:"interval"`
	}
	if err := json.Unmarshal(payload, &started); err != nil {
		return accessToken{}, err
	}
	verification := started.Complete
	if verification == "" {
		verification = started.Verification
	}
	if verification == "" {
		verification = provider.AuthURL
	}
	fmt.Fprintf(stdout, "open %s\ncode %s\n", verification, firstNonEmpty(started.UserCode, started.DeviceCode))
	interval := time.Duration(started.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return accessToken{}, ctx.Err()
		case <-time.After(interval):
		}
		token, pending, err := pollToken(ctx, client, tokenURL, provider, started.DeviceAuthID, started.UserCode, started.DeviceCode)
		if err != nil {
			return accessToken{}, err
		}
		if !pending {
			return token, nil
		}
	}
}

func pollToken(ctx context.Context, client *http.Client, tokenURL string, provider Provider, deviceAuthID, userCode, deviceCode string) (accessToken, bool, error) {
	var request *http.Request
	var err error
	if provider.Name == "codex-device" {
		body, _ := json.Marshal(map[string]string{"device_auth_id": deviceAuthID, "user_code": userCode})
		request, err = http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(body)))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
		}
	} else {
		form := url.Values{
			"client_id": {provider.ClientID}, "device_code": {deviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err == nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return accessToken{}, false, err
	}
	response, err := client.Do(request)
	if err != nil {
		return accessToken{}, false, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return accessToken{}, false, err
	}
	if response.StatusCode == http.StatusPreconditionFailed || response.StatusCode == http.StatusTooEarly {
		return accessToken{}, true, nil
	}
	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(payload, &decoded)
	if decoded.Error == "authorization_pending" || decoded.Error == "slow_down" {
		return accessToken{}, true, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || decoded.AccessToken == "" {
		return accessToken{}, false, fmt.Errorf("oauth: device token returned %d", response.StatusCode)
	}
	return accessToken{Access: decoded.AccessToken, Refresh: decoded.RefreshToken, Account: decoded.AccountID}, false, nil
}

func devinLogin(ctx context.Context, provider Provider, opts Options) (accessToken, error) {
	verifier, challenge, err := pkce()
	if err != nil {
		return accessToken{}, err
	}
	state, err := randomID()
	if err != nil {
		return accessToken{}, err
	}
	port := opts.Port
	if port == 0 {
		port = 53682
	}
	redirect := opts.PublicBase
	if redirect == "" {
		listener, listenErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if listenErr != nil {
			return accessToken{}, listenErr
		}
		defer listener.Close()
		redirect = "http://" + listener.Addr().String()
		server := &http.Server{Handler: opts.Registry.Handler()}
		go func() { _ = server.Serve(listener) }()
		defer func() { _ = server.Close() }()
	}
	redirectURI := strings.TrimRight(redirect, "/") + "/callback"
	ready := opts.Registry.Begin(state)
	authURL := provider.AuthURL + "?code_challenge=" + url.QueryEscape(challenge) +
		"&code_challenge_method=S256&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&state=" + url.QueryEscape(state)
	fmt.Fprintf(opts.Stdout, "open %s\n", authURL)
	if !opts.NoBrowser {
		opener := opts.Opener
		if opener == nil {
			opener = openBrowser
		}
		_ = opener(authURL)
	}
	code, err := opts.Registry.wait(ctx, state, ready)
	if err != nil {
		return accessToken{}, err
	}
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenURL, strings.NewReader(string(body)))
	if err != nil {
		return accessToken{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := opts.HTTP.Do(request)
	if err != nil {
		return accessToken{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return accessToken{}, err
	}
	var decoded struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Token == "" || response.StatusCode >= 300 {
		return accessToken{}, errors.New("oauth: devin token exchange failed")
	}
	return accessToken{Access: decoded.Token}, nil
}

func exchangeCode(ctx context.Context, client *http.Client, provider Provider, redirectURI, verifier, code string) (accessToken, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {provider.ClientID}, "code_verifier": {verifier},
	}
	if provider.Resource != "" {
		form.Set("resource", provider.Resource)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return accessToken{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return accessToken{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return accessToken{}, err
	}
	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.AccessToken == "" || response.StatusCode >= 300 {
		return accessToken{}, errors.New("oauth: token exchange failed")
	}
	return accessToken{Access: decoded.AccessToken, Refresh: decoded.RefreshToken, Account: decoded.AccountID}, nil
}

func authorizeURL(provider Provider, redirectURI, state, challenge string) string {
	values := url.Values{
		"client_id": {provider.ClientID}, "redirect_uri": {redirectURI},
		"response_type": {"code"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"state": {state},
	}
	if len(provider.Scopes) > 0 {
		values.Set("scope", strings.Join(provider.Scopes, " "))
	}
	if provider.Resource != "" {
		values.Set("resource", provider.Resource)
	}
	for _, extra := range provider.ExtraAuth {
		values.Set(extra[0], extra[1])
	}
	return provider.AuthURL + "?" + values.Encode()
}

func discoverDevice(ctx context.Context, client *http.Client, issuer string) (string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	var decoded struct {
		Device string `json:"device_authorization_endpoint"`
		Token  string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil {
		return "", "", err
	}
	if decoded.Device == "" || decoded.Token == "" {
		return "", "", errors.New("oauth: discovery document has no device flow")
	}
	return decoded.Device, decoded.Token, nil
}

func writeToken(dir string, provider Provider, token accessToken) (string, error) {
	if dir == "" {
		return "", errors.New("oauth: auth directory is required")
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	accountID := provider.AccountProvider + "-" + id
	return accounts.WriteAuthFile(dir, accounts.Account{
		ID: accountID, Provider: provider.AccountProvider, Protocol: provider.Protocol,
		Kind: accounts.KindOAuth, BaseURL: provider.APIBase, Enabled: true, Weight: 1,
	}, accounts.Secret{AccessToken: token.Access, RefreshToken: token.Refresh, AccountID: token.Account})
}

func pkce() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func openBrowser(raw string) error {
	return exec.Command("xdg-open", raw).Start()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Refresh exchanges a refresh token for a new access token.
func Refresh(ctx context.Context, client *http.Client, name, refreshToken string) (string, string, error) {
	provider, ok := Lookup(name)
	if !ok {
		return "", "", fmt.Errorf("oauth: unknown login %s", name)
	}
	if provider.TokenURL == "" || refreshToken == "" {
		return "", "", errors.New("oauth: account cannot be refreshed")
	}
	if client == nil {
		client = http.DefaultClient
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {provider.ClientID},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil || decoded.AccessToken == "" || response.StatusCode >= 300 {
		return "", "", errors.New("oauth: refresh failed")
	}
	if decoded.RefreshToken == "" {
		decoded.RefreshToken = refreshToken
	}
	return decoded.AccessToken, decoded.RefreshToken, nil
}
