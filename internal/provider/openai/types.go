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
	Model              string            `json:"model"`
	Instructions       string            `json:"instructions,omitempty"`
	Input              []json.RawMessage `json:"input"`
	Tools              []responseTool    `json:"tools,omitempty"`
	Text               *responseText     `json:"text,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	MaxOutputTokens    int64             `json:"max_output_tokens,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type responseText struct {
	Format responseTextFormat `json:"format"`
}

type responseTextFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      bool            `json:"strict,omitempty"`
}

type responseInputItem struct {
	Type      string            `json:"type"`
	Role      string            `json:"role,omitempty"`
	Content   []responseContent `json:"content,omitempty"`
	CallID    string            `json:"call_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Arguments json.RawMessage   `json:"arguments,omitempty"`
	Output    json.RawMessage   `json:"output,omitempty"`
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
		Model: modelID(model), Instructions: request.Instructions, Stream: stream,
		MaxOutputTokens: request.MaxOutputTokens, PreviousResponseID: request.PreviousResponseID,
	}
	for _, item := range request.Input {
		encoded, err := json.Marshal(toResponseInput(item))
		if err != nil {
			return responsesRequest{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("cannot encode provider request item")}
		}
		result.Input = append(result.Input, encoded)
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, responseTool{Type: tool.Type, Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, Strict: tool.Strict})
	}
	if format := request.TextFormat; format != nil {
		result.Text = &responseText{Format: responseTextFormat{Type: format.Type, Name: format.Name, Description: format.Description, Schema: format.Schema, Strict: format.Strict}}
	}
	return result, nil
}

func toResponseInput(item inference.Item) responseInputItem {
	switch item.Type {
	case "message":
		contentType := "input_text"
		if item.Role == "assistant" {
			contentType = "output_text"
		}
		return responseInputItem{Type: item.Type, Role: item.Role, Content: []responseContent{{Type: contentType, Text: item.Text}}}
	case "input_image":
		return responseInputItem{Type: "message", Role: item.Role, Content: []responseContent{{Type: "input_image", ImageURL: item.ImageURL}}}
	default:
		return responseInputItem{Type: item.Type, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Output: item.Output}
	}
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
		ResponseID string `json:"response_id"`
		ItemID     string `json:"item_id"`
		CallID     string `json:"call_id"`
		Name       string `json:"name"`
		Delta      string `json:"delta"`
		Arguments  string `json:"arguments"`
		Text       string `json:"text"`
		Response   *struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Usage  json.RawMessage `json:"usage"`
		} `json:"response"`
		Item *struct {
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
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
	event := inference.Event{Type: kind, ResponseID: frame.ResponseID, ItemID: frame.ItemID, CallID: frame.CallID, Name: frame.Name, Data: append(json.RawMessage(nil), data...)}
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
	}
	return event, nil
}
