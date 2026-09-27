package openai

import (
	"encoding/json"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

// metaProviderID identifies the Meta Responses-compatible endpoint, whose JSON
// Schema validator accepts a narrower regex dialect than OpenAI's.
const metaProviderID = "meta"

// providerCompatibleTool returns a tool whose schema the concrete provider's
// validator accepts. Only Meta needs a rewrite today.
func providerCompatibleTool(model domain.Model, tool inference.Tool) inference.Tool {
	if model.Provider != metaProviderID {
		return tool
	}
	if rewritten, changed := metaCompatibleSchema(tool.Parameters); changed {
		tool.Parameters = rewritten
	}
	for index, nested := range tool.Tools {
		tool.Tools[index] = providerCompatibleTool(model, nested)
	}
	return tool
}

// metaCompatibleSchema rewrites `pattern` keywords Meta rejects. The schema is
// re-encoded only when a pattern changed so untouched tools keep their bytes.
func metaCompatibleSchema(schema json.RawMessage) (json.RawMessage, bool) {
	if len(schema) == 0 {
		return schema, false
	}
	var decoded any
	if json.Unmarshal(schema, &decoded) != nil {
		return schema, false
	}
	if !rewriteSchemaPatterns(decoded) {
		return schema, false
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return schema, false
	}
	return encoded, true
}

func rewriteSchemaPatterns(node any) bool {
	changed := false
	switch value := node.(type) {
	case map[string]any:
		if pattern, ok := value["pattern"].(string); ok {
			if rewritten := metaCompatiblePattern(pattern); rewritten != pattern {
				value["pattern"], changed = rewritten, true
			}
		}
		for _, child := range value {
			if rewriteSchemaPatterns(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range value {
			if rewriteSchemaPatterns(child) {
				changed = true
			}
		}
	}
	return changed
}

// metaCompatiblePattern replaces the null octal escape `\0`, which Meta's
// validator rejects inside character classes, with the accepted `\x00`. A `\0`
// followed by another digit is a longer octal escape and is left alone.
func metaCompatiblePattern(pattern string) string {
	if !strings.Contains(pattern, `\0`) {
		return pattern
	}
	var builder strings.Builder
	for index := 0; index < len(pattern); index++ {
		if pattern[index] != '\\' || index+1 >= len(pattern) {
			builder.WriteByte(pattern[index])
			continue
		}
		next := pattern[index+1]
		if next == '0' && (index+2 >= len(pattern) || pattern[index+2] < '0' || pattern[index+2] > '9') {
			builder.WriteString(`\x00`)
		} else {
			builder.WriteByte('\\')
			builder.WriteByte(next)
		}
		index++
	}
	return builder.String()
}
