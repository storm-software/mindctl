package api

import (
	"encoding/json"
	"net/http"

	"github.com/storm-software/mindctl/internal/inference"
)

type DecodedMessages struct {
	Request          inference.Request
	RequiredProvider string
}

type messagesWireRequest struct {
	Model      string                `json:"model"`
	System     json.RawMessage       `json:"system"`
	Messages   []messagesWireMessage `json:"messages"`
	Tools      []messagesWireTool    `json:"tools"`
	Stream     bool                  `json:"stream"`
	MaxTokens  int64                 `json:"max_tokens"`
	ToolChoice json.RawMessage       `json:"tool_choice"`
	Thinking   json.RawMessage       `json:"thinking"`
}

type messagesWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messagesWireBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Source       json.RawMessage `json:"source"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"`
	Thinking     string          `json:"thinking"`
	Signature    string          `json:"signature"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type messagesWireTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control"`
}

func DecodeMessagesRequest(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) (DecodedMessages, error) {
	if maxBodyBytes <= 0 {
		return DecodedMessages{}, inference.Invalid("body", "limit must be positive")
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer body.Close()
	var wire messagesWireRequest
	if err := decodeOne(body, &wire); err != nil {
		return DecodedMessages{}, decodeError(err)
	}
	if wire.MaxTokens <= 0 {
		return DecodedMessages{}, inference.Invalid("max_tokens", "must be positive")
	}
	decoded := DecodedMessages{Request: inference.Request{Model: wire.Model, Stream: wire.Stream, MaxOutputTokens: wire.MaxTokens}}
	if string(wire.System) == "null" {
		return DecodedMessages{}, inference.Invalid("system", "cannot be null")
	}
	if len(wire.Thinking) > 0 {
		var thinking inference.ThinkingOptions
		if err := decodeStrict(wire.Thinking, &thinking); err != nil || (thinking.Type != "enabled" && thinking.Type != "adaptive" && thinking.Type != "disabled") || (thinking.Type == "enabled" && thinking.BudgetTokens <= 0) || (thinking.Type != "enabled" && thinking.BudgetTokens != 0) {
			return DecodedMessages{}, inference.Invalid("thinking", "unsupported thinking configuration")
		}
		if thinking.Type != "disabled" {
			decoded.RequiredProvider = "anthropic"
			decoded.Request.Thinking = &thinking
		}
	}
	if len(wire.System) > 0 {
		if wire.System[0] == '"' {
			if err := json.Unmarshal(wire.System, &decoded.Request.Instructions); err != nil {
				return DecodedMessages{}, inference.Invalid("system", "must contain text")
			}
		} else {
			var blocks []messagesWireBlock
			if err := decodeStrict(wire.System, &blocks); err != nil {
				return DecodedMessages{}, inference.Invalid("system", "must contain text blocks")
			}
			for _, block := range blocks {
				if block.Type != "text" || block.Text == "" {
					return DecodedMessages{}, inference.Invalid("system", "has unsupported native content")
				}
				decoded.Request.Instructions += block.Text
				if len(block.CacheControl) != 0 {
					if !validCacheControl(block.CacheControl) {
						return DecodedMessages{}, inference.Invalid("system", "invalid cache control")
					}
					decoded.RequiredProvider = "anthropic"
				}
				decoded.Request.AnthropicSystem = append(decoded.Request.AnthropicSystem, inference.NativeSystemBlock{Type: "text", Text: block.Text, CacheControl: block.CacheControl})
			}
		}
	}
	for _, message := range wire.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return DecodedMessages{}, inference.Invalid("messages.role", "must be user or assistant")
		}
		var blocks []messagesWireBlock
		if err := decodeStrict(message.Content, &blocks); err != nil {
			var text string
			if json.Unmarshal(message.Content, &text) != nil || text == "" {
				return DecodedMessages{}, inference.Invalid("messages.content", "must be text or supported blocks")
			}
			blocks = []messagesWireBlock{{Type: "text", Text: text}}
		}
		if len(blocks) == 0 {
			return DecodedMessages{}, inference.Invalid("messages.content", "must not be empty")
		}
		var nativePrefix []messagesWireBlock
		for _, block := range blocks {
			if block.CacheControl != nil {
				decoded.RequiredProvider = "anthropic"
			}
			switch block.Type {
			case "thinking":
				if message.Role != "assistant" || block.Signature == "" {
					return DecodedMessages{}, inference.Invalid("messages.content", "signed thinking is required")
				}
				decoded.RequiredProvider = "anthropic"
				nativePrefix = append(nativePrefix, block)
				continue
			case "text":
				if block.Text == "" {
					return DecodedMessages{}, inference.Invalid("messages.content", "text cannot be empty")
				}
				item := inference.Item{Type: "message", Role: message.Role, Text: block.Text}
				attachNativeContent(&item, block, nativePrefix, decoded.RequiredProvider)
				decoded.Request.Input = append(decoded.Request.Input, item)
			case "image":
				if len(nativePrefix) > 0 || message.Role != "user" || block.CacheControl != nil {
					return DecodedMessages{}, inference.Invalid("messages.content", "cannot translate native image block")
				}
				var source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
					URL       string `json:"url"`
				}
				if err := decodeStrict(block.Source, &source); err != nil {
					return DecodedMessages{}, inference.Invalid("messages.content", "invalid image source")
				}
				imageURL := source.URL
				if source.Type == "base64" && source.MediaType != "" && source.Data != "" {
					imageURL = "data:" + source.MediaType + ";base64," + source.Data
				} else if source.Type != "url" || imageURL == "" {
					return DecodedMessages{}, inference.Invalid("messages.content", "unsupported image source")
				}
				encoded, _ := json.Marshal(map[string]string{"url": imageURL})
				decoded.Request.Input = append(decoded.Request.Input, inference.Item{Type: "input_image", Role: message.Role, ImageURL: encoded})
			case "tool_use":
				if message.Role != "assistant" || block.ID == "" || block.Name == "" || !json.Valid(block.Input) {
					return DecodedMessages{}, inference.Invalid("messages.content", "invalid tool use")
				}
				item := inference.Item{Type: "function_call", CallID: block.ID, Name: block.Name, Arguments: block.Input}
				attachNativeContent(&item, block, nativePrefix, decoded.RequiredProvider)
				decoded.Request.Input = append(decoded.Request.Input, item)
			case "tool_result":
				if len(nativePrefix) > 0 || message.Role != "user" || block.ToolUseID == "" || len(block.Content) == 0 || block.CacheControl != nil {
					return DecodedMessages{}, inference.Invalid("messages.content", "invalid tool result")
				}
				var output string
				if json.Unmarshal(block.Content, &output) != nil {
					var resultBlocks []messagesWireBlock
					if decodeStrict(block.Content, &resultBlocks) != nil || len(resultBlocks) == 0 {
						return DecodedMessages{}, inference.Invalid("messages.content", "unsupported tool result")
					}
					for _, resultBlock := range resultBlocks {
						if resultBlock.Type != "text" || resultBlock.CacheControl != nil {
							return DecodedMessages{}, inference.Invalid("messages.content", "unsupported tool result block")
						}
						output += resultBlock.Text
					}
				}
				encoded, _ := json.Marshal(output)
				decoded.Request.Input = append(decoded.Request.Input, inference.Item{Type: "function_call_output", CallID: block.ToolUseID, Output: encoded})
			default:
				return DecodedMessages{}, inference.Invalid("messages.content", "unsupported content block")
			}
			nativePrefix = nil
		}
		if len(nativePrefix) > 0 {
			return DecodedMessages{}, inference.Invalid("messages.content", "trailing native block cannot be translated")
		}
	}
	for _, tool := range wire.Tools {
		if tool.Name == "" || !json.Valid(tool.InputSchema) {
			return DecodedMessages{}, inference.Invalid("tools", "invalid tool schema")
		}
		if len(tool.CacheControl) != 0 {
			if !validCacheControl(tool.CacheControl) {
				return DecodedMessages{}, inference.Invalid("tools", "invalid cache control")
			}
			decoded.RequiredProvider = "anthropic"
		}
		decoded.Request.Tools = append(decoded.Request.Tools, inference.Tool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema, CacheControl: tool.CacheControl})
	}
	if len(wire.ToolChoice) > 0 {
		var choice inference.NativeToolChoice
		if err := decodeStrict(wire.ToolChoice, &choice); err != nil || (choice.Type != "auto" && choice.Type != "any" && choice.Type != "none" && choice.Type != "tool") || (choice.Type == "tool" && choice.Name == "") || (choice.Type != "tool" && choice.Name != "") {
			return DecodedMessages{}, inference.Invalid("tool_choice", "unsupported tool choice")
		}
		if choice.Type != "auto" {
			decoded.RequiredProvider = "anthropic"
			decoded.Request.AnthropicToolChoice = &choice
		} else {
			decoded.Request.ToolChoice = "auto"
		}
	}
	if err := inference.ValidateRequest(decoded.Request); err != nil {
		return DecodedMessages{}, err
	}
	return decoded, nil
}

func validCacheControl(raw json.RawMessage) bool {
	var value struct {
		Type string `json:"type"`
		TTL  string `json:"ttl"`
	}
	return decodeStrict(raw, &value) == nil && value.Type == "ephemeral" && (value.TTL == "" || value.TTL == "5m" || value.TTL == "1h")
}

func attachNativeContent(item *inference.Item, block messagesWireBlock, prefix []messagesWireBlock, requiredProvider string) {
	if requiredProvider != "anthropic" || (len(prefix) == 0 && block.CacheControl == nil) {
		return
	}
	data, _ := json.Marshal(struct {
		Content messagesWireBlock   `json:"anthropic_content_block"`
		Prefix  []messagesWireBlock `json:"anthropic_prefix,omitempty"`
	}{block, prefix})
	item.ContinuationProvider = "anthropic"
	item.ProviderData = data
}
