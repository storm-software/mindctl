package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage"
)

var (
	// ErrInvalidRequest classifies syntax and portable-contract failures.
	ErrInvalidRequest = inference.ErrInvalidRequest
	// ErrBodyTooLarge identifies requests exceeding the configured body limit.
	ErrBodyTooLarge = errors.New("request body too large")
	// ErrUnauthorized identifies missing or invalid gateway authentication.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrMethodNotAllowed identifies an unsupported endpoint method.
	ErrMethodNotAllowed = errors.New("method not allowed")
)

type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// WriteError emits only stable Responses-style errors. Internal and provider
// error text is intentionally never serialized.
func WriteError(w http.ResponseWriter, err error) {
	status, detail := errorDetail(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: detail})
}

func errorDetail(err error) (int, ErrorDetail) {
	switch {
	case errors.Is(err, ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed, ErrorDetail{Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"}
	case errors.Is(err, ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge, ErrorDetail{Message: "request body too large", Type: "invalid_request_error", Code: "body_too_large"}
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized, ErrorDetail{Message: "missing or invalid authentication", Type: "authentication_error", Code: "invalid_api_key"}
	case errors.Is(err, ErrInvalidRequest):
		detail := ErrorDetail{Message: "invalid request", Type: "invalid_request_error"}
		var validation *inference.ValidationError
		if errors.As(err, &validation) {
			detail.Param = validation.Param
		}
		return http.StatusBadRequest, detail
	case isNoEligibleModel(err):
		return http.StatusBadRequest, ErrorDetail{Message: "no configured model can satisfy this request", Type: "invalid_request_error", Code: "model_not_available"}
	case isUnsupportedFeature(err):
		return http.StatusBadRequest, ErrorDetail{Message: "request uses an unsupported feature", Type: "invalid_request_error", Code: "unsupported_feature"}
	case errors.Is(err, conversation.ErrNotFound):
		return http.StatusNotFound, ErrorDetail{Message: "response not found", Type: "invalid_request_error", Code: "response_not_found"}
	case isProviderKind(err, provider.ErrorRateLimit):
		return http.StatusTooManyRequests, ErrorDetail{Message: "rate limit exceeded", Type: "rate_limit_error", Code: "rate_limit_exceeded"}
	case isProviderKind(err, provider.ErrorOverloaded), isProviderKind(err, provider.ErrorRetryable):
		return http.StatusServiceUnavailable, ErrorDetail{Message: "provider is temporarily unavailable", Type: "server_error", Code: "provider_unavailable"}
	case isProviderKind(err, provider.ErrorAuthentication):
		return http.StatusServiceUnavailable, ErrorDetail{Message: "provider authentication is unavailable", Type: "server_error", Code: "provider_authentication_unavailable"}
	case isProviderKind(err, provider.ErrorSafetyRefusal):
		return http.StatusBadRequest, ErrorDetail{Message: "provider refused this request", Type: "invalid_request_error", Code: "safety_refusal"}
	case isProviderKind(err, provider.ErrorInvalidRequest):
		return http.StatusBadRequest, ErrorDetail{Message: "provider cannot execute this request", Type: "invalid_request_error", Code: "provider_request_invalid"}
	case isStorageError(err):
		return http.StatusServiceUnavailable, ErrorDetail{Message: "gateway persistence is unavailable", Type: "server_error", Code: "persistence_unavailable"}
	default:
		return http.StatusInternalServerError, ErrorDetail{Message: "internal server error", Type: "server_error"}
	}
}

func isNoEligibleModel(err error) bool {
	var target *router.NoEligibleModelError
	return errors.As(err, &target)
}

func isUnsupportedFeature(err error) bool {
	var target *provider.UnsupportedFeatureError
	return errors.As(err, &target)
}

func isProviderKind(err error, kind provider.ErrorKind) bool {
	var target *provider.Error
	return errors.As(err, &target) && target.Kind == kind
}

func isStorageError(err error) bool {
	var target *storage.Error
	return errors.As(err, &target)
}
