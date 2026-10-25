package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// idpClientPrefix keeps external subjects apart from static client IDs in
// conversation storage.
const idpClientPrefix = "idp:"

const introspectionCacheLimit = 4096

var idpHTTP = &http.Client{Timeout: 10 * time.Second}

// Introspector accepts credentials an RFC 7662 endpoint reports active. Each
// active answer is reused for TTL, or until the credential expires if sooner.
type Introspector struct {
	URL, ClientID, ClientSecret, Audience string
	TTL                                   time.Duration
	HTTP                                  *http.Client
	Logger                                *slog.Logger

	mu    sync.Mutex
	cache map[[sha256.Size]byte]introspected
}

type introspected struct {
	clientID string
	until    time.Time
}

func (i *Introspector) Verify(ctx context.Context, presented string) (string, error) {
	key := sha256.Sum256([]byte(presented))
	now := time.Now()
	i.mu.Lock()
	hit, ok := i.cache[key]
	i.mu.Unlock()
	if ok && now.Before(hit.until) {
		return hit.clientID, nil
	}
	answer, err := i.introspect(ctx, presented)
	if err != nil {
		if i.Logger != nil {
			i.Logger.Debug("auth.introspection.failed", "error", err.Error())
		}
		return "", fmt.Errorf("%w: %w", ErrAuthUnavailable, err)
	}
	expires := time.Unix(answer.Exp, 0)
	if !answer.Active || answer.Sub == "" || (answer.Exp != 0 && !now.Before(expires)) ||
		(i.Audience != "" && !slices.Contains(answer.Aud, i.Audience)) {
		return "", ErrUnauthorized
	}
	until := now.Add(i.TTL)
	if answer.Exp != 0 && expires.Before(until) {
		until = expires
	}
	clientID := idpClientPrefix + answer.Sub
	i.mu.Lock()
	// ponytail: forgets every answer at the limit; use an LRU if more distinct
	// live credentials than this are routine.
	if i.cache == nil || len(i.cache) >= introspectionCacheLimit {
		i.cache = map[[sha256.Size]byte]introspected{}
	}
	i.cache[key] = introspected{clientID: clientID, until: until}
	i.mu.Unlock()
	return clientID, nil
}

type introspection struct {
	Active bool     `json:"active"`
	Sub    string   `json:"sub"`
	Exp    int64    `json:"exp"`
	Aud    audience `json:"aud"`
}

func (i *Introspector) introspect(ctx context.Context, presented string) (introspection, error) {
	var answer introspection
	body := strings.NewReader(url.Values{"token": {presented}}.Encode())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, i.URL, body)
	if err != nil {
		return answer, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	// RFC 6749 section 2.3.1 form-encodes Basic client credentials.
	request.SetBasicAuth(url.QueryEscape(i.ClientID), url.QueryEscape(i.ClientSecret))
	response, err := httpClient(i.HTTP).Do(request)
	if err != nil {
		return answer, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return answer, fmt.Errorf("introspection status %d", response.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&answer)
	return answer, err
}

// OIDC accepts JWT access tokens from an OpenID Connect issuer and backs the
// dashboard's PKCE sign-in. Discovery runs on first use, so the gateway can
// start before its identity provider.
type OIDC struct {
	Issuer, ClientID, Audience, RequiredScope string
	HTTP                                      *http.Client
	Logger                                    *slog.Logger

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

func (o *OIDC) discover(ctx context.Context) (*oidc.Provider, *oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider == nil {
		provider, err := oidc.NewProvider(oidc.ClientContext(ctx, httpClient(o.HTTP)), o.Issuer)
		if err != nil {
			if o.Logger != nil {
				o.Logger.Debug("auth.oidc.discovery.failed", "error", err.Error())
			}
			return nil, nil, fmt.Errorf("%w: %w", ErrAuthUnavailable, err)
		}
		// The verifier checks iss, exp, the signature against the issuer's
		// JWKS, and that aud contains Audience. Audience is never the client
		// ID, so the dashboard's own ID tokens are refused.
		o.provider, o.verifier = provider, provider.Verifier(&oidc.Config{ClientID: o.Audience})
	}
	return o.provider, o.verifier, nil
}

func (o *OIDC) Verify(ctx context.Context, presented string) (string, error) {
	_, verifier, err := o.discover(ctx)
	if err != nil {
		return "", err
	}
	token, err := verifier.Verify(ctx, presented)
	if err != nil || token.Subject == "" {
		return "", ErrUnauthorized
	}
	// ponytail: reads the RFC 9068 space-delimited scope claim only; add scp
	// for issuers such as Okta or Entra ID.
	var claims struct {
		Scope string `json:"scope"`
	}
	if err := token.Claims(&claims); err != nil ||
		(o.RequiredScope != "" && !slices.Contains(strings.Fields(claims.Scope), o.RequiredScope)) {
		return "", ErrUnauthorized
	}
	return idpClientPrefix + token.Subject, nil
}

// serveSignIn tells the dashboard where to send people to sign in.
func (o *OIDC) serveSignIn(w http.ResponseWriter, r *http.Request) {
	provider, _, err := o.discover(r.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"authorization_endpoint": provider.Endpoint().AuthURL,
		"client_id":              o.ClientID,
		"scope":                  strings.TrimSpace("openid " + o.RequiredScope),
		"resource":               o.Audience,
	})
}

// exchange redeems the dashboard's authorization code. Relaying it means the
// issuer needs no CORS policy for this origin; PKCE still binds the code to
// the browser that began sign-in. Only the access token reaches the page.
func (o *OIDC) exchange(w http.ResponseWriter, r *http.Request) {
	provider, _, err := o.discover(r.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		WriteError(w, ErrInvalidRequest)
		return
	}
	code, verifier, redirect := r.PostForm.Get("code"), r.PostForm.Get("code_verifier"), r.PostForm.Get("redirect_uri")
	if code == "" || verifier == "" || redirect == "" {
		WriteError(w, ErrInvalidRequest)
		return
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {o.ClientID}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {o.Audience},
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, provider.Endpoint().TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		WriteError(w, fmt.Errorf("%w: %w", ErrAuthUnavailable, err))
		return
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := httpClient(o.HTTP).Do(request)
	if err != nil {
		WriteError(w, fmt.Errorf("%w: %w", ErrAuthUnavailable, err))
		return
	}
	defer response.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in,omitempty"`
	}
	if response.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token) != nil || token.AccessToken == "" {
		WriteError(w, ErrUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(token)
}

// audience decodes an aud claim, which is either a string or an array.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*a = audience{one}
		return nil
	}
	return json.Unmarshal(data, (*[]string)(a))
}

func httpClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return idpHTTP
}
