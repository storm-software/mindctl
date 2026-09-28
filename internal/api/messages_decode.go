package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/storm-software/mindctl/internal/inference"
)

type DecodedMessages struct {
	Request          inference.Request
	RequiredProvider string
	// Native reports Anthropic-only content that is forwarded verbatim to
	// Anthropic and stripped for other providers.
	Native bool
}

type messagesWireRequest struct {
	Model      string            `json:"model"`
	System     json.RawMessage   `json:"system"`
	Messages   []json.RawMessage `json:"messages"`
	Tools      []json.RawMessage `json:"tools"`
	Stream     bool              `json:"stream"`
	MaxTokens  int64             `json:"max_tokens"`
	ToolChoice json.RawMessage   `json:"tool_choice"`
	Thinking   json.RawMessage   `json:"thinking"`
}

// Fields outside these sets have no portable meaning. They are accepted,
// forwarded unchanged to Anthropic, and dropped for other providers.
var (
	messagesPortableFields = []string{"model", "system", "messages", "tools", "stream", "max_tokens", "tool_choice", "thinking"}
	messagePortableFields  = []string{"role", "content"}
	toolPortableFields     = []string{"name", "description", "input_schema", "cache_control"}
)

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

// messagesNativeData is the Anthropic continuation payload attached to a
// portable item. Blocks are kept verbatim so unknown fields survive.
type messagesNativeData struct {
	Content       json.RawMessage            `json:"anthropic_content_block,omitempty"`
	Prefix        []json.RawMessage          `json:"anthropic_prefix,omitempty"`
	Suffix        []json.RawMessage          `json:"anthropic_suffix,omitempty"`
	MessageFields map[string]json.RawMessage `json:"anthropic_message_fields,omitempty"`
}

type messagesPendingItem struct {
	item   inference.Item
	native bool
	data   messagesNativeData
}

