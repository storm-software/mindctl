package upstreamauth

import (
	"context"
	"net/http"
	"net/url"
)

type attemptContextKey struct{}

// Attempt carries one named-model account's call settings. It is absent for
// automatic routing, which keeps using the provider client constructed at startup.
type Attempt struct {
	APIKey, BaseURL, ProxyURL, UserAgent string
	ChatGPT                              *ChatGPTCredential
	Claude                               *ClaudeCredential
	Headers                              map[string]string
}

// WithAttempt returns a child context carrying one account attempt.
func WithAttempt(ctx context.Context, attempt Attempt) context.Context {
	return context.WithValue(ctx, attemptContextKey{}, attempt)
}

// AttemptFrom returns the account attempt attached to ctx.
func AttemptFrom(ctx context.Context) (Attempt, bool) {
	attempt, ok := ctx.Value(attemptContextKey{}).(Attempt)
	return attempt, ok
}

// ProxyClient clones base and sends its requests through proxyURL.
func ProxyClient(base *http.Client, proxyURL string) (*http.Client, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, err
	}
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	transport := http.DefaultTransport
	if base.Transport != nil {
		transport = base.Transport
	}
	if typed, ok := transport.(*http.Transport); ok {
		copied := typed.Clone()
		copied.Proxy = http.ProxyURL(parsed)
		clone.Transport = copied
		return &clone, nil
	}
	clone.Transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
	return &clone, nil
}
