package gemini

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
)

type generateRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Tools             []generateTool    `json:"tools,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FileData         *fileData         `json:"fileData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type fileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type functionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type functionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type generateTool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type generationConfig struct {
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	MaxOutputTokens  int64           `json:"maxOutputTokens,omitempty"`
}

type generateResponse struct {
	ResponseID     string          `json:"responseId"`
	Candidates     []candidate     `json:"candidates"`
	PromptFeedback *promptFeedback `json:"promptFeedback"`
	UsageMetadata  json.RawMessage `json:"usageMetadata"`
}

type candidate struct {
	Content       content         `json:"content"`
	FinishReason  json.RawMessage `json:"finishReason"`
	SafetyRatings json.RawMessage `json:"safetyRatings"`
}

type promptFeedback struct {
	BlockReason json.RawMessage `json:"blockReason"`
}

type usageMetadata struct {
	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
}

type providerData struct {
	Part part `json:"gemini_part"`
}

func toGenerateRequest(model domain.Model, request inference.Request) (generateRequest, error) {
	if strings.TrimSpace(model.UpstreamID) == "" {
		return generateRequest{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("provider upstream model is not configured")}
	}
	if err := validateCapabilities(model, request); err != nil {
		return generateRequest{}, err
	}
	if err := inference.ValidateRequest(request); err != nil {
		return generateRequest{}, err
	}

	result := generateRequest{}
	system := request.Instructions
	for _, item := range request.Input {
		if item.Type == "message" && item.Role == "system" {
			system = joinSystem(system, item.Text)
			continue
		}
		role, mapped, err := toContentPart(item)
		if err != nil {
			return generateRequest{}, err
		}
		result.Contents = appendContent(result.Contents, role, mapped)
	}
	if system != "" {
		result.SystemInstruction = &content{Parts: []part{{Text: system}}}
	}
	if len(request.Tools) != 0 {
		tool := generateTool{}
		for _, input := range request.Tools {
			tool.FunctionDeclarations = append(tool.FunctionDeclarations, functionDeclaration{Name: input.Name, Description: input.Description, Parameters: append(json.RawMessage(nil), input.Parameters...)})
		}
		result.Tools = []generateTool{tool}
	}
	if request.TextFormat != nil || request.MaxOutputTokens != 0 || model.MaxOutputTokens != 0 {
		config := &generationConfig{}
		if request.TextFormat != nil {
			config.ResponseMimeType = "application/json"
			config.ResponseSchema = append(json.RawMessage(nil), request.TextFormat.Schema...)
		}
		config.MaxOutputTokens = request.MaxOutputTokens
		if config.MaxOutputTokens == 0 {
			config.MaxOutputTokens = model.MaxOutputTokens
		}
		result.GenerationConfig = config
	}
	return result, nil
}

func appendContent(contents []content, role string, mapped part) []content {
	if len(contents) == 0 || contents[len(contents)-1].Role != role {
		return append(contents, content{Role: role, Parts: []part{mapped}})
	}
	contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, mapped)
	return contents
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

func toContentPart(item inference.Item) (string, part, error) {
	switch item.Type {
	case "message":
		mapped := part{Text: item.Text}
		if prior, ok := partFromProviderData(item.ProviderData); ok && prior.Text != "" {
			mapped.ThoughtSignature = prior.ThoughtSignature
		}
		return geminiRole(item.Role), mapped, nil
	case "input_image":
		image, err := imagePart(item.ImageURL)
		if err != nil {
			return "", part{}, err
		}
		return geminiRole(item.Role), image, nil
	case "function_call":
		mapped := part{FunctionCall: &functionCall{Name: item.Name, Args: append(json.RawMessage(nil), item.Arguments...)}}
		if prior, ok := partFromProviderData(item.ProviderData); ok && prior.FunctionCall != nil {
			mapped.ThoughtSignature = prior.ThoughtSignature
		}
		return "model", mapped, nil
	case "function_call_output":
		return "user", part{FunctionResponse: &functionResponse{Name: item.CallID, Response: append(json.RawMessage(nil), item.Output...)}}, nil
	default:
		return "", part{}, inference.Invalid("input", "has unsupported item type")
	}
}

func geminiRole(role string) string {
	if role == "assistant" {
		return "model"
	}
	return "user"
}

