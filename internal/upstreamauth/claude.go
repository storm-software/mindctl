package upstreamauth

import (
	"context"
	"strings"
)

const ClaudeTokenHeader = "X-Mindctl-Claude-Token"

// ClaudeCredential authorizes one request against a Claude subscription.
type ClaudeCredential struct {
	AccessToken string
}

type claudeContextKey struct{}

// WithClaude returns a child context containing a valid credential.
func WithClaude(ctx context.Context, credential ClaudeCredential) context.Context {
	if !validClaudeCredential(credential) {
		return ctx
	}
	return context.WithValue(ctx, claudeContextKey{}, credential)
}

// Claude returns the Claude credential attached to ctx.
func Claude(ctx context.Context) (ClaudeCredential, bool) {
	credential, ok := ctx.Value(claudeContextKey{}).(ClaudeCredential)
	return credential, ok && validClaudeCredential(credential)
}

func validClaudeCredential(credential ClaudeCredential) bool {
	return credential.AccessToken != "" && strings.TrimSpace(credential.AccessToken) == credential.AccessToken &&
		!strings.ContainsAny(credential.AccessToken, " \t\r\n")
}
