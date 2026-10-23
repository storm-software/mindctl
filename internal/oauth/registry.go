package oauth

import (
	"context"
	"html"
	"net/http"
	"sync"
)

// Registry matches an OAuth state to the authorization code from its callback.
// A callback with no matching login is rejected.
type Registry struct {
	mu      sync.Mutex
	waiting map[string]chan string
}

// NewRegistry returns an empty callback table.
func NewRegistry() *Registry {
	return &Registry{waiting: map[string]chan string{}}
}

// Callbacks is the gateway's login table. It only completes a request while
// a login started in this process is waiting.
var Callbacks = NewRegistry()

// Begin waits for the callback that presents state.
func (r *Registry) Begin(state string) <-chan string {
	ch := make(chan string, 1)
	r.mu.Lock()
	if r.waiting == nil {
		r.waiting = map[string]chan string{}
	}
	r.waiting[state] = ch
	r.mu.Unlock()
	return ch
}

// Finish delivers a code for a pending state. It reports whether one existed.
func (r *Registry) Finish(state, code string) bool {
	r.mu.Lock()
	ch, ok := r.waiting[state]
	if ok {
		delete(r.waiting, state)
	}
	r.mu.Unlock()
	if !ok || code == "" {
		return false
	}
	ch <- code
	return true
}

func (r *Registry) cancel(state string) {
	r.mu.Lock()
	delete(r.waiting, state)
	r.mu.Unlock()
}

// Handler serves the callback page. With no login in progress it is a 404.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !r.Finish(request.URL.Query().Get("state"), request.URL.Query().Get("code")) {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>Mindctl</title><p>" + html.EscapeString("Login complete. You can close this window.") + "</p>"))
	})
}

func (r *Registry) wait(ctx context.Context, state string, ready <-chan string) (string, error) {
	select {
	case <-ctx.Done():
		r.cancel(state)
		return "", ctx.Err()
	case code := <-ready:
		if code == "" {
			return "", context.Canceled
		}
		return code, nil
	}
}
