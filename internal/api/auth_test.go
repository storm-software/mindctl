package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/storm-software/mindctl/internal/upstreamauth"
)

func TestCaptureNativeClaudeOAuthIsRequestScoped(t *testing.T) {
	for _, test := range []struct {
		name, auth, beta, version string
		duplicate                 bool
		wantCredential            bool
	}{
		{name: "complete bearer", auth: "Bearer oauth-token", beta: "oauth-2025-04-20,context-1", version: "2023-06-01", wantCredential: true},
		{name: "missing"},
		{name: "duplicate", auth: "Bearer oauth-token", duplicate: true},
		{name: "invalid scheme", auth: "Basic oauth-token"},
		{name: "malformed bearer", auth: "Bearer oauth token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var credential upstreamauth.ClaudeCredential
			var present bool
			handler := CaptureNativeClaudeOAuth(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				credential, present = upstreamauth.Claude(request.Context())
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if test.auth != "" {
				request.Header.Add("Authorization", test.auth)
			}
			if test.duplicate {
				request.Header.Add("Authorization", test.auth)
			}
			if test.beta != "" {
				request.Header.Set("anthropic-beta", test.beta)
			}
			if test.version != "" {
				request.Header.Set("anthropic-version", test.version)
			}
			handler.ServeHTTP(httptest.NewRecorder(), request)
			if present != test.wantCredential {
				t.Fatalf("credential present=%v, want %v", present, test.wantCredential)
			}
			if test.wantCredential && (credential.AccessToken != "oauth-token" || credential.Beta != test.beta || credential.Version != test.version) {
				t.Fatal("credential did not retain request headers")
			}
		})
	}
}

// TestCaptureNativeClaudeOAuthToleratesRepeatedIdenticalProtocolHeaders covers
// a relay proxy (e.g. Headroom) repeating anthropic-beta/anthropic-version
// verbatim instead of folding them into one value. Unlike Authorization, these
// headers carry no credential weight, so identical repeats must not 400.
func TestCaptureNativeClaudeOAuthToleratesRepeatedIdenticalProtocolHeaders(t *testing.T) {
	var credential upstreamauth.ClaudeCredential
	var present bool
	handler := CaptureNativeClaudeOAuth(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		credential, present = upstreamauth.Claude(request.Context())
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Add("Authorization", "Bearer oauth-token")
	request.Header.Add("anthropic-beta", "oauth-2025-04-20,context-1")
	request.Header.Add("anthropic-beta", "oauth-2025-04-20,context-1")
	request.Header.Add("anthropic-version", "2023-06-01")
	request.Header.Add("anthropic-version", "2023-06-01")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusOK)
	}
	if !present {
		t.Fatal("credential not present")
	}
	if credential.Beta != "oauth-2025-04-20,context-1" || credential.Version != "2023-06-01" {
		t.Fatalf("credential did not retain repeated protocol headers: %+v", credential)
	}
}

// TestCaptureNativeClaudeOAuthRejectsConflictingProtocolHeaders keeps the
// existing fail-closed behavior for genuinely conflicting repeated values.
func TestCaptureNativeClaudeOAuthRejectsConflictingProtocolHeaders(t *testing.T) {
	for _, test := range []struct {
		name     string
		betas    []string
		versions []string
	}{
		{name: "conflicting beta", betas: []string{"a", "b"}, versions: []string{"2023-06-01"}},
		{name: "conflicting version", betas: []string{"a"}, versions: []string{"2023-06-01", "2024-01-01"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := CaptureNativeClaudeOAuth(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Fatal("next handler must not run for conflicting protocol headers")
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			request.Header.Add("Authorization", "Bearer oauth-token")
			for _, beta := range test.betas {
				request.Header.Add("anthropic-beta", beta)
			}
			for _, version := range test.versions {
				request.Header.Add("anthropic-version", version)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want %d", recorder.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestAuthenticateWithErrorKeepsExistingResponsesShape(t *testing.T) {
	for _, test := range []struct {
		name, header string
		custom       bool
	}{
		{name: "missing Responses token"},
		{name: "duplicate Responses token", header: "gateway"},
		{name: "custom Messages error", custom: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if test.header != "" {
				request.Header.Add("X-Mindctl-Token", test.header)
				request.Header.Add("X-Mindctl-Token", test.header)
			}
			called := false
			errorWriter := WriteError
			if test.custom {
				errorWriter = func(w http.ResponseWriter, _ error) {
					called = true
					w.WriteHeader(http.StatusTeapot)
				}
			}
			handler := AuthenticateWithError(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("unauthorized request reached handler")
			}), "X-Mindctl-Token", staticTokens{{ID: "client", Value: "gateway"}}, errorWriter)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if test.custom && (!called || response.Code != http.StatusTeapot) {
				t.Fatal("custom error writer not invoked")
			}
			if !test.custom && (response.Code != http.StatusUnauthorized || response.Body.String() == "") {
				t.Fatalf("Responses unauthorized envelope missing: %d", response.Code)
			}
		})
	}
}
