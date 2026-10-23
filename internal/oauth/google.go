package oauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const googleCloudScope = "https://www.googleapis.com/auth/cloud-platform"

// ServiceAccount is the non-logged material of a Vertex or Google credential.
type ServiceAccount struct {
	ClientEmail, PrivateKey, TokenURI, Scope string
}

type cachedToken struct {
	value   string
	expires time.Time
}

var googleTokens struct {
	mu sync.Mutex
	m  map[string]cachedToken
}

// GoogleAccessToken mints a bearer token for a service-account JSON key.
// The private key stays in the request and is not written to the error text.
func GoogleAccessToken(ctx context.Context, account ServiceAccount) (string, error) {
	if account.ClientEmail == "" || strings.TrimSpace(account.PrivateKey) == "" {
		return "", errors.New("oauth: service account is incomplete")
	}
	tokenURI := account.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	scope := account.Scope
	if scope == "" {
		scope = googleCloudScope
	}
	cacheKey := account.ClientEmail + "\x00" + tokenURI + "\x00" + scope
	googleTokens.mu.Lock()
	if googleTokens.m == nil {
		googleTokens.m = map[string]cachedToken{}
	}
	if cached, ok := googleTokens.m[cacheKey]; ok && time.Until(cached.expires) > time.Minute {
		googleTokens.mu.Unlock()
		return cached.value, nil
	}
	googleTokens.mu.Unlock()

	key, err := parseRSAKey(account.PrivateKey)
	if err != nil {
		return "", err
	}
	assertion, err := serviceAccountJWT(key, account.ClientEmail, tokenURI, scope, time.Now())
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("oauth: token endpoint returned %d", response.StatusCode)
	}
	var decoded struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.AccessToken == "" {
		return "", errors.New("oauth: token endpoint returned no access token")
	}
	expires := time.Duration(decoded.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = time.Hour
	}
	googleTokens.mu.Lock()
	googleTokens.m[cacheKey] = cachedToken{value: decoded.AccessToken, expires: time.Now().Add(expires)}
	googleTokens.mu.Unlock()
	return decoded.AccessToken, nil
}

func parseRSAKey(material string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(material))
	if block == nil {
		return nil, errors.New("oauth: service account private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("oauth: service account private key is not RSA")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("oauth: service account private key is not RSA")
	}
	return key, nil
}

func serviceAccountJWT(key *rsa.PrivateKey, email, tokenURI, scope string, now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": email, "scope": scope, "aud": tokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
