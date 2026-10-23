package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
)

// ChatAuthorizationHeader is the only credential Cursor's OpenAI base-URL
// override can send. Other routes keep the configured client header so their
// Authorization value can stay an upstream subscription token.
const ChatAuthorizationHeader = "Authorization"

type chatHandler struct {
	service ResponseExecutor
	config  ResponsesConfig
}

// NewChatHandler exposes the OpenAI Chat Completions subset Cursor sends when
// its OpenAI base URL points at the gateway. Authentication is composed outside
// this handler and must use Authorization.
func NewChatHandler(service ResponseExecutor, cfg ResponsesConfig) http.Handler {
	return &chatHandler{service: service, config: cloneResponsesConfig(cfg)}
}

func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		WriteError(w, ErrMethodNotAllowed)
		return
	}
	if h.service == nil {
		WriteError(w, errors.New("chat executor is unavailable"))
		return
	}
	clientID, ok := ClientID(r.Context())
	if !ok {
		WriteError(w, ErrUnauthorized)
		return
	}
	request, err := DecodeChatRequest(w, r, h.config.MaxBodyBytes)
	if err != nil {
		h.reject(w, err)
		return
	}
	request.SessionKey = requestSessionKey(h.config, r, clientID, request, "")
	minTier, maxTier := tierBounds(h.config, Controls{})
	input := executor.Input{
		ClientID: clientID, Request: request, Models: append([]domain.Model(nil), h.config.Models...),
		MinTier: minTier, MaxTier: maxTier, SafeFallbackTier: h.config.SafeFallbackTier,
		ProviderCredentials: providerCredentials(r.Context(), h.config), ProviderAvailability: cloneBools(h.config.ProviderAvailability),
		SavingsBaseline:      h.config.SavingsBaseline,
		ExpectedOutputTokens: h.config.ExpectedOutputTokens, Session: h.config.Session,
	}
	applyAccountSession(r, &input)
	if request.Stream {
		writer := &chatSSEWriter{response: w, created: time.Now().Unix()}
		if err := h.service.Stream(r.Context(), input, writer); err != nil && !writer.wrote {
			WriteError(w, err)
		}
		return
	}
	output, err := h.service.Execute(r.Context(), input)
	if err != nil {
		WriteError(w, err)
		return
	}
	setRoutingHeaders(w, executor.StreamMetadata{
		ResponseID: output.Result.ID, Model: output.Result.Model, Provider: output.Decision.Provider,
		DecisionID: output.AttemptID, Tier: output.Decision.Tier, Attempts: 1,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(chatCompletion(output.Result, time.Now().Unix()))
}

func (h *chatHandler) reject(w http.ResponseWriter, err error) {
	if h.config.Logger != nil {
		h.config.Logger.Debug("chat.request.rejected", "error", err.Error())
	}
	WriteError(w, err)
}

type chatWireRequest struct {
	Model               string            `json:"model"`
	Messages            []chatWireMessage `json:"messages"`
	Tools               []chatWireTool    `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	Stream              bool              `json:"stream"`
	MaxTokens           int64             `json:"max_tokens"`
	MaxCompletionTokens int64             `json:"max_completion_tokens"`
}

type chatWireMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolCalls    []chatWireCall  `json:"tool_calls"`
	ToolCallID   string          `json:"tool_call_id"`
	FunctionCall *chatWireFn     `json:"function_call"`
}

type chatWireCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function chatWireFn `json:"function"`
}

type chatWireFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatWireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict"`
	} `json:"function"`
}

// DecodeChatRequest accepts the Chat Completions fields Cursor sends. Unknown
// fields such as temperature are ignored: the gateway has nowhere to put them,
// and rejecting them would make Cursor's requests fail.
func DecodeChatRequest(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) (inference.Request, error) {
	if maxBodyBytes <= 0 {
		return inference.Request{}, inference.Invalid("body", "limit must be positive")
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		return inference.Request{}, decodeError(err)
	}
	var wire chatWireRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return inference.Request{}, decodeError(err)
	}
	if strings.TrimSpace(wire.Model) == "" {
		return inference.Request{}, inference.Invalid("model", "is required")
	}
	request := inference.Request{
		Model: wire.Model, Stream: wire.Stream, ParallelToolCalls: cloneBool(wire.ParallelToolCalls),
		MaxOutputTokens: wire.MaxTokens,
	}
	if wire.MaxCompletionTokens > 0 {
		request.MaxOutputTokens = wire.MaxCompletionTokens
	}
	if request.MaxOutputTokens < 0 {
		return inference.Request{}, inference.Invalid("max_tokens", "must not be negative")
	}
	choice, err := chatToolChoice(wire.ToolChoice)
	if err != nil {
		return inference.Request{}, err
	}
	request.ToolChoice = choice
	for index, message := range wire.Messages {
		items, err := chatMessageItems(index, message)
		if err != nil {
			return inference.Request{}, err
		}
		request.Input = append(request.Input, items...)
	}
	for index, tool := range wire.Tools {
		decoded, err := chatTool(index, tool)
		if err != nil {
			return inference.Request{}, err
		}
		request.Tools = append(request.Tools, decoded)
	}
	if err := inference.ValidateRequest(request); err != nil {
		return inference.Request{}, err
	}
	return request, nil
}

