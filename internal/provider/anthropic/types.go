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
	Model        string                `json:"model"`
	System       string                `json:"system,omitempty"`
	Messages     []messagesMessage     `json:"messages"`
	Tools        []messagesTool        `json:"tools,omitempty"`
	OutputConfig *messagesOutputConfig `json:"output_config,omitempty"`
	Stream       bool                  `json:"stream,omitempty"`
	MaxTokens    int64                 `json:"max_tokens,omitempty"`
}

type messagesMessage struct {
	Role    string           `json:"role"`
	Content []messageContent `json:"content"`
}

type messageContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type messagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesOutputConfig struct {
	Format messagesOutputFormat `json:"format"`
}

type messagesOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type messagesResponse struct {
	ID         string           `json:"id"`
	Model      string           `json:"model"`
	Role       string           `json:"role"`
	Content    []messageContent `json:"content"`
	StopReason json.RawMessage  `json:"stop_reason"`
	Usage      *messagesUsage   `json:"usage"`
}

type messagesUsage struct {
	InputTokens          *int64 `json:"input_tokens"`
	OutputTokens         *int64 `json:"output_tokens"`
	CacheReadInputTokens *int64 `json:"cache_read_input_tokens"`
}

type providerData struct {
	Content messageContent   `json:"anthropic_content_block"`
	Prefix  []messageContent `json:"anthropic_prefix,omitempty"`
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
	result := messagesRequest{Model: model.UpstreamID, Stream: stream, MaxTokens: maxTokens}
	system := request.Instructions
	for _, item := range request.Input {
		if item.Type == "message" && item.Role == "system" {
			system = joinSystem(system, item.Text)
			continue
		}
		message, err := toMessagesMessage(item)
		if err != nil {
			return messagesRequest{}, err
		}
		result.Messages = appendMessage(result.Messages, message)
	}
	result.System = system
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, messagesTool{Name: tool.Name, Description: tool.Description, InputSchema: append(json.RawMessage(nil), tool.Parameters...)})
	}
	if format := request.TextFormat; format != nil {
		result.OutputConfig = &messagesOutputConfig{Format: messagesOutputFormat{Type: "json_schema", Schema: append(json.RawMessage(nil), format.Schema...)}}
	}
	return result, nil
}

func appendMessage(messages []messagesMessage, message messagesMessage) []messagesMessage {
	if len(messages) == 0 || messages[len(messages)-1].Role != message.Role {
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
	switch item.Type {
	case "message":
		content := messageContent{Type: "text", Text: item.Text}
		if data, ok := contentFromProviderData(item.ProviderData); ok && data.Content.Type == "text" {
			content = data.Content
			content.Text = item.Text
			return messagesMessage{Role: item.Role, Content: append(data.Prefix, content)}, nil
		}
		return messagesMessage{Role: item.Role, Content: []messageContent{content}}, nil
	case "input_image":
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
		content := messageContent{Type: "tool_use", ID: item.CallID, Name: item.Name, Input: append(json.RawMessage(nil), item.Arguments...)}
		if data, ok := contentFromProviderData(item.ProviderData); ok && data.Content.Type == "tool_use" {
			content = data.Content
			content.ID, content.Name, content.Input = item.CallID, item.Name, append(json.RawMessage(nil), item.Arguments...)
			return messagesMessage{Role: "assistant", Content: append(data.Prefix, content)}, nil
		}
		return messagesMessage{Role: "assistant", Content: []messageContent{content}}, nil
	case "function_call_output":
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
	if json.Unmarshal(raw, &data) != nil || data.Content.Type == "" {
		return providerData{}, false
	}
	return data, true
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
	prefix := make([]messageContent, 0)
	for _, content := range response.Content {
		switch content.Type {
		case "text":
			result.Output = append(result.Output, inference.Item{Type: "message", Role: "assistant", Text: content.Text, ProviderData: makeProviderData(content, prefix)})
			prefix = nil
		case "tool_use":
			result.Output = append(result.Output, inference.Item{Type: "function_call", CallID: content.ID, Name: content.Name, Arguments: append(json.RawMessage(nil), content.Input...), ProviderData: makeProviderData(content, prefix)})
			prefix = nil
		default:
			prefix = append(prefix, content)
		}
	}
	return result
}

func makeProviderData(content messageContent, prefix []messageContent) json.RawMessage {
	raw, err := json.Marshal(providerData{Content: content, Prefix: prefix})
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
