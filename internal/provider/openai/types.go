package openai

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
)

type responsesRequest struct {
	Model              string                  `json:"model"`
	Instructions       string                  `json:"instructions,omitempty"`
	Input              []json.RawMessage       `json:"input"`
	Tools              []responseTool          `json:"tools,omitempty"`
	ToolChoice         string                  `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                   `json:"parallel_tool_calls,omitempty"`
	Reasoning          *responseReasoning      `json:"reasoning,omitempty"`
	Store              *bool                   `json:"store,omitempty"`
	Text               *responseText           `json:"text,omitempty"`
	Stream             *bool                   `json:"stream,omitempty"`
	StreamOptions      *responseStreamOptions  `json:"stream_options,omitempty"`
	Include            *[]string               `json:"include,omitempty"`
	ServiceTier        string                  `json:"service_tier,omitempty"`
	PromptCacheKey     string                  `json:"prompt_cache_key,omitempty"`
	ClientMetadata     map[string]string       `json:"client_metadata,omitempty"`
	AccessPrograms     *responseAccessPrograms `json:"access_programs,omitempty"`
	MaxOutputTokens    int64                   `json:"max_output_tokens,omitempty"`
	PreviousResponseID string                  `json:"previous_response_id,omitempty"`
}

type responseReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
	Context string `json:"context,omitempty"`
}

type responseStreamOptions struct {
	ReasoningSummaryDelivery string `json:"reasoning_summary_delivery"`
}

type responseAccessPrograms struct {
	Cyber string `json:"cyber"`
}

type responseTool struct {
	Type         string              `json:"type"`
	Name         string              `json:"name,omitempty"`
	Description  string              `json:"description,omitempty"`
	Parameters   json.RawMessage     `json:"parameters,omitempty"`
	Format       *responseToolFormat `json:"format,omitempty"`
	DeferLoading *bool               `json:"defer_loading,omitempty"`
	Tools        []responseTool      `json:"tools,omitempty"`
	Strict       bool                `json:"strict,omitempty"`
}

type responseToolFormat struct {
	Type       string `json:"type"`
	Syntax     string `json:"syntax"`
	Definition string `json:"definition"`
}

type responseText struct {
	Verbosity string              `json:"verbosity,omitempty"`
	Format    *responseTextFormat `json:"format,omitempty"`
}

type responseTextFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      bool            `json:"strict,omitempty"`
}

type responseInputItem struct {
	ID        string            `json:"id,omitempty"`
	Type      string            `json:"type"`
	Role      string            `json:"role,omitempty"`
	Content   []responseContent `json:"content,omitempty"`
	CallID    string            `json:"call_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Input     string            `json:"input,omitempty"`
	Arguments json.RawMessage   `json:"arguments,omitempty"`
	Output    json.RawMessage   `json:"output,omitempty"`
	Tools     []responseTool    `json:"tools,omitempty"`
}

type responseContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

type responsesResponse struct {
	ID     string           `json:"id"`
	Status string           `json:"status"`
	Model  string           `json:"model"`
	Output []responseOutput `json:"output"`
	Usage  json.RawMessage  `json:"usage"`
}

