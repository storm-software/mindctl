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
	"regexp"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

const maxResponseBytes = 16 << 20
const maxErrorBytes = 64 << 10

var (
	safeDiagnosticCode  = regexp.MustCompile(`^[a-z0-9_]+$`)
	safeDiagnosticParam = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\[[0-9]+\]|\.[a-z][a-z0-9_]*)*$`)
)

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
	body, err := c.toResponsesRequest(model, request, false)
	if err != nil {
		return inference.Result{}, err
	}
	c.applyAuthenticationContract(&body, false)
	encoded, err := json.Marshal(body)
	if err != nil {
		return inference.Result{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request")}
	}
	response, requestID, err := c.post(ctx, encoded, false, usesResponsesLite(request), errorSensitiveValues(request))
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
	body, err := c.toResponsesRequest(model, request, true)
	if err != nil {
		return nil, err
	}
	c.applyAuthenticationContract(&body, true)
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request")}
	}
	response, requestID, err := c.post(ctx, encoded, true, usesResponsesLite(request), errorSensitiveValues(request))
	if err != nil {
		return nil, err
	}
	return &stream{body: response.Body, reader: provider.NewSSEReader(response.Body), requestID: requestID}, nil
}

func (c *Client) toResponsesRequest(model domain.Model, request inference.Request, stream bool) (responsesRequest, error) {
	if c.auth == authChatGPTOAuth {
		return toCodexResponsesRequest(model, request, stream)
	}
	return toResponsesRequest(model, request, stream)
}

func (c *Client) applyAuthenticationContract(body *responsesRequest, stream bool) {
	if c.auth != authChatGPTOAuth {
		return
	}
	if body.ToolChoice == "" {
		body.ToolChoice = "auto"
	}
	if body.ParallelToolCalls == nil {
		body.ParallelToolCalls = boolPointer(false)
	}
	body.Store = boolPointer(false)
	body.Stream = boolPointer(stream)
	if body.Include == nil {
		include := []string{}
		body.Include = &include
	}
}

func usesResponsesLite(request inference.Request) bool {
	for _, item := range request.Input {
		if item.Type == "additional_tools" {
			return true
		}
	}
	return false
}

func (c *Client) post(ctx context.Context, body []byte, stream, responsesLite bool, sensitiveValues []string) (*http.Response, string, error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return nil, "", &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider client is not configured")}
	}
	path := "/v1/responses"
	accessToken := c.apiKey
	accountID, originator, userAgent := "", "", ""
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
		originator = credential.Originator
		userAgent = credential.UserAgent
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
		if originator == "" {
			originator = "mindctl"
		}
		request.Header.Set("originator", originator)
		if userAgent == "" {
			userAgent = "mindctl"
		}
		request.Header.Set("User-Agent", userAgent)
	}
	request.Header.Set("Content-Type", "application/json")
	if responsesLite {
		request.Header.Set("x-openai-internal-codex-responses-lite", "true")
	}
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", &provider.Error{Kind: provider.ErrorRetryable, Err: err}
	}
	requestID := response.Header.Get("x-request-id")
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		diagnostic := decodeErrorDiagnostic(response.Body, append(sensitiveValues, accessToken))
		response.Body.Close()
		return nil, requestID, &provider.Error{
			Kind:            errorKind(response.StatusCode),
			Status:          response.StatusCode,
			RequestID:       requestID,
			UpstreamCode:    diagnostic.UpstreamCode,
			UpstreamParam:   diagnostic.UpstreamParam,
			UpstreamMessage: diagnostic.UpstreamMessage,
			Err:             errors.New("provider returned an error status"),
		}
	}
	return response, requestID, nil
}

type errorDiagnostic struct {
	Error struct {
		Code    string `json:"code"`
		Param   string `json:"param"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErrorDiagnostic(body io.Reader, sensitiveValues []string) provider.Error {
	var diagnostic errorDiagnostic
	if err := json.NewDecoder(io.LimitReader(body, maxErrorBytes)).Decode(&diagnostic); err != nil {
		return provider.Error{}
	}
	code := safeDiagnosticValue(diagnostic.Error.Code, sensitiveValues)
	if !safeDiagnosticCode.MatchString(code) {
		code = ""
	}
	param := safeDiagnosticValue(diagnostic.Error.Param, sensitiveValues)
	if !safeDiagnosticParam.MatchString(param) {
		param = ""
	}
	message := safeDiagnosticMessage(diagnostic.Error.Message, param, sensitiveValues)
	return provider.Error{UpstreamCode: code, UpstreamParam: param, UpstreamMessage: message}
}

func errorSensitiveValues(request inference.Request) []string {
	values := []string{request.Instructions}
	for _, item := range request.Input {
		values = append(values, item.Text, item.Input, string(item.Arguments), string(item.Output))
		for _, content := range item.Content {
			values = append(values, content.Text)
		}
	}
	return values
}

func safeDiagnosticValue(value string, sensitiveValues []string) string {
	for _, sensitive := range sensitiveValues {
		if sensitive != "" && strings.Contains(strings.ToLower(value), strings.ToLower(sensitive)) {
			return ""
		}
	}
	return value
}

func safeDiagnosticMessage(message, param string, sensitiveValues []string) string {
	message = safeDiagnosticValue(message, sensitiveValues)
	if param == "" {
		return ""
	}
	for _, prefix := range []string{"Unsupported parameter: ", "Unknown parameter: ", "Invalid parameter: "} {
		if message == prefix+"'"+param+"'." {
			return message
		}
	}
	return ""
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
