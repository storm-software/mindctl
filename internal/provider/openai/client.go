// Package openai adapts portable inference requests to the native OpenAI
// Responses API.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

const maxResponseBytes = 16 << 20

type authMode uint8

const (
	authAPIKey authMode = iota
	authChatGPTOAuth
)

// Client is an OpenAI Responses API adapter. apiKey is a resolved credential.
type Client struct {
	baseURL string
	apiKey  string
	auth    authMode
	http    *http.Client
}

var _ provider.Provider = (*Client)(nil)

// NewClient snapshots the supplied HTTP client and disables redirects, keeping
// the bearer credential scoped to the configured endpoint.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	return newClientWithAuth(baseURL, apiKey, authAPIKey, httpClient)
}

// NewChatGPTOAuthClient returns an adapter that receives caller-managed
// ChatGPT credentials exclusively from each request context.
func NewChatGPTOAuthClient(baseURL string, httpClient *http.Client) *Client {
	return newClientWithAuth(baseURL, "", authChatGPTOAuth, httpClient)
}

func newClientWithAuth(baseURL, apiKey string, auth authMode, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	copy := *httpClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, auth: auth, http: &copy}
}

// Execute performs one native non-streaming Responses request.
func (c *Client) Execute(ctx context.Context, model domain.Model, request inference.Request) (inference.Result, error) {
	body, err := toResponsesRequest(model, request, false)
	if err != nil {
		return inference.Result{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return inference.Result{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request")}
	}
	response, requestID, err := c.post(ctx, encoded, false)
	if err != nil {
		return inference.Result{}, err
	}
	defer response.Body.Close()
	var wire responsesResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&wire); err != nil {
		return inference.Result{}, &provider.Error{Kind: provider.ErrorRetryable, Status: response.StatusCode, RequestID: requestID, Err: errors.New("invalid provider response")}
	}
	return fromResponsesResponse(wire, model, requestID), nil
}

// Stream opens a native Responses SSE stream and returns portable events.
func (c *Client) Stream(ctx context.Context, model domain.Model, request inference.Request) (provider.Stream, error) {
	body, err := toResponsesRequest(model, request, true)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request")}
	}
	response, requestID, err := c.post(ctx, encoded, true)
	if err != nil {
		return nil, err
	}
	return &stream{body: response.Body, reader: provider.NewSSEReader(response.Body), requestID: requestID}, nil
}

func (c *Client) post(ctx context.Context, body []byte, stream bool) (*http.Response, string, error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
	}
	path := "/v1/responses"
	accessToken := c.apiKey
	accountID := ""
	switch c.auth {
	case authAPIKey:
		if accessToken == "" {
			return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
		}
	case authChatGPTOAuth:
		credential, ok := upstreamauth.ChatGPT(ctx)
		if !ok {
			return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
		}
		path = "/responses"
		accessToken = credential.AccessToken
		accountID = credential.AccountID
	default:
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("invalid provider endpoint")}
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	if c.auth == authChatGPTOAuth {
		request.Header.Set("ChatGPT-Account-Id", accountID)
		request.Header.Set("originator", "mindctl")
		request.Header.Set("User-Agent", "mindctl")
	}
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", &provider.Error{Kind: provider.ErrorRetryable, Err: err}
	}
	requestID := response.Header.Get("x-request-id")
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return nil, requestID, &provider.Error{Kind: errorKind(response.StatusCode), Status: response.StatusCode, RequestID: requestID, Err: errors.New("provider returned an error status")}
	}
	return response, requestID, nil
}

func errorKind(status int) provider.ErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrorRateLimit
	case status == 529:
		return provider.ErrorOverloaded
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.ErrorAuthentication
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return provider.ErrorInvalidRequest
	default:
		return provider.ErrorRetryable
	}
}

type stream struct {
	body      io.ReadCloser
	reader    *provider.SSEReader
	requestID string
}

func (s *stream) Next(ctx context.Context) (inference.Event, error) {
	frame, err := s.reader.Next(ctx)
	if err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return inference.Event{}, &provider.Error{Kind: provider.ErrorRetryable, RequestID: s.requestID, Err: errors.New("provider stream read failed")}
		}
		return inference.Event{}, err
	}
	if bytes.Equal(frame.Data, []byte("[DONE]")) {
		return inference.Event{}, io.EOF
	}
	if unsuccessfulTerminalFrame(frame.Type) {
		return inference.Event{}, provider.UnsuccessfulCompletionError(s.requestID)
	}
	event, err := streamEvent(frame.Type, frame.Data)
	if err != nil {
		return inference.Event{}, &provider.Error{Kind: provider.ErrorRetryable, RequestID: s.requestID, Err: errors.New("invalid provider stream event")}
	}
	if event.Status != "" && event.Status != "in_progress" && !provider.IsSuccessfulCompletion(event.Status) {
		return inference.Event{}, provider.UnsuccessfulCompletionError(s.requestID)
	}
	event.ProviderRequestID = s.requestID
	return event, nil
}

func (s *stream) Close() error { return s.body.Close() }

func unsuccessfulTerminalFrame(kind string) bool {
	switch kind {
	case "error", "response.failed", "response.cancelled", "response.incomplete":
		return true
	default:
		return false
	}
}
