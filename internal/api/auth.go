package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/storm-software/mindctl/internal/upstreamauth"
)

// Token is one configured gateway client credential and its stable client ID.
type Token struct {
	ID, Value string
}

// TokenSource permits static or rotated client-token sources without exposing
// token values to request handlers.
type TokenSource interface {
	Tokens() []Token
}

type clientIDContextKey struct{}

// ClientID returns the authenticated client identity, if the request passed
// through Authenticate.
func ClientID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(clientIDContextKey{}).(string)
	return id, ok
}

// Authenticate validates a gateway token before calling next. Authorization
// uses Bearer syntax; dedicated headers carry the raw token. Tokens are
// compared as fixed-size hashes, and every configured token is checked.
func Authenticate(next http.Handler, header string, tokens TokenSource) http.Handler {
	return AuthenticateWithError(next, header, tokens, WriteError)
}

// AuthenticateWithError validates the gateway credential and writes failures
// using the endpoint's error envelope.
func AuthenticateWithError(next http.Handler, header string, tokens TokenSource, writeError func(http.ResponseWriter, error)) http.Handler {
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
		presentedHash := sha256.Sum256([]byte(presented))
		matchedID := ""
		for _, token := range tokens.Tokens() {
			tokenHash := sha256.Sum256([]byte(token.Value))
			matches := subtle.ConstantTimeCompare(presentedHash[:], tokenHash[:])
			if matches == 1 && matchedID == "" && token.ID != "" {
				matchedID = token.ID
			}
		}
		if matchedID == "" {
			writeError(w, ErrUnauthorized)
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
		credential := upstreamauth.ClaudeCredential{AccessToken: fields[1]}
		for _, header := range []struct {
			name  string
			value *string
		}{{"anthropic-beta", &credential.Beta}, {"anthropic-version", &credential.Version}} {
			values := r.Header.Values(header.name)
			if len(values) > 1 {
				writeMessagesError(w, ErrInvalidRequest)
				return
			}
			if len(values) == 1 {
				*header.value = values[0]
			}
		}
		ctx := upstreamauth.WithClaude(r.Context(), credential)
		if _, ok := upstreamauth.Claude(ctx); !ok {
			writeMessagesError(w, ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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

func protocolHeader(r *http.Request, name string) string {
	values := r.Header.Values(name)
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] || strings.ContainsAny(values[0], "\r\n") {
		return ""
	}
	return values[0]
}
