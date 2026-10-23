package accounts

import "strings"

// ProtocolFamily groups wire protocols that share an adapter.
// An empty protocol has no family.
func ProtocolFamily(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai", "openai-compatible", "codex", "openrouter":
		return "openai"
	case "anthropic":
		return "anthropic"
	case "gemini", "antigravity", "aistudio", "vertex":
		return "gemini"
	case "":
		return ""
	default:
		return strings.ToLower(protocol)
	}
}

// Resolve maps a client model name through aliases.
// The bool is false when the name is not an alias.
func Resolve(aliases []Alias, requested string) (modelID, provider string, ok bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", "", false
	}
	for _, alias := range aliases {
		if alias.Alias != requested || alias.Name == "" {
			continue
		}
		return alias.Name, alias.Provider, true
	}
	return requested, "", false
}
