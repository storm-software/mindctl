// Package anthropic adapts portable inference requests to the native Anthropic
// Messages API.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

const (
	maxResponseBytes      = 16 << 20
	anthropicVersion      = "2023-06-01"
	claudeOAuthBetaHeader = "oauth-2025-04-20"
)

type authMode int

const (
	authAPIKey authMode = iota
	authClaudeOAuth
)

// Client is an Anthropic Messages API adapter. apiKey is a resolved credential.
type Client struct {
	baseURL string
	apiKey  string
	auth    authMode
	http    *http.Client
}

var _ provider.Provider = (*Client)(nil)

// NewClient snapshots the supplied HTTP client and disables redirects, keeping
// the API key scoped to the configured endpoint.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	return newClientWithAuth(baseURL, apiKey, authAPIKey, httpClient)
}

// NewClaudeOAuthClient returns an adapter that receives caller-managed Claude
// subscription credentials exclusively from each request context.
func NewClaudeOAuthClient(baseURL string, httpClient *http.Client) *Client {
	return newClientWithAuth(baseURL, "", authClaudeOAuth, httpClient)
}

func newClientWithAuth(baseURL, apiKey string, auth authMode, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	copy := *httpClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, auth: auth, http: &copy}
}

// Execute performs one native non-streaming Messages request.
func (c *Client) Execute(ctx context.Context, model domain.Model, request inference.Request) (inference.Result, error) {
	body, err := toMessagesRequest(model, request, false)
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
	var wire messagesResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&wire); err != nil {
		return inference.Result{}, &provider.Error{Kind: provider.ErrorRetryable, Status: response.StatusCode, RequestID: requestID, Err: errors.New("invalid provider response")}
	}
	return fromMessagesResponse(wire, model, requestID), nil
}

// Stream opens a native Messages SSE stream and returns portable events.
func (c *Client) Stream(ctx context.Context, model domain.Model, request inference.Request) (provider.Stream, error) {
	body, err := toMessagesRequest(model, request, true)
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
	accessToken := c.apiKey
	var claudeCredential upstreamauth.ClaudeCredential
	if c.auth == authClaudeOAuth {
		credential, ok := upstreamauth.Claude(ctx)
		if !ok {
			return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
		}
		accessToken = credential.AccessToken
		claudeCredential = credential
	}
	if accessToken == "" {
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("invalid provider endpoint")}
	}
	if c.auth == authClaudeOAuth {
		request.Header.Set("Authorization", "Bearer "+accessToken)
		beta := claudeCredential.Beta
		if beta == "" {
			beta = claudeOAuthBetaHeader
		}
		request.Header.Set("anthropic-beta", beta)
	} else {
		request.Header.Set("x-api-key", accessToken)
	}
	version := anthropicVersion
	if c.auth == authClaudeOAuth && claudeCredential.Version != "" {
		version = claudeCredential.Version
	}
	request.Header.Set("anthropic-version", version)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", &provider.Error{Kind: provider.ErrorRetryable, Err: err}
	}
	requestID := response.Header.Get("request-id")
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
	body                          io.ReadCloser
	reader                        *provider.SSEReader
	requestID, responseID         string
	usage                         inference.Usage
	hasInputUsage, hasOutputUsage bool
	status                        string
	contentBlocks                 map[int]streamContentIdentity
}

type streamContentIdentity struct{ itemID, callID, name string }

func (s *stream) Next(ctx context.Context) (inference.Event, error) {
	frame, err := s.reader.Next(ctx)
	if err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return inference.Event{}, &provider.Error{Kind: provider.ErrorRetryable, RequestID: s.requestID, Err: errors.New("provider stream read failed")}
		}
		return inference.Event{}, err
	}
	if frame.Type == "error" {
		return inference.Event{}, &provider.Error{Kind: streamErrorKind(frame.Data), RequestID: s.requestID, Err: errors.New("provider stream returned an error")}
	}
	event, responseID, usage, err := parseStreamEvent(frame.Type, frame.Data, s.responseID, s.status)
	if err != nil {
		return inference.Event{}, &provider.Error{Kind: provider.ErrorRetryable, RequestID: s.requestID, Err: errors.New("invalid provider stream event")}
	}
	if responseID != "" {
		s.responseID = responseID
	}
	if event.Status != "" {
		s.status = event.Status
	}
	s.trackContentBlock(frame.Type, frame.Data, &event)
	if usage != nil {
		s.mergeUsage(usage)
		event.Usage = s.usage
	}
	event.ProviderRequestID = s.requestID
	return event, nil
}

