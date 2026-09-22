// Package provider defines native inference adapters behind a provider-neutral
// boundary used by the gateway executor.
package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

// Provider executes portable inference requests against one native provider.
type Provider interface {
	Execute(context.Context, domain.Model, inference.Request) (inference.Result, error)
	Stream(context.Context, domain.Model, inference.Request) (Stream, error)
}

// Stream is a provider-native stream normalized into portable events.
type Stream interface {
	Next(context.Context) (inference.Event, error)
	Close() error
}

// ErrorKind classifies provider failures for executor retry decisions.
type ErrorKind string

const (
	ErrorRetryable      ErrorKind = "retryable"
	ErrorRateLimit      ErrorKind = "rate_limit"
	ErrorOverloaded     ErrorKind = "overloaded"
	ErrorAuthentication ErrorKind = "authentication"
	ErrorInvalidRequest ErrorKind = "invalid_request"
	ErrorSafetyRefusal  ErrorKind = "safety_refusal"
)

// Error has safe, normalized provider failure metadata. Its text deliberately
// excludes upstream response bodies, request content, and credentials.
type Error struct {
	Kind      ErrorKind
	Status    int
	RequestID string
	Err       error
}

func (e *Error) Error() string { return fmt.Sprintf("provider request failed (%s)", e.Kind) }
func (e *Error) Unwrap() error { return e.Err }

// IsSuccessfulCompletion reports the only provider terminal state that may be
// committed as a completed gateway response.
func IsSuccessfulCompletion(status string) bool { return status == "completed" }

// UnsuccessfulCompletionError keeps an unacceptable terminal state safe for
// callers while leaving it retryable until stream output becomes visible.
func UnsuccessfulCompletionError(requestID string) *Error {
	return &Error{Kind: ErrorRetryable, RequestID: requestID, Err: errors.New("provider did not complete successfully")}
}

// UnsupportedFeatureError reports a portable feature the concrete model does
// not support. It is intentionally separate from transport/provider errors.
type UnsupportedFeatureError struct{ Feature string }

func (e *UnsupportedFeatureError) Error() string {
	return fmt.Sprintf("unsupported provider feature: %s", e.Feature)
}
