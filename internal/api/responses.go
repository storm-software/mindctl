package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

// ResponseExecutor is the narrow execution boundary used by the HTTP handler.
type ResponseExecutor interface {
	Execute(context.Context, executor.Input) (executor.Output, error)
	Stream(context.Context, executor.Input, executor.EventWriter) error
}

// ResponsesConfig is the immutable, server-owned routing snapshot for the
// Responses handler. It deliberately contains availability booleans, not keys.
type ResponsesConfig struct {
	MaxBodyBytes                              int64
	Models                                    []domain.Model
	MinTier, MaxTier                          domain.Tier
	SafeFallbackTier                          domain.Tier
	ProviderCredentials, ProviderAvailability map[string]bool
	ChatGPTOAuthProviders                     map[string]bool
}

// NewResponsesHandler exposes the supported OpenAI Responses subset. Caller
// authentication is intentionally composed outside this handler.
func NewResponsesHandler(service ResponseExecutor, cfg ResponsesConfig) http.Handler {
	return &responsesHandler{service: service, config: cloneResponsesConfig(cfg)}
}

type responsesHandler struct {
	service ResponseExecutor
	config  ResponsesConfig
}

func (h *responsesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		WriteError(w, ErrMethodNotAllowed)
		return
	}
	if h.service == nil {
		WriteError(w, errors.New("responses executor is unavailable"))
		return
	}
	clientID, ok := ClientID(r.Context())
	if !ok {
		WriteError(w, ErrUnauthorized)
		return
	}
	request, controls, err := DecodeResponseRequest(w, r, h.config.MaxBodyBytes)
	if err != nil {
		WriteError(w, err)
		return
	}
	input, err := h.input(r.Context(), clientID, request, controls)
	if err != nil {
		WriteError(w, err)
		return
	}
	if request.Stream {
		writer := &responseSSEWriter{response: w}
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
	writeResponse(w, output)
}

func (h *responsesHandler) input(ctx context.Context, clientID string, request inference.Request, controls Controls) (executor.Input, error) {
	minTier, maxTier := tierBounds(h.config, controls)
	if minTier != nil && maxTier != nil && minTier.Rank() > maxTier.Rank() {
		return executor.Input{}, inference.Invalid("tier", "minimum tier exceeds maximum tier")
	}
	return executor.Input{
		ClientID: clientID, Request: request, Models: append([]domain.Model(nil), h.config.Models...),
		MinTier: minTier, MaxTier: maxTier, SafeFallbackTier: h.config.SafeFallbackTier,
		AllowEscalation:     controls.AllowEscalation,
		ProviderCredentials: providerCredentials(ctx, h.config), ProviderAvailability: cloneBools(h.config.ProviderAvailability),
	}, nil
}

func providerCredentials(ctx context.Context, cfg ResponsesConfig) map[string]bool {
	credentials := cloneBools(cfg.ProviderCredentials)
	_, hasChatGPT := upstreamauth.ChatGPT(ctx)
	for providerID, enabled := range cfg.ChatGPTOAuthProviders {
		if enabled {
			credentials[providerID] = hasChatGPT
		}
	}
	return credentials
}

func tierBounds(cfg ResponsesConfig, controls Controls) (*domain.Tier, *domain.Tier) {
	var minTier, maxTier *domain.Tier
	if cfg.MinTier.Valid() {
		value := cfg.MinTier
		minTier = &value
	}
	if cfg.MaxTier.Valid() {
		value := cfg.MaxTier
		maxTier = &value
	}
	if controls.MinTier != nil && (minTier == nil || *controls.MinTier > *minTier) {
		value := *controls.MinTier
		minTier = &value
	}
	if controls.MaxTier != nil && (maxTier == nil || *controls.MaxTier < *maxTier) {
		value := *controls.MaxTier
		maxTier = &value
	}
	return minTier, maxTier
}

func cloneResponsesConfig(cfg ResponsesConfig) ResponsesConfig {
	cfg.Models = append([]domain.Model(nil), cfg.Models...)
	cfg.ProviderCredentials = cloneBools(cfg.ProviderCredentials)
	cfg.ProviderAvailability = cloneBools(cfg.ProviderAvailability)
	cfg.ChatGPTOAuthProviders = cloneBools(cfg.ChatGPTOAuthProviders)
	return cfg
}

