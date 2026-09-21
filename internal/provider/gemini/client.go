// Package gemini adapts portable inference requests to the native Gemini API.
package gemini

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
)

const maxResponseBytes = 16 << 20

// Client is a Gemini generateContent adapter. apiKey is a resolved credential.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

var _ provider.Provider = (*Client)(nil)

// NewClient snapshots the supplied HTTP client and disables redirects, keeping
// the API key scoped to the configured endpoint.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	copy := *httpClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: &copy}
}

// Execute performs one native Gemini generateContent request.
func (c *Client) Execute(ctx context.Context, model domain.Model, request inference.Request) (inference.Result, error) {
	body, err := toGenerateRequest(model, request)
	if err != nil {
		return inference.Result{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return inference.Result{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request")}
	}
	response, requestID, err := c.post(ctx, model.UpstreamID, encoded, false)
	if err != nil {
		return inference.Result{}, err
	}
	defer response.Body.Close()
	var wire generateResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&wire); err != nil {
		return inference.Result{}, &provider.Error{Kind: provider.ErrorRetryable, Status: response.StatusCode, RequestID: requestID, Err: errors.New("invalid provider response")}
	}
	result, err := fromGenerateResponse(wire, model, requestID)
	if err != nil {
		return inference.Result{}, err
	}
	return result, nil
}

// Stream opens a native Gemini streamGenerateContent SSE stream.
func (c *Client) Stream(ctx context.Context, model domain.Model, request inference.Request) (provider.Stream, error) {
	body, err := toGenerateRequest(model, request)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request")}
	}
	response, requestID, err := c.post(ctx, model.UpstreamID, encoded, true)
	if err != nil {
		return nil, err
	}
	return &stream{body: response.Body, reader: provider.NewSSEReader(response.Body), requestID: requestID}, nil
}

func (c *Client) post(ctx context.Context, modelID string, body []byte, stream bool) (*http.Response, string, error) {
	if strings.TrimSpace(c.baseURL) == "" || c.apiKey == "" {
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
	}
	path := "/v1beta/models/" + modelID + ":generateContent"
	if stream {
		path = "/v1beta/models/" + modelID + ":streamGenerateContent?alt=sse"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("invalid provider endpoint")}
	}
	request.Header.Set("x-goog-api-key", c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", &provider.Error{Kind: provider.ErrorRetryable, Err: err}
	}
	requestID := responseRequestID(response)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return nil, requestID, &provider.Error{Kind: errorKind(response.StatusCode), Status: response.StatusCode, RequestID: requestID, Err: errors.New("provider returned an error status")}
	}
	return response, requestID, nil
}

func responseRequestID(response *http.Response) string {
	for _, header := range []string{"x-goog-request-id", "x-request-id", "request-id"} {
		if value := response.Header.Get(header); value != "" {
			return value
		}
	}
	return ""
}

func errorKind(status int) provider.ErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrorRateLimit
	case status == http.StatusServiceUnavailable || status == 529:
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
	body       io.ReadCloser
	reader     *provider.SSEReader
	requestID  string
	responseID string
	status     string
}

func (s *stream) Next(ctx context.Context) (inference.Event, error) {
	frame, err := s.reader.Next(ctx)
	if err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return inference.Event{}, &provider.Error{Kind: provider.ErrorRetryable, RequestID: s.requestID, Err: errors.New("provider stream read failed")}
		}
		return inference.Event{}, err
	}
	if kind, ok := streamErrorKind(frame.Data); ok {
		return inference.Event{}, &provider.Error{Kind: kind, RequestID: s.requestID, Err: errors.New("provider stream returned an error")}
	}
	event, responseID, status, err := streamEvent(frame.Data, s.responseID)
	if err != nil {
		return inference.Event{}, &provider.Error{Kind: provider.ErrorRetryable, RequestID: s.requestID, Err: errors.New("invalid provider stream event")}
	}
	if responseID != "" {
		s.responseID = responseID
	}
	if status != "" {
		s.status = status
	}
	event.ProviderRequestID = s.requestID
	return event, nil
}

func (s *stream) Close() error { return s.body.Close() }

func streamErrorKind(data []byte) (provider.ErrorKind, bool) {
	var wire struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Error == nil {
		return "", false
	}
	return errorKind(wire.Error.Code), true
}

func streamEvent(data []byte, previousResponseID string) (inference.Event, string, string, error) {
	var wire struct {
		ResponseID     string          `json:"responseId"`
		Candidates     []candidate     `json:"candidates"`
		PromptFeedback *promptFeedback `json:"promptFeedback"`
		UsageMetadata  json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return inference.Event{}, "", "", err
	}
	if (wire.PromptFeedback != nil && nonEmptyJSON(wire.PromptFeedback.BlockReason)) || len(wire.Candidates) == 0 {
		return inference.Event{}, "", "", errors.New("provider stream returned an error")
	}
	responseID := wire.ResponseID
	if responseID == "" {
		responseID = previousResponseID
	}
	candidate := wire.Candidates[0]
	event := inference.Event{Type: "response.in_progress", ResponseID: responseID, Status: "in_progress", Data: append(json.RawMessage(nil), data...)}
	if candidate.FinishReason != nil {
		status, err := finishStatus(candidate.FinishReason)
		if err != nil || status == "safety" {
			return inference.Event{}, "", "", errors.New("invalid provider finish reason")
		}
		event.Type, event.Status = "response.completed", status
	}
	if len(wire.UsageMetadata) != 0 && string(wire.UsageMetadata) != "null" {
		usage, err := parseUsage(wire.UsageMetadata)
		if err != nil {
			return inference.Event{}, "", "", err
		}
		event.Usage = usage
	}
	if len(candidate.Content.Parts) == 0 {
		return event, responseID, event.Status, nil
	}
	mapped := candidate.Content.Parts[0]
	switch {
	case mapped.Text != "":
		event.Type, event.Delta = "response.output_text.delta", mapped.Text
	case mapped.FunctionCall != nil:
		arguments, err := json.Marshal(mapped.FunctionCall.Args)
		if err != nil {
			return inference.Event{}, "", "", err
		}
		event.Type, event.ItemID, event.CallID, event.Name, event.ArgumentsDelta = "response.function_call_arguments.delta", mapped.FunctionCall.Name, mapped.FunctionCall.Name, mapped.FunctionCall.Name, string(arguments)
	}
	return event, responseID, event.Status, nil
}
