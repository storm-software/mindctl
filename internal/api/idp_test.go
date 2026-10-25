package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIssuer is a minimal OpenID Connect issuer that signs EdDSA access
// tokens, as Better Auth's jwt plugin does by default.
type fakeIssuer struct {
	*httptest.Server
	key ed25519.PrivateKey
}

func newFakeIssuer(t *testing.T, token http.HandlerFunc) *fakeIssuer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{key: private}
	mux := http.NewServeMux()
	issuer.Server = httptest.NewServer(mux)
	t.Cleanup(issuer.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize",
			"token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "kid": "k1", "alg": "EdDSA", "use": "sig",
			"x": base64.RawURLEncoding.EncodeToString(public),
		}}})
	})
	if token != nil {
		mux.HandleFunc("/token", token)
	}
	return issuer
}

func (f *fakeIssuer) sign(changes map[string]any) string {
	claims := map[string]any{
		"iss": f.URL, "sub": "user-1", "aud": "https://mindctl.test", "azp": "dashboard",
		"scope": "openid mindctl:admin", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	maps.Copy(claims, changes)
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "k1", "typ": "at+jwt"})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.key, []byte(signing)))
}

func TestOIDCAcceptsOnlyScopedAccessTokensForTheAudience(t *testing.T) {
	issuer := newFakeIssuer(t, nil)
	verifier := &OIDC{Issuer: issuer.URL, ClientID: "dashboard", Audience: "https://mindctl.test", RequiredScope: "mindctl:admin"}
	id, err := verifier.Verify(context.Background(), issuer.sign(nil))
	if err != nil || id != "idp:user-1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	for name, changes := range map[string]map[string]any{
		"missing scope":   {"scope": "openid"},
		"ID token":        {"aud": "dashboard"},
		"expired":         {"exp": time.Now().Add(-time.Minute).Unix()},
		"other issuer":    {"iss": "https://elsewhere.test"},
		"missing subject": {"sub": ""},
		"scope substring": {"scope": "openid mindctl:admin-lite"},
		"other audiences": {"aud": []string{"https://other.test"}},
	} {
		if _, err := verifier.Verify(context.Background(), issuer.sign(changes)); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: err=%v, want unauthorized", name, err)
		}
	}
	tampered := issuer.sign(nil)
	tampered = tampered[:len(tampered)-4] + "AAAA"
	if _, err := verifier.Verify(context.Background(), tampered); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("tampered signature: err=%v", err)
	}
}

func TestUnreachableIssuerIsUnavailableButStaticTokenStillWorks(t *testing.T) {
	issuer := newFakeIssuer(t, nil)
	issuer.Close()
	chain := Verifiers{StaticTokens{{ID: "management", Value: "break-glass"}}, &OIDC{Issuer: issuer.URL, ClientID: "dashboard", Audience: "https://mindctl.test"}}
	handler := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "Authorization", chain)
	for token, want := range map[string]int{"break-glass": http.StatusOK, "some.jwt.value": http.StatusServiceUnavailable} {
		request := httptest.NewRequest(http.MethodGet, "/v8/management/models", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Errorf("%s: status=%d, want %d", token, response.Code, want)
		}
	}
}

func TestIntrospectorCachesActiveAnswersAndRefusesOthers(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if user, secret, ok := r.BasicAuth(); !ok || user != "mindctl" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		answers := map[string]map[string]any{
			"good":           {"active": true, "sub": "user-1", "aud": "https://mindctl.test", "exp": time.Now().Add(time.Hour).Unix()},
			"other-audience": {"active": true, "sub": "user-2", "aud": []string{"https://other.test"}},
			"expired":        {"active": true, "sub": "user-3", "aud": "https://mindctl.test", "exp": time.Now().Add(-time.Second).Unix()},
		}
		answer, ok := answers[r.PostFormValue("token")]
		if !ok {
			answer = map[string]any{"active": false}
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	defer server.Close()
	introspector := &Introspector{URL: server.URL, ClientID: "mindctl", ClientSecret: "s3cret", Audience: "https://mindctl.test", TTL: time.Minute}
	for range 2 {
		if id, err := introspector.Verify(context.Background(), "good"); err != nil || id != "idp:user-1" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("active answer was not cached: %d calls", calls.Load())
	}
	for _, token := range []string{"revoked", "other-audience", "expired"} {
		if _, err := introspector.Verify(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: err=%v, want unauthorized", token, err)
		}
	}

	misconfigured := &Introspector{URL: server.URL, ClientID: "mindctl", ClientSecret: "wrong", TTL: time.Minute}
	handler := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request passed a failed introspection")
	}), "X-Mindctl-Token", misconfigured)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("X-Mindctl-Token", "good")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "auth_unavailable") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDashboardSignInRelaysOnlyTheAccessToken(t *testing.T) {
	issuer := newFakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		want := url.Values{
			"grant_type": {"authorization_code"}, "client_id": {"dashboard"}, "code": {"good-code"},
			"code_verifier": {"verifier"}, "redirect_uri": {"https://mindctl.test/dashboard/"}, "resource": {"https://mindctl.test"},
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Encode() != want.Encode() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 3600, "refresh_token": "rt", "id_token": "it"})
	})
	dashboard := Dashboard(&OIDC{Issuer: issuer.URL, ClientID: "dashboard", Audience: "https://mindctl.test", RequiredScope: "mindctl:admin"})

	response := httptest.NewRecorder()
	dashboard.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/dashboard/oidc", nil))
	var signIn map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &signIn); err != nil ||
		signIn["authorization_endpoint"] != issuer.URL+"/authorize" || signIn["scope"] != "openid mindctl:admin" || signIn["resource"] != "https://mindctl.test" {
		t.Fatalf("sign-in config=%s", response.Body.String())
	}

	for code, want := range map[string]int{"good-code": http.StatusOK, "bad-code": http.StatusUnauthorized} {
		form := url.Values{"code": {code}, "code_verifier": {"verifier"}, "redirect_uri": {"https://mindctl.test/dashboard/"}}
		request := httptest.NewRequest(http.MethodPost, "/dashboard/oidc/token", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		dashboard.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: status=%d body=%s", code, response.Code, response.Body.String())
		}
		if want == http.StatusOK && (response.Body.String() != "{\"access_token\":\"at\",\"expires_in\":3600}\n" || response.Header().Get("Cache-Control") != "no-store") {
			t.Fatalf("relayed %q", response.Body.String())
		}
	}
}
