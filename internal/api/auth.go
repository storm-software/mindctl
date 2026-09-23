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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(header)
		if len(values) != 1 {
			WriteError(w, ErrUnauthorized)
			return
		}
		presented := values[0]
		if strings.EqualFold(header, "Authorization") {
			fields := strings.Fields(presented)
			if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
				WriteError(w, ErrUnauthorized)
				return
			}
			presented = fields[1]
		}
		if presented == "" {
			WriteError(w, ErrUnauthorized)
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
			WriteError(w, ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIDContextKey{}, matchedID)))
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
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
