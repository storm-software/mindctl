// Package upstreamauth carries request-scoped provider credentials without
// placing them in portable inference, routing, or persistence types.
package upstreamauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// ReadCodexAuthFile loads the ChatGPT login Codex keeps in auth.json.
func ReadCodexAuthFile(path string) (ChatGPTCredential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ChatGPTCredential{}, err
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return ChatGPTCredential{}, err
	}
	credential := ChatGPTCredential{AccessToken: auth.Tokens.AccessToken, AccountID: auth.Tokens.AccountID}
	if !validChatGPTCredential(credential) {
		return ChatGPTCredential{}, errors.New("codex auth file has no ChatGPT login")
	}
	return credential, nil
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
