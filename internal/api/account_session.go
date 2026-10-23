package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
)

// applyAccountSession binds a named model to an account. The routing-session
// model pin is a different key and stays on the automatic path.
func applyAccountSession(r *http.Request, input *executor.Input) {
	if r == nil || input == nil {
		return
	}
	key, parent, guardian := accountSession(r, input.ClientID, input.Request.Model)
	input.AccountSessionKey = key
	input.ParentSessionKey = parent
	input.Guardian = guardian
}

func accountSession(r *http.Request, clientID, model string) (string, string, bool) {
	if r == nil || model == "" || model == inference.AutomaticModel {
		return "", "", false
	}
	key := digestAccountSession(clientID, clientSessionID(r, ""))
	parentID := ""
	if value, ok := repeatedProtocolHeader(r, "X-Mindctl-Parent-Session-Id"); ok {
		parentID = normalizeClientSessionID(value)
	}
	guardian := false
	if value, ok := repeatedProtocolHeader(r, "X-Openai-Subagent"); ok {
		guardian = strings.EqualFold(value, "guardian")
	}
	return key, digestAccountSession(clientID, parentID), guardian
}

func digestAccountSession(clientID, sessionID string) string {
	if clientID == "" || sessionID == "" {
		return ""
	}
	digest := sha256.New()
	for _, part := range [...]string{"mindctl-account-session-v1", clientID, sessionID} {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}