func chatToolChoice(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		switch choice {
		case "auto", "none", "required":
			return choice, nil
		default:
			return "", inference.Invalid("tool_choice", "must be auto, none, or required")
		}
	}
	return "", inference.Invalid("tool_choice", "named function choice is not supported")
}

func chatMessageItems(index int, message chatWireMessage) ([]inference.Item, error) {
	field := "messages[" + strconv.Itoa(index) + "]"
	text, images, err := chatContent(field, message.Content)
	if err != nil {
		return nil, err
	}
	role := message.Role
	if role != "system" && role != "user" && role != "assistant" && role != "tool" && role != "developer" {
		return nil, inference.Invalid(field+".role", "is invalid")
	}
	if role == "tool" {
		if message.ToolCallID == "" {
			return nil, inference.Invalid(field+".tool_call_id", "is required")
		}
		output, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		return []inference.Item{{Type: "function_call_output", CallID: message.ToolCallID, Output: output}}, nil
	}
	var items []inference.Item
	if text != "" || (len(images) == 0 && len(message.ToolCalls) == 0 && message.FunctionCall == nil) {
		if text == "" && len(images) == 0 {
			return nil, inference.Invalid(field+".content", "is required")
		}
		if text != "" {
			items = append(items, inference.Item{Type: "message", Role: role, Text: text})
		}
	}
	for _, image := range images {
		items = append(items, inference.Item{Type: "input_image", Role: role, ImageURL: image})
	}
	calls := message.ToolCalls
	if message.FunctionCall != nil && len(calls) == 0 {
		calls = []chatWireCall{{ID: "call_" + message.FunctionCall.Name, Type: "function", Function: *message.FunctionCall}}
	}
	for callIndex, call := range calls {
		if call.Type != "" && call.Type != "function" {
			return nil, inference.Invalid(field+".tool_calls["+strconv.Itoa(callIndex)+"].type", "must be function")
		}
		arguments, err := chatArguments(call.Function.Arguments)
		if err != nil {
			return nil, inference.Invalid(field+".tool_calls["+strconv.Itoa(callIndex)+"].function.arguments", "must be JSON")
		}
		id := call.ID
		if id == "" {
			id = "call_" + call.Function.Name
		}
		items = append(items, inference.Item{Type: "function_call", CallID: id, Name: call.Function.Name, Arguments: arguments})
	}
	if len(items) == 0 {
		return nil, inference.Invalid(field, "has no content")
	}
	return items, nil
}

func chatContent(field string, raw json.RawMessage) (string, []json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", nil, inference.Invalid(field+".content", "must contain text")
		}
		return text, nil, nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil, inference.Invalid(field+".content", "must be text or a content array")
	}
	var texts []string
	var images []json.RawMessage
	for partIndex, part := range parts {
		text, image, err := chatContentPart(field, partIndex, part)
		if err != nil {
			return "", nil, err
		}
		if text != "" {
			texts = append(texts, text)
		}
		if len(image) > 0 {
			images = append(images, image)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

func chatContentPart(field string, index int, raw json.RawMessage) (string, json.RawMessage, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var part struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if json.Unmarshal(raw, &part) != nil {
		return "", nil, inference.Invalid(field+".content["+strconv.Itoa(index)+"]", "must be a text or image part")
	}
	switch part.Type {
	case "", "text":
		return part.Text, nil, nil
	case "image_url":
		image, err := chatImageURL(part.ImageURL)
		if err != nil {
			return "", nil, inference.Invalid(field+".content["+strconv.Itoa(index)+"].image_url", "must include a url")
		}
		return "", image, nil
	default:
		return "", nil, inference.Invalid(field+".content["+strconv.Itoa(index)+"].type", "is unsupported")
	}
}

func chatImageURL(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("missing image url")
	}
	if raw[0] == '"' {
		var url string
		if json.Unmarshal(raw, &url) != nil || url == "" {
			return nil, errors.New("missing image url")
		}
		encoded, err := json.Marshal(struct {
			URL string `json:"url"`
		}{URL: url})
		return encoded, err
	}
	var object struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &object) != nil || object.URL == "" {
		return nil, errors.New("missing image url")
	}
	return append(json.RawMessage(nil), raw...), nil
}

func chatArguments(raw string) (json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, errors.New("invalid arguments")
	}
	return json.RawMessage(raw), nil
}

func chatTool(index int, tool chatWireTool) (inference.Tool, error) {
	if tool.Type != "" && tool.Type != "function" {
		return inference.Tool{}, inference.Invalid("tools["+strconv.Itoa(index)+"].type", "must be function")
	}
	decoded := inference.Tool{Type: "function", Name: tool.Function.Name, Description: tool.Function.Description, Strict: tool.Function.Strict}
	if len(tool.Function.Parameters) > 0 && string(tool.Function.Parameters) != "null" {
		decoded.Parameters = append(json.RawMessage(nil), tool.Function.Parameters...)
	}
	return decoded, nil
}

