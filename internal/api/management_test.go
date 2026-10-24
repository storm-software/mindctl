package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/storage"
)

func TestManagementUsageValidatesWindowAndPassesFilter(t *testing.T) {
	var got storage.UsageFilter
	management := &Management{Usage: func(_ context.Context, filter storage.UsageFilter) (storage.UsageReport, error) {
		got = filter
		return storage.UsageReport{Rows: []storage.UsageRow{}, Recent: []storage.UsageAttempt{}}, nil
	}}
	handler := management.Handler()
	for query, status := range map[string]int{
		"range=24h&bucket=1h&offset=-25200": http.StatusOK,
		"range=24h&bucket=1s":               http.StatusBadRequest,
		"range=9000h&bucket=24h":            http.StatusBadRequest,
		"range=720h&bucket=1m":              http.StatusBadRequest,
		"range=24h&bucket=1h&offset=x":      http.StatusBadRequest,
		"bucket=1h":                         http.StatusBadRequest,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v8/management/usage?"+query, nil))
		if recorder.Code != status {
			t.Fatalf("%s: status = %d, want %d: %s", query, recorder.Code, status, recorder.Body)
		}
	}
	if got.Bucket != time.Hour || got.Offset != -7*time.Hour || got.Recent != 100 || time.Since(got.Since)-24*time.Hour > time.Minute {
		t.Fatalf("filter = %+v", got)
	}
}

func TestDashboardServesStaticClientWithStrictPolicy(t *testing.T) {
	for _, path := range []string{"/dashboard/", "/dashboard/app.js", "/dashboard/style.css"} {
		recorder := httptest.NewRecorder()
		Dashboard().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
			t.Fatalf("%s: status = %d", path, recorder.Code)
		}
		if policy := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "script-src 'self'") ||
			strings.Contains(policy, "unsafe") {
			t.Fatalf("%s: policy = %q", path, policy)
		}
	}
}
