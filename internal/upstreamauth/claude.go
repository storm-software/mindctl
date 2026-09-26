package upstreamauth

import (
	"context"
	"strings"
)

const ClaudeTokenHeader = "X-Mindctl-Claude-Token"

// ClaudeCredential authorizes one request against a Claude subscription.
type ClaudeCredential struct {
	AccessToken string
	Beta        string
	Version     string
}

type claudeContextKey struct{}

// WithClaude returns a child context containing a valid credential.
func WithClaude(ctx context.Context, credential ClaudeCredential) context.Context {
	if !validClaudeCredential(credential) {
		return ctx
	}
	return context.WithValue(ctx, claudeContextKey{}, credential)
}

// WithoutClaude prevents non-Anthropic adapters from accessing a captured
// subscription credential while preserving cancellation and deadlines.
func WithoutClaude(ctx context.Context) context.Context {
	return context.WithValue(ctx, claudeContextKey{}, nil)
}

// Claude returns the Claude credential attached to ctx.
func Claude(ctx context.Context) (ClaudeCredential, bool) {
	credential, ok := ctx.Value(claudeContextKey{}).(ClaudeCredential)
	return credential, ok && validClaudeCredential(credential)
}

func validClaudeCredential(credential ClaudeCredential) bool {
	return credential.AccessToken != "" && strings.TrimSpace(credential.AccessToken) == credential.AccessToken &&
		!strings.ContainsAny(credential.AccessToken, " \t\r\n") && validProtocolValue(credential.Beta) &&
		validProtocolValue(credential.Version) && !strings.ContainsAny(credential.Version, ", \t")
}

func validProtocolValue(value string) bool {
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}
