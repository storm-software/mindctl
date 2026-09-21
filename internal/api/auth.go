package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
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

// Authenticate validates a gateway bearer token before calling next. Tokens
// are compared as fixed-size hashes, and every configured token is checked.
func Authenticate(next http.Handler, tokens TokenSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		presented, ok := strings.CutPrefix(authorization, "Bearer ")
		if !ok || presented == "" {
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