func imagePart(raw json.RawMessage) (part, error) {
	var image struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &image) != nil || strings.TrimSpace(image.URL) == "" {
		return part{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("invalid image input")}
	}
	if !strings.HasPrefix(image.URL, "data:") {
		return part{FileData: &fileData{FileURI: image.URL}}, nil
	}
	metadata, encoded, ok := strings.Cut(strings.TrimPrefix(image.URL, "data:"), ",")
	if !ok || !strings.HasSuffix(metadata, ";base64") || strings.TrimSpace(encoded) == "" {
		return part{}, &provider.Error{Kind: provider.ErrorInvalidRequest, Err: errors.New("invalid image input")}
	}
	return part{InlineData: &inlineData{MimeType: strings.TrimSuffix(metadata, ";base64"), Data: encoded}}, nil
}

func partFromProviderData(raw json.RawMessage) (part, bool) {
	if len(raw) == 0 {
		return part{}, false
	}
	var data providerData
	if json.Unmarshal(raw, &data) != nil || data.Part.ThoughtSignature == "" {
		return part{}, false
	}
	return data.Part, true
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

func fromGenerateResponse(response generateResponse, model domain.Model, requestID string) (inference.Result, error) {
	if response.PromptFeedback != nil && nonEmptyJSON(response.PromptFeedback.BlockReason) {
		return inference.Result{}, normalizedResponseError(requestID, "provider blocked prompt")
	}
	if len(response.Candidates) == 0 {
		return inference.Result{}, normalizedResponseError(requestID, "provider returned no candidates")
	}
	finish, err := finishStatus(response.Candidates[0].FinishReason)
	if err != nil {
		return inference.Result{}, normalizedResponseError(requestID, "invalid provider finish reason")
	}
	if finish == "safety" {
		return inference.Result{}, normalizedResponseError(requestID, "provider rejected unsafe content")
	}
	usage, err := parseUsage(response.UsageMetadata)
	if err != nil {
		return inference.Result{}, normalizedResponseError(requestID, "invalid provider usage")
	}
	result := inference.Result{ID: response.ResponseID, Model: model.UpstreamID, ProviderRequestID: requestID, Status: finish, Usage: usage}
	for _, mapped := range response.Candidates[0].Content.Parts {
		switch {
		case mapped.Text != "":
			result.Output = append(result.Output, inference.Item{Type: "message", Role: "assistant", Text: mapped.Text, ProviderData: makeProviderData(mapped)})
		case mapped.FunctionCall != nil:
			result.Output = append(result.Output, inference.Item{Type: "function_call", CallID: mapped.FunctionCall.Name, Name: mapped.FunctionCall.Name, Arguments: append(json.RawMessage(nil), mapped.FunctionCall.Args...), ProviderData: makeProviderData(mapped)})
		}
	}
	return result, nil
}

func makeProviderData(mapped part) json.RawMessage {
	if mapped.ThoughtSignature == "" {
		return nil
	}
	raw, err := json.Marshal(providerData{Part: mapped})
	if err != nil {
		return nil
	}
	return raw
}

func finishStatus(raw json.RawMessage) (string, error) {
	var finish string
	if json.Unmarshal(raw, &finish) != nil || finish == "" {
		return "", errors.New("missing finish reason")
	}
	switch finish {
	case "STOP":
		return "completed", nil
	case "MAX_TOKENS":
		return "incomplete", nil
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
		return "safety", nil
	default:
		return "", errors.New("unknown finish reason")
	}
}

func parseUsage(raw json.RawMessage) (inference.Usage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return inference.Usage{}, nil
	}
	var metadata usageMetadata
	if json.Unmarshal(raw, &metadata) != nil {
		return inference.Usage{}, errors.New("invalid usage metadata")
	}
	if metadata.PromptTokenCount == nil || metadata.CandidatesTokenCount == nil || *metadata.PromptTokenCount < 0 || *metadata.CandidatesTokenCount < 0 || (metadata.CachedContentTokenCount != nil && *metadata.CachedContentTokenCount < 0) {
		return inference.Usage{}, errors.New("invalid usage metadata")
	}
	usage := inference.Usage{InputTokens: *metadata.PromptTokenCount, OutputTokens: *metadata.CandidatesTokenCount, Known: true}
	if metadata.CachedContentTokenCount != nil {
		usage.CachedInputTokens = *metadata.CachedContentTokenCount
	}
	return usage, nil
}

func nonEmptyJSON(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value != ""
}

func normalizedResponseError(requestID, message string) error {
	return &provider.Error{Kind: provider.ErrorRetryable, RequestID: requestID, Err: errors.New(message)}
}