type responseOutput struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Role      string            `json:"role"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Input     string            `json:"input"`
	Arguments json.RawMessage   `json:"arguments"`
	Content   []responseContent `json:"content"`
}

type responseUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	Details      *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func toResponsesRequest(model domain.Model, request inference.Request, stream bool) (responsesRequest, error) {
	if strings.TrimSpace(model.UpstreamID) == "" {
		return responsesRequest{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider upstream model is not configured")}
	}
	if err := validateCapabilities(model, request); err != nil {
		return responsesRequest{}, err
	}
	if err := inference.ValidateRequest(request); err != nil {
		return responsesRequest{}, err
	}
	result := responsesRequest{
		Model: modelID(model), Instructions: request.Instructions, ToolChoice: request.ToolChoice,
		ParallelToolCalls: cloneBoolPointer(request.ParallelToolCalls), ServiceTier: request.ServiceTier,
		PromptCacheKey: request.PromptCacheKey, ClientMetadata: cloneStringMap(request.ClientMetadata),
		MaxOutputTokens: request.MaxOutputTokens, PreviousResponseID: request.PreviousResponseID,
	}
	if stream {
		result.Stream = boolPointer(true)
	}
	if request.Include != nil {
		include := append([]string(nil), request.Include...)
		result.Include = &include
	}
	if request.Reasoning != nil {
		result.Reasoning = &responseReasoning{Effort: request.Reasoning.Effort, Summary: request.Reasoning.Summary, Context: request.Reasoning.Context}
	}
	if request.StreamOptions != nil {
		result.StreamOptions = &responseStreamOptions{ReasoningSummaryDelivery: request.StreamOptions.ReasoningSummaryDelivery}
	}
	if request.AccessPrograms != nil {
		result.AccessPrograms = &responseAccessPrograms{Cyber: request.AccessPrograms.Cyber}
	}
	for _, item := range request.Input {
		encoded, err := json.Marshal(toResponseInput(item))
		if err != nil {
			return responsesRequest{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request item")}
		}
		result.Input = append(result.Input, encoded)
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, encodeTool(tool))
	}
	if request.TextVerbosity != "" || request.TextFormat != nil {
		result.Text = &responseText{Verbosity: request.TextVerbosity}
		if format := request.TextFormat; format != nil {
			result.Text.Format = &responseTextFormat{Type: format.Type, Name: format.Name, Description: format.Description, Schema: format.Schema, Strict: format.Strict}
		}
	}
	return result, nil
}

func boolPointer(value bool) *bool { return &value }

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	return boolPointer(*value)
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func toResponseInput(item inference.Item) responseInputItem {
	switch item.Type {
	case "message":
		contentType := "input_text"
		if item.Role == "assistant" {
			contentType = "output_text"
		}
		return responseInputItem{ID: item.ID, Type: item.Type, Role: item.Role, Content: []responseContent{{Type: contentType, Text: item.Text}}}
	case "input_image":
		return responseInputItem{ID: item.ID, Type: "message", Role: item.Role, Content: []responseContent{{Type: "input_image", ImageURL: item.ImageURL}}}
	case "additional_tools":
		encoded := responseInputItem{ID: item.ID, Type: item.Type, Role: item.Role}
		for _, tool := range item.Tools {
			encoded.Tools = append(encoded.Tools, encodeTool(tool))
		}
		return encoded
	case "custom_tool_call":
		return responseInputItem{ID: item.ID, Type: item.Type, CallID: item.CallID, Name: item.Name, Namespace: item.Namespace, Input: item.Input}
	case "custom_tool_call_output":
		return responseInputItem{ID: item.ID, Type: item.Type, CallID: item.CallID, Name: item.Name, Output: item.Output}
	default:
		return responseInputItem{ID: item.ID, Type: item.Type, CallID: item.CallID, Name: item.Name, Namespace: item.Namespace, Arguments: item.Arguments, Output: item.Output}
	}
}

func encodeTool(tool inference.Tool) responseTool {
	encoded := responseTool{
		Type: tool.Type, Name: tool.Name, Description: tool.Description,
		Parameters: tool.Parameters, DeferLoading: cloneBoolPointer(tool.DeferLoading), Strict: tool.Strict,
	}
	if tool.Format != nil {
		encoded.Format = &responseToolFormat{Type: tool.Format.Type, Syntax: tool.Format.Syntax, Definition: tool.Format.Definition}
	}
	for _, nested := range tool.Tools {
		encoded.Tools = append(encoded.Tools, encodeTool(nested))
	}
	return encoded
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
		case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "additional_tools":
			if !model.Capabilities.Functions {
				return unsupported("functions")
			}
		}
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" && tool.Type != "custom" && tool.Type != "namespace" {
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

func modelID(model domain.Model) string { return model.UpstreamID }

func fromResponsesResponse(response responsesResponse, model domain.Model, requestID string) inference.Result {
	result := inference.Result{ID: response.ID, Model: modelID(model), ProviderRequestID: requestID, Status: response.Status, Usage: parseUsage(response.Usage)}
	for _, output := range response.Output {
		switch output.Type {
		case "message":
			for _, content := range output.Content {
				if content.Type == "output_text" {
					result.Output = append(result.Output, inference.Item{Type: "message", Role: output.Role, Text: content.Text})
				}
			}
		case "function_call":
			result.Output = append(result.Output, inference.Item{Type: output.Type, CallID: output.CallID, Name: output.Name, Arguments: append(json.RawMessage(nil), output.Arguments...)})
		case "custom_tool_call":
			result.Output = append(result.Output, inference.Item{ID: output.ID, Type: output.Type, CallID: output.CallID, Name: output.Name, Namespace: output.Namespace, Input: output.Input})
		}
	}
	return result
}

func parseUsage(raw json.RawMessage) inference.Usage {
	if len(raw) == 0 || string(raw) == "null" {
		return inference.Usage{}
	}
	var usage responseUsage
	if json.Unmarshal(raw, &usage) != nil || usage.InputTokens == nil || usage.OutputTokens == nil || *usage.InputTokens < 0 || *usage.OutputTokens < 0 {
		return inference.Usage{}
	}
	result := inference.Usage{InputTokens: *usage.InputTokens, OutputTokens: *usage.OutputTokens, Known: true}
	if usage.Details != nil && usage.Details.CachedTokens != nil {
		if *usage.Details.CachedTokens < 0 {
			return inference.Usage{}
		}
		result.CachedInputTokens = *usage.Details.CachedTokens
	}
	return result
}

func streamEvent(kind string, data []byte) (inference.Event, error) {
	var frame struct {
		ResponseID  string `json:"response_id"`
		ItemID      string `json:"item_id"`
		OutputIndex int    `json:"output_index"`
		CallID      string `json:"call_id"`
		Name        string `json:"name"`
		Delta       string `json:"delta"`
		Input       string `json:"input"`
		Arguments   string `json:"arguments"`
		Text        string `json:"text"`
		Response    *struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Usage  json.RawMessage `json:"usage"`
		} `json:"response"`
		Item *struct {
			ID        string            `json:"id"`
			Type      string            `json:"type"`
			Role      string            `json:"role"`
			CallID    string            `json:"call_id"`
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Input     string            `json:"input"`
			Arguments string            `json:"arguments"`
			Content   []responseContent `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return inference.Event{}, err
	}
	if frame.ResponseID == "" && frame.Response != nil {
		frame.ResponseID = frame.Response.ID
	}
	if frame.ItemID == "" && frame.Item != nil {
		frame.ItemID = frame.Item.ID
	}
	if frame.CallID == "" && frame.Item != nil {
		frame.CallID = frame.Item.CallID
	}
	if frame.Name == "" && frame.Item != nil {
		frame.Name = frame.Item.Name
	}
	event := inference.Event{Type: kind, ResponseID: frame.ResponseID, ItemID: frame.ItemID, CallID: frame.CallID, Name: frame.Name, OutputIndex: frame.OutputIndex, Data: append(json.RawMessage(nil), data...)}
	if frame.Item != nil {
		event.ItemType = frame.Item.Type
		event.Role = frame.Item.Role
		event.Namespace = frame.Item.Namespace
		event.Input = frame.Item.Input
		if event.ArgumentsDelta == "" {
			event.ArgumentsDelta = frame.Item.Arguments
		}
		for _, content := range frame.Item.Content {
			if content.Type == "output_text" {
				event.ItemText += content.Text
			}
		}
	}
	if frame.Response != nil {
		event.Status = frame.Response.Status
		event.Usage = parseUsage(frame.Response.Usage)
	}
	switch {
	case strings.Contains(kind, "function_call_arguments"):
		event.ArgumentsDelta = frame.Delta
		if event.ArgumentsDelta == "" {
			event.ArgumentsDelta = frame.Arguments
		}
	case strings.Contains(kind, "output_text"):
		event.Delta = frame.Delta
		if event.Delta == "" {
			event.Delta = frame.Text
		}
	case strings.Contains(kind, "custom_tool_call_input"):
		event.Delta = frame.Delta
		if event.Delta == "" {
			event.Delta = frame.Input
		}
	}
	return event, nil
}
