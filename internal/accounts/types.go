// Package accounts selects a credential for a client-named model.
// The selector performs no network I/O and never retains secret material.
package accounts

import (
	"errors"
	"time"
)

const (
	StrategyRoundRobin Strategy = "round-robin"
	StrategyWeighted   Strategy = "weighted-round-robin"
	StrategyFillFirst  Strategy = "fill-first"

	RebindAny   RebindMode = "any"
	RebindQuota RebindMode = "quota"

	KindAPIKey         Kind = "api_key"
	KindOAuth          Kind = "oauth"
	KindPassthrough    Kind = "passthrough"
	KindServiceAccount Kind = "service_account"

	FailureAuth      FailureClass = "auth"
	FailureQuota     FailureClass = "quota"
	FailureTransient FailureClass = "transient"
	FailurePermanent FailureClass = "permanent"

	MaxWeight = 1_000_000

	DefaultStrategy           = StrategyRoundRobin
	DefaultRebind             = RebindAny
	DefaultRequestRetry       = 3
	DefaultSessionAffinityTTL = time.Hour
	DefaultAuthCooldown       = 5 * time.Minute
	DefaultQuotaCooldown      = time.Minute
	DefaultTransientCooldown  = time.Minute
)

// Strategy chooses among eligible accounts.
type Strategy string

// RebindMode decides when a session lets go of its account after a failure.
type RebindMode string

// Kind is how an account authenticates. It is not a secret.
type Kind string

// FailureClass is the cooldown class for one upstream failure.
type FailureClass string

// ErrNone means no eligible account is available for this attempt.
var ErrNone = errors.New("accounts: no eligible account")

// Settings is the account-routing policy. Zero values are not defaults;
// Normalize applies the defaults.
type Settings struct {
	Strategy               Strategy
	SessionAffinity        bool
	SessionAffinityTTL     time.Duration
	Subagents              bool
	RebindOn               RebindMode
	RequestRetry           int
	MaxRetryCredentials    int
	MaxRetryInterval       time.Duration
	DisableCooling         bool
	ForceModelPrefix       bool
	RequestRetryConfigured bool
	SubagentsConfigured    bool
}

// Normalize fills omitted settings. An explicit zero retry count is kept.
func (s Settings) Normalize() Settings {
	if s.Strategy == "" {
		s.Strategy = DefaultStrategy
	}
	if s.RebindOn == "" {
		s.RebindOn = DefaultRebind
	}
	if !s.RequestRetryConfigured {
		s.RequestRetry = DefaultRequestRetry
	}
	if s.SessionAffinityTTL == 0 {
		s.SessionAffinityTTL = DefaultSessionAffinityTTL
	}
	if !s.SubagentsConfigured {
		s.Subagents = true
	}
	return s
}

// Account is one routable credential without its secret.
type Account struct {
	ID, Provider, Protocol, BaseURL, Prefix, ProxyURL string
	Kind                                              Kind
	Weight, Priority, RequestRetry                    int
	ExcludedModels                                    []string
	Enabled                                           bool
	Configured                                        bool
	ClaudeProfile                                     bool
	ThinkingReplay                                    bool
	NativeToolSearch                                  bool
	RequestRetryConfigured                            bool
}

// Secret is the material used to call an account. It stays off the selector.
type Secret struct {
	APIKey, AccessToken, RefreshToken, AccountID string
	ClientEmail, PrivateKey, TokenURI            string
	Headers                                      map[string]string
}

// Alias maps a client-visible model name to an upstream model.
type Alias struct {
	Name, Alias, Provider string
}

// SelectRequest is one named-model selection.
type SelectRequest struct {
	ModelID, Provider, SessionKey, ParentSessionKey string
	Round                                           int
	Guardian                                        bool
	Tried                                           []string
}

// Selection is the account chosen for one attempt.
type Selection struct {
	Account Account
	Secret  Secret
	Round   int
}

// Report is the outcome of an attempt, used for cooldown and rebind.
type Report struct {
	SessionKey, AccountID string
	Class                 FailureClass
}

// UnavailableError reports that every eligible account is cooling down.
type UnavailableError struct {
	RetryAfter time.Duration
}

func (e *UnavailableError) Error() string { return ErrNone.Error() }
func (e *UnavailableError) Unwrap() error { return ErrNone }

// ClassFromStatus maps an upstream HTTP status to a cooldown class.
func ClassFromStatus(status int) FailureClass {
	switch status {
	case 401, 403:
		return FailureAuth
	case 402, 429:
		return FailureQuota
	case 408, 500, 502, 503, 504:
		return FailureTransient
	default:
		if status >= 520 && status <= 526 {
			return FailureTransient
		}
		if status >= 400 && status < 500 {
			return FailurePermanent
		}
		if status >= 500 {
			return FailureTransient
		}
		return FailurePermanent
	}
}
