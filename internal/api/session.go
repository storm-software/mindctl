package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/storm-software/mindctl/internal/inference"
)

// sessionHeaders are client session identifiers in precedence order after
// Claude Code's metadata.user_id: an explicit Mindctl header, Claude Code's
// header, then Codex's.
var sessionHeaders = [...]string{"X-Mindctl-Session-Id", "X-Claude-Code-Session-Id", "Session-Id"}

const (
	minClientSessionIDLength = 16
	maxClientSessionIDLength = 128
)

// clientSessionID returns the caller's own session identifier, or "" when none
// is usable. metadataUserID is the Messages metadata.user_id value; Claude
// Code sends a JSON object there that carries its session ID. Identifiers must
// be random enough that the derived session key reveals nothing about content.
func clientSessionID(r *http.Request, metadataUserID string) string {
	if strings.HasPrefix(metadataUserID, "{") {
		var metadata map[string]any
		if json.Unmarshal([]byte(metadataUserID), &metadata) == nil {
			for _, key := range [...]string{"session_id", "sessionId", "conversation_id", "conversationId"} {
				if value, ok := metadata[key].(string); ok {
					if id := normalizeClientSessionID(value); id != "" {
						return id
					}
				}
			}
		}
	}
	for _, name := range sessionHeaders {
		value, ok := repeatedProtocolHeader(r, name)
		if !ok {
			continue
		}
		if id := normalizeClientSessionID(value); id != "" {
			return id
		}
	}
	return ""
}

func normalizeClientSessionID(value string) string {
	if len(value) < minClientSessionIDLength || len(value) > maxClientSessionIDLength {
		return ""
	}
	for index := 0; index < len(value); index++ {
		if value[index] <= ' ' || value[index] > '~' {
			return ""
		}
	}
	return value
}

// sessionKey derives the opaque routing-session key. The first user message
// separates sub-agents that share their parent's session ID while staying
// stable across turns; instructions are excluded because clients rewrite
// them every turn. It returns "" when any component is missing.
func sessionKey(clientID, clientSessionID string, input []inference.Item) string {
	first := firstUserText(input)
	if clientID == "" || clientSessionID == "" || first == "" {
		return ""
	}
	digest := sha256.New()
	for _, part := range [...]string{"mindctl-session-v1", clientID, clientSessionID, first} {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// firstUserText joins the text of the leading run of user items, which is the
// whole first user message even when a client splits it into several blocks.
func firstUserText(input []inference.Item) string {
	var text strings.Builder
	for _, item := range input {
		if item.Role != "user" {
			break
		}
		for _, part := range append([]string{item.Text}, contentTexts(item.Content)...) {
			if part == "" {
				continue
			}
			text.WriteString(part)
			text.WriteByte(0)
		}
	}
	return text.String()
}

func contentTexts(parts []inference.ContentPart) []string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		texts = append(texts, part.Text)
	}
	return texts
}

// requestSessionKey returns the session key for a routed request that carries
// no gateway-owned continuation, or "" when sessions do not apply.
func requestSessionKey(cfg ResponsesConfig, r *http.Request, clientID string, request inference.Request, metadataUserID string) string {
	if !cfg.SessionEnabled || request.Model != inference.AutomaticModel || request.PreviousResponseID != "" {
		return ""
	}
	return sessionKey(clientID, clientSessionID(r, metadataUserID), request.Input)
}

// messagesMetadataUserID reads metadata.user_id from forwarded Messages fields
// without removing it.
func messagesMetadataUserID(extra map[string]json.RawMessage) string {
	var metadata struct {
		UserID string `json:"user_id"`
	}
	if raw, ok := extra["metadata"]; !ok || json.Unmarshal(raw, &metadata) != nil {
		return ""
	}
	return metadata.UserID
}