func DecodeMessagesRequest(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) (DecodedMessages, error) {
	if maxBodyBytes <= 0 {
		return DecodedMessages{}, inference.Invalid("body", "limit must be positive")
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		return DecodedMessages{}, decodeError(err)
	}
	var fields map[string]json.RawMessage
	if err := decodeStrict(raw, &fields); err != nil {
		return DecodedMessages{}, decodeError(err)
	}
	if fields == nil {
		return DecodedMessages{}, inference.Invalid("body", "must be a JSON object")
	}
	var wire messagesWireRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return DecodedMessages{}, decodeError(err)
	}
	if wire.MaxTokens <= 0 {
		return DecodedMessages{}, inference.Invalid("max_tokens", "must be positive")
	}
	decoded := DecodedMessages{Request: inference.Request{Model: wire.Model, Stream: wire.Stream, MaxOutputTokens: wire.MaxTokens}}
	if extra := unknownFields(fields, messagesPortableFields); extra != nil {
		decoded.Native = true
		decoded.Request.AnthropicExtra = extra
	}
	if string(wire.System) == "null" {
		return DecodedMessages{}, inference.Invalid("system", "cannot be null")
	}
	if len(wire.Thinking) > 0 {
		var thinking inference.ThinkingOptions
		if err := decodeStrict(wire.Thinking, &thinking); err != nil {
			return DecodedMessages{}, inference.Invalid("thinking", "must be an object with type, budget_tokens, or display")
		}
		switch {
		case thinking.Type != "enabled" && thinking.Type != "adaptive" && thinking.Type != "disabled":
			return DecodedMessages{}, inference.Invalid("thinking.type", fmt.Sprintf("unsupported thinking type %q", thinking.Type))
		case thinking.Type == "enabled" && thinking.BudgetTokens <= 0:
			return DecodedMessages{}, inference.Invalid("thinking.budget_tokens", "must be positive when thinking is enabled")
		case thinking.Type != "enabled" && thinking.BudgetTokens != 0:
			return DecodedMessages{}, inference.Invalid("thinking.budget_tokens", "is only supported when thinking is enabled")
		}
		if thinking.Type != "disabled" {
			decoded.Native = true
			decoded.Request.Thinking = &thinking
		}
	}
	if len(wire.System) > 0 {
		if wire.System[0] == '"' {
			if err := json.Unmarshal(wire.System, &decoded.Request.Instructions); err != nil {
				return DecodedMessages{}, inference.Invalid("system", "must contain text")
			}
		} else {
			var blocks []json.RawMessage
			if json.Unmarshal(wire.System, &blocks) != nil {
				return DecodedMessages{}, inference.Invalid("system", "must contain text blocks")
			}
			for _, raw := range blocks {
				var block messagesWireBlock
				if json.Unmarshal(raw, &block) != nil || block.Type != "text" || block.Text == "" {
					return DecodedMessages{}, inference.Invalid("system", "has unsupported native content")
				}
				if decoded.Request.Instructions != "" {
					decoded.Request.Instructions += "\n"
				}
				decoded.Request.Instructions += block.Text
				if len(block.CacheControl) != 0 {
					if !validCacheControl(block.CacheControl) {
						return DecodedMessages{}, inference.Invalid("system", "invalid cache control")
					}
					decoded.Native = true
				}
				native := inference.NativeSystemBlock{Type: "text", Text: block.Text, CacheControl: block.CacheControl}
				// Fields such as citations have no portable form; keep the block verbatim.
				if decodeStrict(raw, &messagesWireBlock{}) != nil {
					native.Raw = append(json.RawMessage(nil), raw...)
					decoded.Native = true
				}
				decoded.Request.AnthropicSystem = append(decoded.Request.AnthropicSystem, native)
			}
		}
	}
	for index, raw := range wire.Messages {
		items, native, err := decodeMessagesMessage(fmt.Sprintf("messages[%d]", index), raw)
		if err != nil {
			return DecodedMessages{}, err
		}
		if native {
			decoded.Native = true
		}
		decoded.Request.Input = append(decoded.Request.Input, items...)
	}
	for index, raw := range wire.Tools {
		tool, native, err := decodeMessagesTool(fmt.Sprintf("tools[%d]", index), raw)
		if err != nil {
			return DecodedMessages{}, err
		}
		if native {
			decoded.Native = true
		}
		decoded.Request.Tools = append(decoded.Request.Tools, tool)
	}
	if len(wire.ToolChoice) > 0 {
		var choice inference.NativeToolChoice
		if err := decodeStrict(wire.ToolChoice, &choice); err != nil || (choice.Type != "auto" && choice.Type != "any" && choice.Type != "none" && choice.Type != "tool") || (choice.Type == "tool" && choice.Name == "") || (choice.Type != "tool" && choice.Name != "") {
			return DecodedMessages{}, inference.Invalid("tool_choice", "unsupported tool choice")
		}
		switch {
		case choice.Type != "auto":
			// Dropping a forced or disabled tool choice would change behavior.
			decoded.RequiredProvider = "anthropic"
			decoded.Native = true
			decoded.Request.AnthropicToolChoice = &choice
		case choice.DisableParallelToolUse != nil:
			parallel := !*choice.DisableParallelToolUse
			decoded.Native = true
			decoded.Request.AnthropicToolChoice = &choice
			decoded.Request.ToolChoice, decoded.Request.ParallelToolCalls = "auto", &parallel
		default:
			decoded.Request.ToolChoice = "auto"
		}
	}
	if err := inference.ValidateRequest(decoded.Request); err != nil {
		return DecodedMessages{}, err
	}
	return decoded, nil
}

