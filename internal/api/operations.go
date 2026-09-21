// Package api exposes the gateway's HTTP operations.
package api

import (
	"context"
	"net/http"
)

// Operations exposes exact GET probes. Probe failures never expose diagnostics.
func Operations(ready func(context.Context) error) http.Handler {
	return &operations{ready: ready}
}

type operations struct{ ready func(context.Context) error }

func (h *operations) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/healthz" {
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
		return
	}
	if h.ready(r.Context()) != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("{\"status\":\"unavailable\"}\n"))
		return
	}
	_, _ = w.Write([]byte("{\"status\":\"ready\"}\n"))
}
