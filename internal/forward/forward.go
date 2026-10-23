// Package forward sends a named-model request to the account the conductor
// selects. It does not classify or rank models.
package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/translate"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

const maxResponseBytes = 32 << 20

// Call is one raw named-model request.
type Call struct {
	Method, Model, CallerProtocol, Path string
	Body                                []byte
	Header                              http.Header
	SessionKey, ParentSessionKey        string
	Guardian                            bool
}

// Result is the upstream status and body after account selection.
type Result struct {
	Status int
	Header http.Header
	Body   []byte
}

// Forwarder selects an account and performs the call.
type Forwarder struct {
	Selector accounts.Selector
	Aliases  []accounts.Alias
	Models   []domain.Model
	HTTP     *http.Client
}

// Resolve returns the catalog model and provider for a client name.
func (f *Forwarder) Resolve(model string) (string, string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == "mindctl-auto" {
		return "", "", errors.New("forward: a named model is required")
	}
	if modelID, provider, ok := accounts.Resolve(f.aliases(), model); ok {
		model = modelID
		if provider != "" {
			if !f.known(model, provider) {
				return "", "", errors.New("forward: unknown model")
			}
			return model, provider, nil
		}
	}
	for _, candidate := range f.models() {
		if candidate.ID == model {
			return candidate.ID, candidate.Provider, nil
		}
	}
	return "", "", errors.New("forward: unknown model")
}

// Eligible reports whether any account can serve the model without consuming
// the round-robin cursor.
func (f *Forwarder) Eligible(model string) bool {
	modelID, provider, err := f.Resolve(model)
	if err != nil || f == nil || f.Selector == nil {
		return false
	}
	return f.Selector.HasEligible(accounts.SelectRequest{ModelID: modelID, Provider: provider})
}

// Do selects an account and calls it, translating only when the protocols differ.
func (f *Forwarder) Do(ctx context.Context, call Call) (Result, error) {
	if f == nil || f.Selector == nil {
		return Result{}, errors.New("forward: account selector is required")
	}
	modelID, provider, err := f.Resolve(call.Model)
	if err != nil {
		return Result{}, err
	}
	var last Result
	var lastErr error
	tried := []string{}
	for round := 0; round <= 32; round++ {
		selection, selectErr := f.Selector.Select(ctx, accounts.SelectRequest{
			ModelID: modelID, Provider: provider, SessionKey: call.SessionKey,
			ParentSessionKey: call.ParentSessionKey, Guardian: call.Guardian,
			Round: round, Tried: append([]string(nil), tried...),
		})
		if selectErr != nil {
			if last.Status != 0 {
				return last, nil
			}
			if lastErr != nil {
				return Result{}, lastErr
			}
			return Result{}, selectErr
		}
		body := call.Body
		accountProtocol := selection.Account.Protocol
		if accountProtocol == "" {
			accountProtocol = nativeProtocol(selection.Account.Provider)
		}
		translated := false
		if call.CallerProtocol != "" && accounts.ProtocolFamily(call.CallerProtocol) != accounts.ProtocolFamily(accountProtocol) {
			converted, convertErr := translate.Request(call.CallerProtocol, accountProtocol, call.Body)
			if convertErr != nil {
				return Result{}, convertErr
			}
			body = converted
			translated = true
		}
		result, callErr := f.once(ctx, selection, call, body)
		if callErr != nil {
			f.Selector.ReportFailure(ctx, call.SessionKey, selection.Account.ID, accounts.FailureTransient)
			lastErr = callErr
			tried = append(tried, selection.Account.ID)
			continue
		}
		if result.Status >= 200 && result.Status < 300 {
			f.Selector.ReportSuccess(ctx, call.SessionKey, selection.Account.ID)
			if translated {
				converted, convertErr := translate.Response(accountProtocol, call.CallerProtocol, result.Body)
				if convertErr != nil {
					return Result{}, convertErr
				}
				result.Body = converted
			}
			return result, nil
		}
		class := accounts.ClassFromStatus(result.Status)
		f.Selector.ReportFailure(ctx, call.SessionKey, selection.Account.ID, class)
		last = result
		if class == accounts.FailurePermanent {
			return result, nil
		}
		tried = append(tried, selection.Account.ID)
	}
	if last.Status != 0 {
		return last, nil
	}
	if lastErr == nil {
		lastErr = accounts.ErrNone
	}
	return Result{}, lastErr
}

