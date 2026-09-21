package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/storm-software/mindctl/internal/inference"
)

var (
	// ErrInvalidRequest classifies syntax and portable-contract failures.
	ErrInvalidRequest = inference.ErrInvalidRequest
	// ErrBodyTooLarge identifies requests exceeding the configured body limit.
	ErrBodyTooLarge = errors.New("request body too large")
	// ErrUnauthorized identifies missing or invalid gateway authentication.
	ErrUnauthorized = errors.New("unauthorized")
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
	default:
		return http.StatusInternalServerError, ErrorDetail{Message: "internal server error", Type: "server_error"}
	}
}
