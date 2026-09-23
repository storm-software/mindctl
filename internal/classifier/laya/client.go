// Package laya implements Mindctl's authenticated Laya classifier client.
package laya

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
	schemaVersion        = "mindctl.classifier.v1"
	defaultTimeout       = 5 * time.Second
	initialBackoff       = 25 * time.Millisecond
	maximumBackoff       = time.Second
	maximumResponseBytes = 1 << 20
)

type Client struct {
	config config.ClassifierConfig
	token  string
	http   *http.Client
}

var _ classifier.Classifier = (*Client)(nil)

func NewClient(cfg config.ClassifierConfig, token string, httpClient *http.Client) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	copy := *httpClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{config: cfg, token: token, http: &copy}
}

func (c *Client) Classify(parent context.Context, input classifier.Input) (domain.ClassifierJudgment, error) {
	if c.config.Timeout <= 0 || c.config.MaxRetries < 0 || strings.TrimSpace(c.config.Endpoint) == "" || c.token == "" {
		return domain.ClassifierJudgment{}, classifier.ErrUnavailable
	}
	body, err := json.Marshal(request{SchemaVersion: schemaVersion, State: input})
	if err != nil {
		return domain.ClassifierJudgment{}, classifier.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, c.config.Timeout)
	defer cancel()
	start, retries, delay := time.Now(), c.config.MaxRetries, initialBackoff
	for {
		if ctx.Err() != nil {
			return domain.ClassifierJudgment{}, classifier.ErrUnavailable
		}
		judgment, retry, err := c.attempt(ctx, body)
		if err == nil && ctx.Err() == nil {
			judgment.Latency = time.Since(start)
			return judgment, nil
		}
		if ctx.Err() != nil || !retry || retries == 0 {
			return domain.ClassifierJudgment{}, classifier.ErrUnavailable
		}
		retries--
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return domain.ClassifierJudgment{}, classifier.ErrUnavailable
		case <-timer.C:
		}
		delay = min(delay*2, maximumBackoff)
	}
}

func (c *Client) attempt(ctx context.Context, body []byte) (domain.ClassifierJudgment, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.config.Endpoint, "/")+"/v1/classify", bytes.NewReader(body))
	if err != nil {
		return domain.ClassifierJudgment{}, false, classifier.ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ClassifierJudgment{}, temporary(err), classifier.ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return domain.ClassifierJudgment{}, resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529, classifier.ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maximumResponseBytes+1))
	if err != nil || len(data) > maximumResponseBytes {
		return domain.ClassifierJudgment{}, temporary(err), classifier.ErrUnavailable
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
