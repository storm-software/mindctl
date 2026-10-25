package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/storm-software/mindctl/internal/upstreamauth"
)

// Token is one configured gateway client credential and its stable client ID.
type Token struct {
	ID, Value string
}

// Verifier resolves a presented credential to a stable client ID. It fails
// with ErrUnauthorized for a credential it does not accept, or with
// ErrAuthUnavailable when it cannot decide.
type Verifier interface {
	Verify(ctx context.Context, presented string) (string, error)
}

// StaticTokens accepts configured tokens. Tokens are compared as fixed-size
// hashes, and every configured token is checked.
type StaticTokens []Token

func (tokens StaticTokens) Verify(_ context.Context, presented string) (string, error) {
	presentedHash := sha256.Sum256([]byte(presented))
	matchedID := ""
	for _, token := range tokens {
		tokenHash := sha256.Sum256([]byte(token.Value))
		matches := subtle.ConstantTimeCompare(presentedHash[:], tokenHash[:])
		if matches == 1 && matchedID == "" && token.ID != "" && token.Value != "" {
			matchedID = token.ID
		}
	}
	if matchedID == "" {
		return "", ErrUnauthorized
	}
	return matchedID, nil
}

// Verifiers accepts a credential that any member accepts, trying them in
// order. An unavailable member is reported only when none accepts it.
type Verifiers []Verifier

func (verifiers Verifiers) Verify(ctx context.Context, presented string) (string, error) {
	err := ErrUnauthorized
	for _, verifier := range verifiers {
		id, verifyErr := verifier.Verify(ctx, presented)
		if verifyErr == nil {
			return id, nil
		}
		if !errors.Is(verifyErr, ErrUnauthorized) {
			err = verifyErr
		}
	}
	return "", err
}

type clientIDContextKey struct{}

// ClientID returns the authenticated client identity, if the request passed
// through Authenticate.
func ClientID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(clientIDContextKey{}).(string)
	return id, ok
}

// Authenticate validates a gateway credential before calling next.
// Authorization uses Bearer syntax; dedicated headers carry the raw token.
func Authenticate(next http.Handler, header string, verifier Verifier) http.Handler {
	return AuthenticateWithError(next, header, verifier, WriteError)
}

// AuthenticateWithError validates the gateway credential and writes failures
// using the endpoint's error envelope.
func AuthenticateWithError(next http.Handler, header string, verifier Verifier, writeError func(http.ResponseWriter, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(header)
		if len(values) != 1 {
			writeError(w, ErrUnauthorized)
			return
		}
		presented := values[0]
		if strings.EqualFold(header, "Authorization") {
			fields := strings.Fields(presented)
			if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
				writeError(w, ErrUnauthorized)
				return
			}
			presented = fields[1]
		}
		if presented == "" {
			writeError(w, ErrUnauthorized)
			return
		}
		matchedID, err := verifier.Verify(r.Context(), presented)
		if err != nil {
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIDContextKey{}, matchedID)))
	})
}

// CaptureNativeClaudeOAuth captures the Messages client's bearer credential
// exclusively for the Anthropic OAuth adapter. Missing credentials are allowed
// so separately credentialed providers remain eligible.
func CaptureNativeClaudeOAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta, ok := repeatedProtocolHeader(r, "anthropic-beta")
		if !ok {
			writeMessagesError(w, ErrInvalidRequest)
			return
		}
		// API-key callers send x-api-key rather than Authorization but rely on
		// the same beta features.
		r = r.WithContext(upstreamauth.WithAnthropicBeta(r.Context(), beta))
		authorizations := r.Header.Values("Authorization")
		if len(authorizations) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if len(authorizations) != 1 {
			writeMessagesError(w, ErrUnauthorized)
			return
		}
		fields := strings.Fields(authorizations[0])
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || authorizations[0] != fields[0]+" "+fields[1] {
			writeMessagesError(w, ErrUnauthorized)
			return
		}
		version, ok := repeatedProtocolHeader(r, "anthropic-version")
		if !ok {
			writeMessagesError(w, ErrInvalidRequest)
			return
		}
		credential := upstreamauth.ClaudeCredential{AccessToken: fields[1], Beta: beta, Version: version}
		ctx := upstreamauth.WithClaude(r.Context(), credential)
		if _, ok := upstreamauth.Claude(ctx); !ok {
			writeMessagesError(w, ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// repeatedProtocolHeader returns a protocol header value. A compression/relay
// proxy in front of Mindctl (e.g. Headroom) may repeat one of these headers
// verbatim rather than folding it into a single value. That repetition carries
// no security weight, unlike Authorization, so only genuinely conflicting
// values are rejected.
func repeatedProtocolHeader(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", true
	}
	for _, value := range values[1:] {
		if value != values[0] {
			return "", false
		}
	}
	return values[0], true
}

// CaptureChatGPTOAuth attaches one complete caller-managed OAuth credential to
// the request context. Missing, malformed, or ambiguous credentials are left
// absent so request-local provider eligibility can fail closed.
func CaptureChatGPTOAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations := r.Header.Values("Authorization")
		accountIDs := r.Header.Values("ChatGPT-Account-Id")
		if len(authorizations) != 1 || len(accountIDs) != 1 {
			next.ServeHTTP(w, r)
			return
		}
		fields := strings.Fields(authorizations[0])
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			next.ServeHTTP(w, r)
			return
		}
		ctx := upstreamauth.WithChatGPT(r.Context(), upstreamauth.ChatGPTCredential{
			AccessToken: fields[1],
			AccountID:   accountIDs[0],
			Originator:  protocolHeader(r, "Originator"),
			UserAgent:   protocolHeader(r, "User-Agent"),
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// CaptureClaudeOAuth attaches one valid caller-managed OAuth credential to
// the request context. Invalid or ambiguous values fail closed in routing.
func CaptureClaudeOAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(upstreamauth.ClaudeTokenHeader)
		if len(values) != 1 {
			next.ServeHTTP(w, r)
			return
		}
		ctx := upstreamauth.WithClaude(r.Context(), upstreamauth.ClaudeCredential{AccessToken: values[0]})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// UseGatewayCredentials replaces caller-managed subscription credentials with
// ones this gateway holds, so remote clients need no login of their own. It
// runs after the capture middleware and keeps the caller's protocol headers.
// The Codex auth file is re-read per request to follow Codex's own refreshes;
// if it is unreadable, any caller credential is left in place.
func UseGatewayCredentials(next http.Handler, claudeToken, codexAuthFile string) http.Handler {
	if claudeToken == "" && codexAuthFile == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if claudeToken != "" {
			credential, _ := upstreamauth.Claude(ctx)
			credential.AccessToken = claudeToken
			ctx = upstreamauth.WithClaude(upstreamauth.WithoutClaude(ctx), credential)
		}
		if codexAuthFile != "" {
			if credential, err := upstreamauth.ReadCodexAuthFile(codexAuthFile); err == nil {
				credential.Originator = protocolHeader(r, "Originator")
				credential.UserAgent = protocolHeader(r, "User-Agent")
				ctx = upstreamauth.WithChatGPT(ctx, credential)
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func protocolHeader(r *http.Request, name string) string {
	values := r.Header.Values(name)
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] || strings.ContainsAny(values[0], "\r\n") {
		return ""
	}
	return values[0]
}
