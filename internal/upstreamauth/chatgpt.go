// Package upstreamauth carries request-scoped provider credentials without
// placing them in portable inference, routing, or persistence types.
package upstreamauth

import (
	"context"
	"strings"
)

// ChatGPTCredential authorizes one request to a ChatGPT workspace.
type ChatGPTCredential struct {
	AccessToken string
	AccountID   string
	Originator  string
	UserAgent   string
}

type chatGPTContextKey struct{}

// WithChatGPT returns a child context containing a complete credential.
// Incomplete or malformed credentials are ignored.
func WithChatGPT(ctx context.Context, credential ChatGPTCredential) context.Context {
	if !validChatGPTCredential(credential) {
		return ctx
	}
	return context.WithValue(ctx, chatGPTContextKey{}, credential)
}

// ChatGPT returns the complete ChatGPT credential attached to ctx.
func ChatGPT(ctx context.Context) (ChatGPTCredential, bool) {
	credential, ok := ctx.Value(chatGPTContextKey{}).(ChatGPTCredential)
	return credential, ok && validChatGPTCredential(credential)
}

func validChatGPTCredential(credential ChatGPTCredential) bool {
	return credential.AccessToken != "" && strings.TrimSpace(credential.AccessToken) == credential.AccessToken &&
		!strings.ContainsAny(credential.AccessToken, " \t\r\n") && strings.TrimSpace(credential.AccountID) != ""
}
