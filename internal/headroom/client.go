package headroom

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

const (
	maxRequestBytes  = 8 << 20
	maxResponseBytes = 8 << 20
	requestTimeout   = 30 * time.Second
)

type Metrics struct {
	TokensBefore int64 `json:"tokens_before"`
	TokensAfter  int64 `json:"tokens_after"`
	TokensSaved  int64 `json:"tokens_saved"`
}

type Compressor interface {
	Compress(context.Context, domain.Model, string, string, inference.Request) (inference.Request, Metrics, error)
}

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	clone := *httpClient
	clone.Transport = &noRedirectTransport{base: transport}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, httpClient: &clone}
}

func (c *Client) Ready(ctx context.Context) error {
	request := compressionRequest{Model: "", Config: compressionConfig{SessionID: "readiness"}, Messages: []compressionMessage{}}
	response, err := c.do(ctx, request)
	if err == nil && response.Skipped {
		return unavailable(errors.New("readiness compression skipped"))
	}
	return err
}

func (c *Client) Compress(ctx context.Context, model domain.Model, conversationID, providerID string, request inference.Request) (inference.Request, Metrics, error) {
	copyRequest, err := cloneRequest(request)
	if err != nil {
		return inference.Request{}, Metrics{}, unavailable(err)
	}
	messages, eligible := eligibleMessages(copyRequest)
	if !eligible {
		if err := c.Ready(ctx); err != nil {
			return inference.Request{}, Metrics{}, err
		}
		return copyRequest, Metrics{}, nil
	}
	session := sessionID(conversationID, providerID, model.ID)
	response, err := c.do(ctx, compressionRequest{
		Model:    model.UpstreamID,
		Config:   compressionConfig{SessionID: session},
		Messages: messages,
	})
	if err != nil {
		return inference.Request{}, Metrics{}, err
	}
	if response.Skipped || len(response.Messages) != len(messages) {
		return inference.Request{}, Metrics{}, unavailable(errors.New("compression skipped or incomplete"))
	}
	if err := applyMessages(&copyRequest, response.Messages); err != nil {
		return inference.Request{}, Metrics{}, unavailable(err)
	}
	if response.Metrics.TokensBefore < 0 || response.Metrics.TokensAfter < 0 || response.Metrics.TokensSaved < 0 || response.Metrics.TokensAfter > response.Metrics.TokensBefore || response.Metrics.TokensSaved != response.Metrics.TokensBefore-response.Metrics.TokensAfter {
		return inference.Request{}, Metrics{}, unavailable(errors.New("invalid compression metrics"))
	}
	return copyRequest, response.Metrics, nil
}

type compressionRequest struct {
	Model    string               `json:"model"`
	Config   compressionConfig    `json:"config"`
	Messages []compressionMessage `json:"messages"`
}

type compressionConfig struct {
	SessionID string `json:"session_id"`
}

type compressionMessage struct {
	Index int    `json:"index"`
	Role  string `json:"role"`
	Text  string `json:"text"`
}

type compressionResponse struct {
	Skipped  bool                 `json:"compression_skipped"`
	Messages []compressionMessage `json:"messages"`
	Metrics  Metrics              `json:"metrics"`
}

func (c *Client) do(ctx context.Context, payload compressionRequest) (compressionResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil || len(body) > maxRequestBytes {
		return compressionResponse{}, unavailable(errors.New("compression request is too large"))
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+"/v1/compress", bytes.NewReader(body))
	if err != nil {
		return compressionResponse{}, unavailable(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return compressionResponse{}, unavailable(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return compressionResponse{}, unavailable(errors.New("compression service rejected request"))
	}
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > maxResponseBytes {
		return compressionResponse{}, unavailable(errors.New("compression response is too large"))
	}
	var decoded compressionResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return compressionResponse{}, unavailable(errors.New("malformed compression response"))
	}
	return decoded, nil
}

func unavailable(err error) error {
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

func sessionID(conversationID, providerID, modelID string) string {
	digest := sha256.Sum256([]byte(conversationID + "\x00" + providerID + "\x00" + modelID))
	return hex.EncodeToString(digest[:])
}

type noRedirectTransport struct{ base http.RoundTripper }

func (t *noRedirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(request)
}
