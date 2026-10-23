package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNamedRoutesRejectAutomaticModel(t *testing.T) {
	cfg, env := fixture(t)
	application, err := newWithLookup(context.Background(), cfg, lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if status(application, "/v1/models") != http.StatusUnauthorized {
		t.Fatalf("status=%d", status(application, "/v1/models"))
	}
	list := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	list.Header.Set("Authorization", "Bearer private-gateway-token")
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, list)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "first") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	completion := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"mindctl-auto","prompt":"hello"}`))
	completion.Header.Set("Authorization", "Bearer private-gateway-token")
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, completion)
	if response.Code == http.StatusOK {
		t.Fatalf("automatic completion was accepted: %s", response.Body.String())
	}
}