func (f *Forwarder) once(ctx context.Context, selection accounts.Selection, call Call, body []byte) (Result, error) {
	base := selection.Account.BaseURL
	if base == "" {
		return Result{}, errors.New("forward: account has no base URL")
	}
	target, err := url.Parse(strings.TrimRight(base, "/") + normalizePath(base, call.Path))
	if err != nil {
		return Result{}, err
	}
	method := call.Method
	if method == "" {
		method = http.MethodPost
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	applyUpstreamAuth(request, selection)
	if call.Header != nil {
		if contentType := call.Header.Get("Content-Type"); contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		if accept := call.Header.Get("Accept"); accept != "" {
			request.Header.Set("Accept", accept)
		}
	}
	client := f.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	if selection.Account.ProxyURL != "" {
		proxied, proxyErr := upstreamauth.ProxyClient(client, selection.Account.ProxyURL)
		if proxyErr != nil {
			return Result{}, errors.New("forward: invalid account proxy")
		}
		client = proxied
	}
	response, err := client.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(payload) > maxResponseBytes {
		return Result{}, errors.New("forward: upstream response is too large")
	}
	header := make(http.Header)
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return Result{Status: response.StatusCode, Header: header, Body: payload}, nil
}

func applyUpstreamAuth(request *http.Request, selection accounts.Selection) {
	protocol := accounts.ProtocolFamily(selection.Account.Protocol)
	if protocol == "" {
		protocol = accounts.ProtocolFamily(nativeProtocol(selection.Account.Provider))
	}
	token := selection.Secret.APIKey
	if token == "" {
		token = selection.Secret.AccessToken
	}
	switch protocol {
	case "gemini":
		if token != "" {
			request.Header.Set("x-goog-api-key", token)
		}
	case "anthropic":
		if selection.Secret.AccessToken != "" && selection.Account.Kind != accounts.KindAPIKey {
			request.Header.Set("Authorization", "Bearer "+selection.Secret.AccessToken)
			request.Header.Set("anthropic-version", "2023-06-01")
			return
		}
		if token != "" {
			request.Header.Set("x-api-key", token)
			request.Header.Set("anthropic-version", "2023-06-01")
		}
	default:
		if selection.Secret.AccessToken != "" && selection.Secret.AccountID != "" {
			request.Header.Set("Authorization", "Bearer "+selection.Secret.AccessToken)
			request.Header.Set("ChatGPT-Account-Id", selection.Secret.AccountID)
			return
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
}

func normalizePath(base, path string) string {
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasSuffix(strings.TrimRight(base, "/"), "/v1") && strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

func nativeProtocol(providerID string) string {
	switch providerID {
	case "anthropic":
		return "anthropic"
	case "gemini", "antigravity", "vertex":
		return "gemini"
	default:
		return "openai"
	}
}

func (f *Forwarder) aliases() []accounts.Alias {
	if f == nil {
		return nil
	}
	return f.Aliases
}

func (f *Forwarder) models() []domain.Model {
	if f == nil {
		return nil
	}
	return f.Models
}

func (f *Forwarder) known(model, provider string) bool {
	for _, candidate := range f.models() {
		if candidate.ID == model && (provider == "" || candidate.Provider == provider) {
			return true
		}
	}
	return false
}

// ModelFromJSON reads a model field without interpreting the rest of the body.
func ModelFromJSON(body []byte) (string, error) {
	var decoded struct {
		Model string `json:"model"`
	}
	if err := jsonUnmarshal(body, &decoded); err != nil {
		return "", err
	}
	return decoded.Model, nil
}

func jsonUnmarshal(body []byte, dest any) error {
	if len(body) == 0 {
		return errors.New("forward: empty body")
	}
	decoder := newDecoder(body)
	if err := decoder.Decode(dest); err != nil {
		return fmt.Errorf("forward: %w", err)
	}
	return nil
}
