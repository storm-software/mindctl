package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/storm-software/mindctl/internal/api"
)

func TestHealthAndReadiness(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	h := api.Operations(func(context.Context) error {
		if ready.Load() {
			return nil
		}
		return errors.New("sqlite unavailable: private diagnostic")
	})
	for _, available := range []bool{true, false} {
		ready.Store(available)
		for _, path := range []string{"/healthz", "/readyz"} {
			status, body := http.StatusOK, "{\"status\":\"ok\"}\n"
			if path == "/readyz" {
				body = "{\"status\":\"ready\"}\n"
				if !available {
					status, body = http.StatusServiceUnavailable, "{\"status\":\"unavailable\"}\n"
				}
			}
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
			if r.Code != status || r.Body.String() != body || r.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("%s ready=%v: status=%d body=%q headers=%v", path, available, r.Code, r.Body.String(), r.Header())
			}
		}
	}
}

func TestOperationsExposeOnlyExactGETRoutes(t *testing.T) {
	h := api.Operations(func(context.Context) error { return nil })
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/healthz", 405}, {"HEAD", "/readyz", 405}, {"HEAD", "/healthz", 405},
		{"GET", "/healthz/", 404}, {"GET", "/readyz/child", 404}, {"GET", "/v1/responses", 404},
		{"GET", "/v1/feedback", 404}, {"GET", "/", 404}, {"GET", "//healthz", 404},
	} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(tc.method, tc.path, nil))
		if r.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, r.Code, tc.status)
		}
	}
}

func TestReadinessReceivesRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := api.Operations(func(ctx context.Context) error { return ctx.Err() })
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/readyz", nil).WithContext(ctx))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", r.Code)
	}
}