func cloneBools(input map[string]bool) map[string]bool {
	if input == nil {
		return nil
	}
	output := make(map[string]bool, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func writeResponse(w http.ResponseWriter, output executor.Output) {
	setRoutingHeaders(w, executor.StreamMetadata{ResponseID: output.Result.ID, Model: output.Result.Model, DecisionID: output.AttemptID, Tier: output.Decision.Tier, Attempts: 1})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(responseFromResult(output.Result))
}

type responseBody struct {
	ID     string           `json:"id"`
	Object string           `json:"object"`
	Status string           `json:"status"`
	Model  string           `json:"model"`
	Output []responseOutput `json:"output"`
	Usage  *responseUsage   `json:"usage,omitempty"`
}

type responseOutput struct {
	Type      string            `json:"type,omitempty"`
	Role      string            `json:"role,omitempty"`
	CallID    string            `json:"call_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Arguments json.RawMessage   `json:"arguments,omitempty"`
	Content   []responseContent `json:"content,omitempty"`
}

type responseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseUsage struct {
	InputTokens        int64               `json:"input_tokens"`
	OutputTokens       int64               `json:"output_tokens"`
	InputTokensDetails responseUsageDetail `json:"input_tokens_details"`
}

type responseUsageDetail struct {
	CachedTokens int64 `json:"cached_tokens"`
}

func responseFromResult(result inference.Result) responseBody {
	body := responseBody{ID: result.ID, Object: "response", Status: result.Status, Model: result.Model}
	for _, item := range result.Output {
		switch item.Type {
		case "message":
			body.Output = append(body.Output, responseOutput{Type: "message", Role: item.Role, Content: []responseContent{{Type: "output_text", Text: item.Text}}})
		case "function_call":
			body.Output = append(body.Output, responseOutput{Type: "function_call", CallID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		}
	}
	if result.Usage.Known {
		body.Usage = &responseUsage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens, InputTokensDetails: responseUsageDetail{CachedTokens: result.Usage.CachedInputTokens}}
	}
	return body
}

type responseSSEWriter struct {
	response http.ResponseWriter
	metadata executor.StreamMetadata
	wrote    bool
}

func (w *responseSSEWriter) Start(metadata executor.StreamMetadata) error {
	w.metadata = metadata
	return nil
}

func (w *responseSSEWriter) WriteEvent(_ context.Context, event inference.Event) error {
	payload := streamPayload(event, w.metadata)
	return w.write(event.Type, payload)
}

func (w *responseSSEWriter) WriteTerminalError(_ context.Context, err error) error {
	_, detail := errorDetail(err)
	return w.write("error", struct {
		Type  string      `json:"type"`
		Error ErrorDetail `json:"error"`
	}{Type: "error", Error: detail})
}

func (w *responseSSEWriter) write(event string, payload any) error {
	response := w.response
	if !w.wrote {
		setRoutingHeaders(response, w.metadata)
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-cache")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := response.Write([]byte("event: " + event + "\ndata: ")); err != nil {
		return err
	}
	if _, err := response.Write(encoded); err != nil {
		return err
	}
	if _, err := response.Write([]byte("\n\n")); err != nil {
		return err
	}
	w.wrote = true
	if flusher, ok := response.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func streamPayload(event inference.Event, metadata executor.StreamMetadata) any {
	switch event.Type {
	case "response.output_text.delta":
		return struct {
			Type       string `json:"type,omitempty"`
			ResponseID string `json:"response_id,omitempty"`
			ItemID     string `json:"item_id,omitempty"`
			Delta      string `json:"delta,omitempty"`
		}{Type: event.Type, ResponseID: event.ResponseID, ItemID: event.ItemID, Delta: event.Delta}
	case "response.output_text.done":
		return struct {
			Type       string `json:"type,omitempty"`
			ResponseID string `json:"response_id,omitempty"`
			ItemID     string `json:"item_id,omitempty"`
			Text       string `json:"text,omitempty"`
		}{Type: event.Type, ResponseID: event.ResponseID, ItemID: event.ItemID, Text: event.Delta}
	case "response.function_call_arguments.delta":
		return struct {
			Type       string `json:"type,omitempty"`
			ResponseID string `json:"response_id,omitempty"`
			ItemID     string `json:"item_id,omitempty"`
			CallID     string `json:"call_id,omitempty"`
			Delta      string `json:"delta,omitempty"`
		}{Type: event.Type, ResponseID: event.ResponseID, ItemID: event.ItemID, CallID: event.CallID, Delta: event.ArgumentsDelta}
	case "response.function_call_arguments.done":
		return struct {
			Type       string `json:"type,omitempty"`
			ResponseID string `json:"response_id,omitempty"`
			ItemID     string `json:"item_id,omitempty"`
			CallID     string `json:"call_id,omitempty"`
			Arguments  string `json:"arguments,omitempty"`
		}{Type: event.Type, ResponseID: event.ResponseID, ItemID: event.ItemID, CallID: event.CallID, Arguments: event.ArgumentsDelta}
	case "response.custom_tool_call_input.delta":
		return struct {
			Type        string `json:"type"`
			ResponseID  string `json:"response_id,omitempty"`
			ItemID      string `json:"item_id,omitempty"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}{Type: event.Type, ResponseID: event.ResponseID, ItemID: event.ItemID, OutputIndex: event.OutputIndex, Delta: event.Delta}
	case "response.custom_tool_call_input.done":
		return struct {
			Type        string `json:"type"`
			ResponseID  string `json:"response_id,omitempty"`
			ItemID      string `json:"item_id,omitempty"`
			OutputIndex int    `json:"output_index"`
			Input       string `json:"input"`
		}{Type: event.Type, ResponseID: event.ResponseID, ItemID: event.ItemID, OutputIndex: event.OutputIndex, Input: event.Delta}
	case "response.output_item.added", "response.output_item.done":
		return struct {
			Type        string `json:"type"`
			ResponseID  string `json:"response_id,omitempty"`
			OutputIndex int    `json:"output_index"`
			Item        struct {
				ID        string `json:"id,omitempty"`
				Type      string `json:"type"`
				CallID    string `json:"call_id,omitempty"`
				Name      string `json:"name,omitempty"`
				Namespace string `json:"namespace,omitempty"`
				Input     string `json:"input,omitempty"`
			} `json:"item"`
		}{
			Type: event.Type, ResponseID: event.ResponseID, OutputIndex: event.OutputIndex,
			Item: struct {
				ID        string `json:"id,omitempty"`
				Type      string `json:"type"`
				CallID    string `json:"call_id,omitempty"`
				Name      string `json:"name,omitempty"`
				Namespace string `json:"namespace,omitempty"`
				Input     string `json:"input,omitempty"`
			}{ID: event.ItemID, Type: event.ItemType, CallID: event.CallID, Name: event.Name, Namespace: event.Namespace, Input: event.Input},
		}
	default:
		return struct {
			Type     string       `json:"type"`
			Response responseBody `json:"response"`
		}{Type: event.Type, Response: responseBody{ID: event.ResponseID, Object: "response", Status: event.Status, Model: metadata.Model, Usage: usageBody(event.Usage)}}
	}
}

func usageBody(usage inference.Usage) *responseUsage {
	if !usage.Known {
		return nil
	}
	return &responseUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, InputTokensDetails: responseUsageDetail{CachedTokens: usage.CachedInputTokens}}
}

func setRoutingHeaders(w http.ResponseWriter, metadata executor.StreamMetadata) {
	if metadata.DecisionID != "" {
		w.Header().Set("X-Mindctl-Decision-ID", metadata.DecisionID)
	}
	if metadata.Tier.Valid() {
		w.Header().Set("X-Mindctl-Tier", metadata.Tier.String())
	}
	if metadata.Attempts > 0 {
		w.Header().Set("X-Mindctl-Attempts", strconv.Itoa(metadata.Attempts))
	}
}