// decodeMessagesMessage translates one Messages turn into portable items. It
// reports whether the turn carries Anthropic-only content.
func decodeMessagesMessage(param string, raw json.RawMessage) ([]inference.Item, bool, error) {
	var fields map[string]json.RawMessage
	var message messagesWireMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || json.Unmarshal(raw, &message) != nil {
		return nil, false, inference.Invalid(param, "must be a message object")
	}
	switch message.Role {
	case "user", "assistant", "system":
	default:
		return nil, false, inference.Invalid(param+".role", fmt.Sprintf("unsupported message role %q", message.Role))
	}
	// Anthropic keeps system turns in place; other providers receive them as
	// portable system messages.
	nativeTurn := message.Role == "system"
	messageFields := unknownFields(fields, messagePortableFields)
	if messageFields != nil {
		nativeTurn = true
	}
	blocks, err := messagesContentBlocks(message.Content)
	if err != nil {
		return nil, false, inference.Invalid(param+".content", err.Error())
	}
	// Clients replay an empty assistant turn when a model returned no content.
	// It carries nothing, and neighbouring turns of one role merge upstream.
	if len(blocks) == 0 {
		return nil, false, nil
	}
	var pending []messagesPendingItem
	var prefix []json.RawMessage
	for index, raw := range blocks {
		blockParam := fmt.Sprintf("%s.content[%d]", param, index)
		var block messagesWireBlock
		if json.Unmarshal(raw, &block) != nil || block.Type == "" {
			return nil, false, inference.Invalid(blockParam, "must be an object with a type")
		}
		native := message.Role == "system" || block.CacheControl != nil || decodeStrict(raw, &messagesWireBlock{}) != nil
		if message.Role == "system" && block.Type != "text" {
			return nil, false, inference.Invalid(blockParam, fmt.Sprintf("system messages support only text blocks, got %q", block.Type))
		}
		var item inference.Item
		switch block.Type {
		case "thinking", "redacted_thinking":
			if message.Role != "assistant" {
				return nil, false, inference.Invalid(blockParam, block.Type+" blocks are only valid in assistant messages")
			}
			if block.Type == "thinking" && block.Signature == "" {
				return nil, false, inference.Invalid(blockParam, "thinking blocks require a signature")
			}
			nativeTurn = true
			prefix = append(prefix, raw)
			continue
		case "text":
			if block.Text == "" {
				return nil, false, inference.Invalid(blockParam, "text cannot be empty")
			}
			item = inference.Item{Type: "message", Role: message.Role, Text: block.Text}
		case "image":
			if message.Role != "user" {
				return nil, false, inference.Invalid(blockParam, "images are only valid in user messages")
			}
			imageURL, err := messagesImageURL(block.Source)
			if err != nil {
				return nil, false, inference.Invalid(blockParam+".source", err.Error())
			}
			item = inference.Item{Type: "input_image", Role: message.Role, ImageURL: imageURL}
		case "tool_use":
			if message.Role != "assistant" || block.ID == "" || block.Name == "" || !json.Valid(block.Input) {
				return nil, false, inference.Invalid(blockParam, "tool_use requires an assistant message with id, name, and input")
			}
			item = inference.Item{Type: "function_call", CallID: block.ID, Name: block.Name, Arguments: block.Input}
		case "tool_result":
			if message.Role != "user" || block.ToolUseID == "" {
				return nil, false, inference.Invalid(blockParam, "tool_result requires a user message with tool_use_id")
			}
			output, portable, err := messagesToolResultOutput(block.Content)
			if err != nil {
				return nil, false, inference.Invalid(blockParam+".content", err.Error())
			}
			native = native || !portable
			item = inference.Item{Type: "function_call_output", CallID: block.ToolUseID, Output: output}
		default:
			// Blocks such as server_tool_use, tool_search_tool_result, and
			// document have no portable form. They ride along with an adjacent
			// item and are dropped for other providers.
			nativeTurn = true
			prefix = append(prefix, raw)
			continue
		}
		nativeTurn = nativeTurn || native
		pending = append(pending, messagesPendingItem{item: item, native: native || len(prefix) > 0, data: messagesNativeData{Content: raw, Prefix: prefix}})
		prefix = nil
	}
	if len(pending) == 0 {
		return nil, false, inference.Invalid(param+".content", "must include a text, image, tool_use, or tool_result block")
	}
	last := &pending[len(pending)-1]
	if len(prefix) > 0 {
		last.data.Suffix, last.native = prefix, true
	}
	if messageFields != nil {
		pending[0].data.MessageFields, pending[0].native = messageFields, true
	}
	items := make([]inference.Item, len(pending))
	for index, entry := range pending {
		items[index] = entry.item
		if entry.native {
			items[index].ContinuationProvider = "anthropic"
			items[index].ProviderData, _ = json.Marshal(entry.data)
		}
	}
	return items, nativeTurn, nil
}