func (s *stream) trackContentBlock(kind string, data []byte, event *inference.Event) {
	index, ok := contentBlockIndex(data)
	if !ok {
		return
	}
	if kind == "content_block_start" {
		if s.contentBlocks == nil {
			s.contentBlocks = make(map[int]streamContentIdentity)
		}
		s.contentBlocks[index] = streamContentIdentity{itemID: event.ItemID, callID: event.CallID, name: event.Name}
		return
	}
	if identity, ok := s.contentBlocks[index]; ok {
		event.ItemID, event.CallID, event.Name = identity.itemID, identity.callID, identity.name
	}
}

func contentBlockIndex(data []byte) (int, bool) {
	var frame struct {
		Index *int `json:"index"`
	}
	if json.Unmarshal(data, &frame) != nil || frame.Index == nil {
		return 0, false
	}
	return *frame.Index, true
}

func streamErrorKind(data []byte) provider.ErrorKind {
	var frame struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &frame) != nil {
		return provider.ErrorRetryable
	}
	switch frame.Error.Type {
	case "rate_limit_error":
		return provider.ErrorRateLimit
	case "overloaded_error":
		return provider.ErrorOverloaded
	case "authentication_error", "permission_error":
		return provider.ErrorAuthentication
	case "invalid_request_error", "not_found_error":
		return provider.ErrorInvalidRequest
	default:
		return provider.ErrorRetryable
	}
}

func (s *stream) mergeUsage(usage *messagesUsage) {
	if usage.InputTokens != nil && *usage.InputTokens >= 0 {
		s.usage.InputTokens = *usage.InputTokens
		s.hasInputUsage = true
	}
	if usage.OutputTokens != nil && *usage.OutputTokens >= 0 {
		s.usage.OutputTokens = *usage.OutputTokens
		s.hasOutputUsage = true
	}
	if usage.CacheReadInputTokens != nil && *usage.CacheReadInputTokens >= 0 {
		s.usage.CachedInputTokens = *usage.CacheReadInputTokens
	}
	s.usage.Known = s.hasInputUsage && s.hasOutputUsage
}

func (s *stream) Close() error { return s.body.Close() }

func parseStreamEvent(kind string, data []byte, previousResponseID, previousStatus string) (inference.Event, string, *messagesUsage, error) {
	var frame struct {
		Index   int `json:"index"`
		Message *struct {
			ID    string         `json:"id"`
			Usage *messagesUsage `json:"usage"`
		} `json:"message"`
		ContentBlock messageContent `json:"content_block"`
		Delta        struct {
			Type        string          `json:"type"`
			Text        string          `json:"text"`
			Thinking    string          `json:"thinking"`
			Signature   string          `json:"signature"`
			PartialJSON string          `json:"partial_json"`
			StopReason  json.RawMessage `json:"stop_reason"`
		} `json:"delta"`
		Usage *messagesUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return inference.Event{}, "", nil, err
	}
	event := inference.Event{Type: kind, ResponseID: previousResponseID, Data: append(json.RawMessage(nil), data...)}
	var usage *messagesUsage
	switch kind {
	case "message_start":
		if frame.Message != nil {
			event.ResponseID = frame.Message.ID
			usage = frame.Message.Usage
		}
		event.Status = "in_progress"
	case "content_block_start":
		event.ItemID = itemID(frame.Index, frame.ContentBlock)
		event.ItemType = frame.ContentBlock.Type
		event.CallID, event.Name = frame.ContentBlock.ID, frame.ContentBlock.Name
	case "content_block_delta":
		event.ItemID = strconv.Itoa(frame.Index)
		switch frame.Delta.Type {
		case "text_delta":
			event.Delta = frame.Delta.Text
		case "input_json_delta":
			event.ArgumentsDelta = frame.Delta.PartialJSON
		case "thinking_delta":
			event.ItemType, event.Thinking = "thinking", frame.Delta.Thinking
		case "signature_delta":
			event.ItemType, event.Signature = "thinking", frame.Delta.Signature
		}
	case "message_delta":
		event.Status = statusForStop(frame.Delta.StopReason)
		_ = json.Unmarshal(frame.Delta.StopReason, &event.StopReason)
		usage = frame.Usage
	case "message_stop":
		event.Status = previousStatus
	}
	return event, event.ResponseID, usage, nil
}