type chatCompletionBody struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int            `json:"index"`
	Message      *chatAssistant `json:"message,omitempty"`
	Delta        map[string]any `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type chatAssistant struct {
	Role      string        `json:"role"`
	Content   *string       `json:"content"`
	ToolCalls []chatCallOut `json:"tool_calls,omitempty"`
}

type chatCallOut struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func chatCompletion(result inference.Result, created int64) chatCompletionBody {
	message := chatAssistant{Role: "assistant"}
	var text string
	var calls []chatCallOut
	for _, item := range result.Output {
		switch item.Type {
		case "message":
			if text != "" && item.Text != "" {
				text += "\n"
			}
			text += item.Text
		case "function_call":
			calls = append(calls, chatCallOut{
				ID: item.CallID, Type: "function",
				Function: chatFunction{Name: item.Name, Arguments: string(item.Arguments)},
			})
		}
	}
	if text != "" || len(calls) == 0 {
		message.Content = &text
	}
	message.ToolCalls = calls
	reason := chatFinishReason(result.Status, len(calls) > 0)
	body := chatCompletionBody{
		ID: result.ID, Object: "chat.completion", Created: created, Model: result.Model,
		Choices: []chatChoice{{Message: &message, FinishReason: &reason}},
	}
	if result.Usage.Known {
		body.Usage = chatUsageFrom(result.Usage)
	}
	return body
}

func chatFinishReason(status string, usedTool bool) string {
	switch {
	case status == "incomplete":
		return "length"
	case usedTool:
		return "tool_calls"
	default:
		return "stop"
	}
}

func chatUsageFrom(usage inference.Usage) *chatUsage {
	return &chatUsage{
		PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens,
		TotalTokens: usage.InputTokens + usage.OutputTokens,
	}
}

type chatSSEWriter struct {
	response http.ResponseWriter
	metadata executor.StreamMetadata
	created  int64
	tools    map[string]int
	wrote    bool
	started  bool
	usedTool bool
}

func (w *chatSSEWriter) Start(metadata executor.StreamMetadata) error {
	w.metadata = metadata
	if w.created == 0 {
		w.created = time.Now().Unix()
	}
	return nil
}

func (w *chatSSEWriter) WriteEvent(ctx context.Context, event inference.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta == "" {
			return nil
		}
		return w.chunk(map[string]any{"content": event.Delta})
	case "response.output_item.added":
		if event.ItemType != "function_call" {
			return nil
		}
		return w.announceTool(event.CallID, event.Name, "")
	case "response.function_call_arguments.delta":
		if err := w.announceTool(event.CallID, event.Name, ""); err != nil {
			return err
		}
		if event.ArgumentsDelta == "" {
			return nil
		}
		index := w.tools[event.CallID]
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": event.ArgumentsDelta},
		}}})
	case "response.completed":
		if !w.started {
			if err := w.chunk(map[string]any{}); err != nil {
				return err
			}
		}
		reason := chatFinishReason(event.Status, w.usedTool)
		choice := chatChoice{Index: 0, Delta: map[string]any{}, FinishReason: &reason}
		body := w.frame([]chatChoice{choice})
		if event.Usage.Known {
			body.Usage = chatUsageFrom(event.Usage)
		}
		if err := w.writeData(body); err != nil {
			return err
		}
		return w.writeRaw("[DONE]")
	default:
		return nil
	}
}

func (w *chatSSEWriter) announceTool(callID, name, arguments string) error {
	if callID == "" {
		return nil
	}
	if w.tools == nil {
		w.tools = map[string]int{}
	}
	if _, exists := w.tools[callID]; exists {
		return nil
	}
	index := len(w.tools)
	w.tools[callID] = index
	w.usedTool = true
	return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": index, "id": callID, "type": "function",
		"function": map[string]any{"name": name, "arguments": arguments},
	}}})
}

func (w *chatSSEWriter) chunk(delta map[string]any) error {
	if !w.started {
		w.started = true
		if _, ok := delta["role"]; !ok {
			delta["role"] = "assistant"
		}
	}
	return w.writeData(w.frame([]chatChoice{{Index: 0, Delta: delta}}))
}

func (w *chatSSEWriter) frame(choices []chatChoice) chatCompletionBody {
	id := w.metadata.ResponseID
	if id == "" {
		id = "chatcmpl"
	}
	return chatCompletionBody{ID: id, Object: "chat.completion.chunk", Created: w.created, Model: w.metadata.Model, Choices: choices}
}

func (w *chatSSEWriter) WriteTerminalError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, detail := errorDetail(err)
	return w.writeData(struct {
		Error ErrorDetail `json:"error"`
	}{Error: detail})
}

func (w *chatSSEWriter) writeData(payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return w.writeRaw(string(encoded))
}

func (w *chatSSEWriter) writeRaw(data string) error {
	if !w.wrote {
		setRoutingHeaders(w.response, w.metadata)
		w.response.Header().Set("Content-Type", "text/event-stream")
		w.response.Header().Set("Cache-Control", "no-cache")
	}
	if _, err := w.response.Write([]byte("data: " + data + "\n\n")); err != nil {
		return err
	}
	w.wrote = true
	if flusher, ok := w.response.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