func messagesContentBlocks(content json.RawMessage) ([]json.RawMessage, error) {
	var text string
	if string(bytes.TrimSpace(content)) != "null" && json.Unmarshal(content, &text) == nil {
		if text == "" {
			return nil, nil
		}
		block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		return []json.RawMessage{block}, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil || blocks == nil {
		return nil, errors.New("must be a string or an array of content blocks")
	}
	return blocks, nil
}

func messagesImageURL(raw json.RawMessage) (json.RawMessage, error) {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := decodeStrict(raw, &source); err != nil {
		return nil, errors.New("invalid image source")
	}
	imageURL := source.URL
	if source.Type == "base64" && source.MediaType != "" && source.Data != "" {
		imageURL = "data:" + source.MediaType + ";base64," + source.Data
	} else if source.Type != "url" || imageURL == "" {
		return nil, fmt.Errorf("unsupported image source type %q", source.Type)
	}
	encoded, _ := json.Marshal(map[string]string{"url": imageURL})
	return encoded, nil
}

// messagesToolResultOutput returns the portable text of a tool result and
// whether that text fully represents it.
func messagesToolResultOutput(content json.RawMessage) (json.RawMessage, bool, error) {
	if len(content) == 0 {
		return json.RawMessage(`""`), true, nil
	}
	var output string
	if json.Unmarshal(content, &output) == nil {
		return append(json.RawMessage(nil), content...), true, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil, false, errors.New("must be a string or an array of content blocks")
	}
	portable := true
	for index, raw := range blocks {
		var block messagesWireBlock
		if json.Unmarshal(raw, &block) != nil || block.Type == "" {
			return nil, false, fmt.Errorf("block %d must be an object with a type", index)
		}
		// tool_reference, image, and other blocks are forwarded natively.
		if block.Type != "text" || block.CacheControl != nil || decodeStrict(raw, &messagesWireBlock{}) != nil {
			portable = false
		}
		output += block.Text
	}
	encoded, _ := json.Marshal(output)
	return encoded, portable, nil
}

func decodeMessagesTool(param string, raw json.RawMessage) (inference.Tool, bool, error) {
	var fields map[string]json.RawMessage
	var tool messagesWireTool
	if json.Unmarshal(raw, &fields) != nil || fields == nil || json.Unmarshal(raw, &tool) != nil {
		return inference.Tool{}, false, inference.Invalid(param, "must be a tool object")
	}
	if tool.Name == "" {
		return inference.Tool{}, false, inference.Invalid(param+".name", "is required")
	}
	// Anthropic server tools such as tool search carry a type and no schema;
	// other providers do not receive them.
	hasNativeFields := unknownFields(fields, toolPortableFields) != nil
	native := hasNativeFields
	if len(tool.InputSchema) == 0 && !native {
		return inference.Tool{}, false, inference.Invalid(param+".input_schema", "is required")
	}
	if len(tool.InputSchema) != 0 && !json.Valid(tool.InputSchema) {
		return inference.Tool{}, false, inference.Invalid(param+".input_schema", "must be valid JSON")
	}
	if len(tool.CacheControl) != 0 {
		if !validCacheControl(tool.CacheControl) {
			return inference.Tool{}, false, inference.Invalid(param+".cache_control", "invalid cache control")
		}
		native = true
	}
	converted := inference.Tool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema, CacheControl: tool.CacheControl}
	if hasNativeFields {
		converted.AnthropicNative = append(json.RawMessage(nil), raw...)
	}
	return converted, native, nil
}

// unknownFields returns fields outside known, or nil when there are none.
func unknownFields(fields map[string]json.RawMessage, known []string) map[string]json.RawMessage {
	var extra map[string]json.RawMessage
	for name, value := range fields {
		if !slices.Contains(known, name) {
			if extra == nil {
				extra = make(map[string]json.RawMessage)
			}
			extra[name] = value
		}
	}
	return extra
}

// validCacheControl checks the documented fields and tolerates newer ones,
// which Anthropic validates when the raw value is forwarded.
func validCacheControl(raw json.RawMessage) bool {
	var value struct {
		Type string `json:"type"`
		TTL  string `json:"ttl"`
	}
	return json.Unmarshal(raw, &value) == nil && value.Type == "ephemeral" && (value.TTL == "" || value.TTL == "5m" || value.TTL == "1h")
}
