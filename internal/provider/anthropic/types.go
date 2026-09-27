package anthropic

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
)

type messagesRequest struct {
	Model        string                      `json:"model"`
	System       any                         `json:"system,omitempty"`
	Messages     []messagesMessage           `json:"messages"`
	Tools        []messagesTool              `json:"tools,omitempty"`
	Thinking     *inference.ThinkingOptions  `json:"thinking,omitempty"`
	ToolChoice   *inference.NativeToolChoice `json:"tool_choice,omitempty"`
	OutputConfig *messagesOutputConfig       `json:"output_config,omitempty"`
	Stream       bool                        `json:"stream,omitempty"`
	MaxTokens    int64                       `json:"max_tokens,omitempty"`
	// Extra holds caller fields forwarded verbatim; typed fields take precedence.
	Extra map[string]json.RawMessage `json:"-"`
}

func (request messagesRequest) MarshalJSON() ([]byte, error) {
	type typedRequest messagesRequest
	return withExtraFields(typedRequest(request), request.Extra)
}

type messagesMessage struct {
	Role    string           `json:"role"`
	Content []messageContent `json:"content"`
	// Fields holds caller message fields, such as per-turn output_config.
	Fields map[string]json.RawMessage `json:"-"`
}

func (message messagesMessage) MarshalJSON() ([]byte, error) {
	type typedMessage messagesMessage
	return withExtraFields(typedMessage(message), message.Fields)
}

// withExtraFields encodes value and adds extra fields it does not already set.
func withExtraFields(value any, extra map[string]json.RawMessage) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(extra) == 0 {
		return encoded, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for name, raw := range extra {
		if _, typed := fields[name]; !typed {
			fields[name] = raw
		}
	}
	return json.Marshal(fields)
}

