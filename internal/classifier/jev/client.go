// Package jev implements the TypeSafe System One requirement classifier.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/domain"
)

const (
	defaultTimeout       = 5 * time.Second
	initialBackoff       = 25 * time.Millisecond
	maximumBackoff       = time.Second
	maximumResponseBytes = 1 << 20
)

type Client struct {
	config config.JevConfig
	apiKey string
	http   *http.Client
}

var _ classifier.Classifier = (*Client)(nil)

// NewClient snapshots configuration and the HTTP client. apiKey is a resolved
// secret, not the config's environment-variable name. MaxRetries counts retries
// after the initial attempt. A zero timeout uses a five-second outer deadline.
// Redirects are rejected so classification and its credentials stay on endpoint.
func NewClient(cfg config.JevConfig, apiKey string, httpClient *http.Client) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{config: cfg, apiKey: apiKey, http: &clientCopy}
}

// Classify emits one logical six-question classification. Retries repeat that
// same payload. Errors deliberately exclude remote bodies and transport errors,
// either of which can contain user data. Routing remains the policy's job.
func (c *Client) Classify(parent context.Context, input classifier.Input) (domain.JevJudgment, error) {
	start := time.Now()
	if c.config.Timeout <= 0 || c.config.MaxRetries < 0 || strings.TrimSpace(c.config.BaseURL) == "" || strings.TrimSpace(c.config.Model) == "" || c.apiKey == "" {
		return domain.JevJudgment{}, classifier.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, c.config.Timeout)
	defer cancel()
	body, err := json.Marshal(request{State: input, Model: c.config.Model, Questions: questions()})
	if err != nil {
		return domain.JevJudgment{}, classifier.ErrUnavailable
	}
	retries, delay := c.config.MaxRetries, initialBackoff
	for {
		if ctx.Err() != nil {
			return domain.JevJudgment{}, classifier.ErrUnavailable
		}
		judgment, retry, err := c.attempt(ctx, body)
		if err == nil && ctx.Err() == nil {
			judgment.Latency = time.Since(start)
			return judgment, nil
		}
		if ctx.Err() != nil || !retry || retries == 0 {
			return domain.JevJudgment{}, classifier.ErrUnavailable
		}
		retries--
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return domain.JevJudgment{}, classifier.ErrUnavailable
		case <-timer.C:
		}
		delay = min(delay*2, maximumBackoff)
	}
}

func (c *Client) attempt(ctx context.Context, body []byte) (domain.JevJudgment, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.config.BaseURL, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return domain.JevJudgment{}, false, classifier.ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return domain.JevJudgment{}, temporary(err), classifier.ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return domain.JevJudgment{}, resp.StatusCode == 429 || resp.StatusCode == 529, classifier.ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maximumResponseBytes+1))
	if err != nil {
		return domain.JevJudgment{}, temporary(err), classifier.ErrUnavailable
	}
	if len(data) > maximumResponseBytes {
		return domain.JevJudgment{}, false, classifier.ErrUnavailable
	}
	judgment, err := parseJudgment(data)
	return judgment, false, err
}

func temporary(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var transportError net.Error
	return errors.As(err, &transportError) && transportError.Temporary()
}