type messageContent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Source       *imageSource    `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// Raw, when set, is the caller's block and is encoded verbatim.
	Raw json.RawMessage `json:"-"`
}

func (content messageContent) MarshalJSON() ([]byte, error) {
	if len(content.Raw) != 0 {
		return content.Raw, nil
	}
	if content.Type == "thinking" {
		return json.Marshal(struct {
			Type         string          `json:"type"`
			Thinking     string          `json:"thinking"`
			Signature    string          `json:"signature"`
			CacheControl json.RawMessage `json:"cache_control,omitempty"`
		}{content.Type, content.Thinking, content.Signature, content.CacheControl})
	}
	type nativeContent messageContent
	return json.Marshal(nativeContent(content))
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type messagesTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// Native, when set, is the caller's tool definition and is encoded verbatim.
	Native json.RawMessage `json:"-"`
}

func (tool messagesTool) MarshalJSON() ([]byte, error) {
	if len(tool.Native) != 0 {
		return tool.Native, nil
	}
	type typedTool messagesTool
	return json.Marshal(typedTool(tool))
}

type messagesOutputConfig struct {
	Format messagesOutputFormat `json:"format"`
}

type messagesOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type messagesResponse struct {
	ID         string            `json:"id"`
	Model      string            `json:"model"`
	Role       string            `json:"role"`
	Content    []json.RawMessage `json:"content"`
	StopReason json.RawMessage   `json:"stop_reason"`
	Usage      *messagesUsage    `json:"usage"`
}

type messagesUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

// providerData is the continuation payload for one portable item. Blocks
// stay raw so fields this adapter does not model reach Anthropic unchanged.
type providerData struct {
	Content       json.RawMessage            `json:"anthropic_content_block,omitempty"`
	Prefix        []json.RawMessage          `json:"anthropic_prefix,omitempty"`
	Suffix        []json.RawMessage          `json:"anthropic_suffix,omitempty"`
	MessageFields map[string]json.RawMessage `json:"anthropic_message_fields,omitempty"`
}

func toMessagesRequest(model domain.Model, request inference.Request, stream bool) (messagesRequest, error) {
	if strings.TrimSpace(model.UpstreamID) == "" {
		return messagesRequest{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider upstream model is not configured")}
	}
	if err := validateCapabilities(model, request); err != nil {
		return messagesRequest{}, err
	}
	if err := inference.ValidateRequest(request); err != nil {
		return messagesRequest{}, err
	}

	maxTokens := request.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = model.MaxOutputTokens
	}
	result := messagesRequest{Model: model.UpstreamID, Stream: stream, MaxTokens: maxTokens, Thinking: request.Thinking, ToolChoice: request.AnthropicToolChoice, Extra: request.AnthropicExtra}
	system := request.Instructions
	for _, item := range request.Input {
		// Native system turns keep their position; portable ones are hoisted.
		if item.Type == "message" && item.Role == "system" && len(item.ProviderData) == 0 {
			system = joinSystem(system, item.Text)
			continue
		}
		message, err := toMessagesMessage(item)
		if err != nil {
			return messagesRequest{}, err
		}
		result.Messages = appendMessage(result.Messages, message)
	}
	if system != "" {
		result.System = system
	}
	if len(request.AnthropicSystem) > 0 {
		result.System = request.AnthropicSystem
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, messagesTool{Name: tool.Name, Description: tool.Description, InputSchema: append(json.RawMessage(nil), tool.Parameters...), CacheControl: append(json.RawMessage(nil), tool.CacheControl...), Native: append(json.RawMessage(nil), tool.AnthropicNative...)})
	}
	if format := request.TextFormat; format != nil {
		result.OutputConfig = &messagesOutputConfig{Format: messagesOutputFormat{Type: "json_schema", Schema: append(json.RawMessage(nil), format.Schema...)}}
	}
	return result, nil
}

func appendMessage(messages []messagesMessage, message messagesMessage) []messagesMessage {
	if len(messages) == 0 || messages[len(messages)-1].Role != message.Role || message.Fields != nil {
		return append(messages, message)
	}
	messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, message.Content...)
	return messages
}

func joinSystem(before, after string) string {
	if before == "" {
		return after
	}
	if after == "" {
		return before
	}
	return before + "\n" + after
}

func toMessagesMessage(item inference.Item) (messagesMessage, error) {
	data, hasData := contentFromProviderData(item.ProviderData)
	switch item.Type {
	case "message":
		if hasData && len(data.Content) == 0 {
			return data.message(item.Role, messageContent{}), nil
		}
		if content, ok := data.content(hasData, "text", map[string]any{"text": item.Text}); ok {
			return data.message(item.Role, content), nil
		}
		return messagesMessage{Role: item.Role, Content: []messageContent{{Type: "text", Text: item.Text}}}, nil
	case "input_image":
		if content, ok := data.content(hasData, "image", nil); ok {
			return data.message("user", content), nil
		}
		source, err := toImageSource(item.ImageURL)
		if err != nil {
			return messagesMessage{}, err
		}
		role := item.Role
		if role == "" {
			role = "user"
		}
		return messagesMessage{Role: role, Content: []messageContent{{Type: "image", Source: &source}}}, nil
	case "function_call":
		if content, ok := data.content(hasData, "tool_use", map[string]any{"id": item.CallID, "name": item.Name, "input": item.Arguments}); ok {
			return data.message("assistant", content), nil
		}
		return messagesMessage{Role: "assistant", Content: []messageContent{{Type: "tool_use", ID: item.CallID, Name: item.Name, Input: append(json.RawMessage(nil), item.Arguments...)}}}, nil
	case "function_call_output":
		if content, ok := data.content(hasData, "tool_result", map[string]any{"tool_use_id": item.CallID}); ok {
			return data.message("user", content), nil
		}
		return messagesMessage{Role: "user", Content: []messageContent{{Type: "tool_result", ToolUseID: item.CallID, Content: toolResultContent(item.Output)}}}, nil
	default:
		return messagesMessage{}, inference.Invalid("input", "has unsupported item type")
	}
}

func toolResultContent(raw json.RawMessage) json.RawMessage {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return append(json.RawMessage(nil), raw...)
	}
	encoded, err := json.Marshal(string(raw))
	if err != nil {
		return nil
	}
	return encoded
}

func toImageSource(raw json.RawMessage) (imageSource, error) {
	var input struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &input) != nil || strings.TrimSpace(input.URL) == "" {
		return imageSource{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("invalid image input")}
	}
	if !strings.HasPrefix(input.URL, "data:") {
		return imageSource{Type: "url", URL: input.URL}, nil
	}
	metadata, encoded, ok := strings.Cut(strings.TrimPrefix(input.URL, "data:"), ",")
	if !ok || !strings.HasSuffix(metadata, ";base64") {
		return imageSource{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("invalid image input")}
	}
	return imageSource{Type: "base64", MediaType: strings.TrimSuffix(metadata, ";base64"), Data: encoded}, nil
}

func contentFromProviderData(raw json.RawMessage) (providerData, bool) {
	if len(raw) == 0 {
		return providerData{}, false
	}
	var data providerData
	if json.Unmarshal(raw, &data) != nil || len(data.Content)+len(data.Prefix)+len(data.Suffix) == 0 {
		return providerData{}, false
	}
	return data, true
}

// content returns the stored block when it has the expected type, with the
// portable fields reapplied so gateway rewrites such as compression persist.
func (data providerData) content(ok bool, kind string, portable map[string]any) (messageContent, bool) {
	if !ok {
		return messageContent{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data.Content, &fields) != nil {
		return messageContent{}, false
	}
	for name, value := range portable {
		encoded, err := json.Marshal(value)
		if err != nil {
			return messageContent{}, false
		}
		fields[name] = encoded
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return messageContent{}, false
	}
	content, ok := rawContent(raw)
	return content, ok && content.Type == kind
}

func (data providerData) message(role string, content messageContent) messagesMessage {
	blocks := make([]messageContent, 0, len(data.Prefix)+1+len(data.Suffix))
	for _, raw := range data.Prefix {
		if block, ok := rawContent(raw); ok {
			blocks = append(blocks, block)
		}
	}
	if content.Type != "" {
		blocks = append(blocks, content)
	}
	for _, raw := range data.Suffix {
		if block, ok := rawContent(raw); ok {
			blocks = append(blocks, block)
		}
	}
	return messagesMessage{Role: role, Content: blocks, Fields: data.MessageFields}
}

func rawContent(raw json.RawMessage) (messageContent, bool) {
	var content messageContent
	if json.Unmarshal(raw, &content) != nil || content.Type == "" {
		return messageContent{}, false
	}
	content.Raw = append(json.RawMessage(nil), raw...)
	return content, true
}

func validateCapabilities(model domain.Model, request inference.Request) error {
	for _, item := range request.Input {
		switch item.Type {
		case "message":
			if !model.Capabilities.Text {
				return unsupported("text")
			}
		case "input_image":
			if !model.Capabilities.Images {
				return unsupported("images")
			}
		case "function_call", "function_call_output":
			if !model.Capabilities.Functions {
				return unsupported("functions")
			}
		}
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" {
			return unsupported(tool.Type)
		}
		if !model.Capabilities.Functions {
			return unsupported("functions")
		}
	}
	if request.TextFormat != nil && !model.Capabilities.JSONSchema {
		return unsupported("json_schema")
	}
	return nil
}

func unsupported(feature string) error { return &provider.UnsupportedFeatureError{Feature: feature} }

func fromMessagesResponse(response messagesResponse, model domain.Model, requestID string) inference.Result {
	result := inference.Result{ID: response.ID, Model: model.UpstreamID, ProviderRequestID: requestID, Status: statusForStop(response.StopReason), Usage: parseUsage(response.Usage)}
	_ = json.Unmarshal(response.StopReason, &result.StopReason)
	var prefix []json.RawMessage
	for _, raw := range response.Content {
		content, ok := rawContent(raw)
		if !ok {
			continue
		}
		data := providerData{Content: content.Raw, Prefix: prefix}
		switch content.Type {
		case "text":
			result.Output = append(result.Output, inference.Item{Type: "message", Role: "assistant", Text: content.Text, ProviderData: data.encode()})
			prefix = nil
		case "tool_use":
			result.Output = append(result.Output, inference.Item{Type: "function_call", CallID: content.ID, Name: content.Name, Arguments: append(json.RawMessage(nil), content.Input...), ProviderData: data.encode()})
			prefix = nil
		default:
			prefix = append(prefix, content.Raw)
		}
	}
	// Trailing native blocks, such as a server_tool_use before pause_turn,
	// stay with the last portable item, or with a textless carrier when the
	// response has no portable content.
	if last := len(result.Output) - 1; last >= 0 && len(prefix) > 0 {
		var data providerData
		if json.Unmarshal(result.Output[last].ProviderData, &data) == nil {
			data.Suffix = prefix
			result.Output[last].ProviderData = data.encode()
		}
	} else if len(prefix) > 0 {
		result.Output = append(result.Output, inference.Item{Type: "message", Role: "assistant", ContinuationProvider: "anthropic", ProviderData: providerData{Suffix: prefix}.encode()})
	}
	return result
}

func (data providerData) encode() json.RawMessage {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return raw
}

func parseUsage(usage *messagesUsage) inference.Usage {
	if usage == nil || usage.InputTokens == nil || usage.OutputTokens == nil || *usage.InputTokens < 0 || *usage.OutputTokens < 0 {
		return inference.Usage{}
	}
	result := inference.Usage{InputTokens: *usage.InputTokens, OutputTokens: *usage.OutputTokens, Known: true}
	if usage.CacheReadInputTokens != nil {
		if *usage.CacheReadInputTokens < 0 {
			return inference.Usage{}
		}
		result.CachedInputTokens = *usage.CacheReadInputTokens
	}
	if usage.CacheCreationInputTokens != nil {
		if *usage.CacheCreationInputTokens < 0 {
			return inference.Usage{}
		}
		result.CacheWriteInputTokens = *usage.CacheCreationInputTokens
	}
	return result
}

func statusForStop(raw json.RawMessage) string {
	var reason string
	if json.Unmarshal(raw, &reason) != nil || reason == "" {
		return ""
	}
	if reason == "max_tokens" {
		return "incomplete"
	}
	return "completed"
}

func itemID(index int, content messageContent) string {
	if content.ID != "" {
		return content.ID
	}
	return strconv.Itoa(index)
}
