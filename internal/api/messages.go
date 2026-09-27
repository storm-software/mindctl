package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
)

type messagesHandler struct {
	service ResponseExecutor
	config  ResponsesConfig
}

// NewMessagesHandler exposes the supported Anthropic Messages subset.
// Gateway authentication and native Claude credential capture are composed
// outside this handler.
func NewMessagesHandler(service ResponseExecutor, cfg ResponsesConfig) http.Handler {
	return &messagesHandler{service: service, config: cloneResponsesConfig(cfg)}
}

func (h *messagesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeMessagesError(w, ErrMethodNotAllowed)
		return
	}
	if h.service == nil {
		writeMessagesError(w, errors.New("messages executor is unavailable"))
		return
	}
	clientID, ok := ClientID(r.Context())
	if !ok {
		writeMessagesError(w, ErrUnauthorized)
		return
	}
	decoded, err := DecodeMessagesRequest(w, r, h.config.MaxBodyBytes)
	if err != nil {
		h.reject(w, err)
		return
	}
	if decoded.Request.Model != "mindctl-auto" {
		matched := false
		for _, model := range h.config.Models {
			if model.ID == decoded.Request.Model && (decoded.RequiredProvider == "" || model.Provider == decoded.RequiredProvider) {
				matched = true
				break
			}
		}
		if !matched {
			h.reject(w, inference.Invalid("model", fmt.Sprintf("%q is unknown or not served by required provider %q", decoded.Request.Model, decoded.RequiredProvider)))
			return
		}
	}
	ctx := r.Context()
	if decoded.Native {
		ctx = conversation.WithTrustedNativeInput(ctx)
	}
	minTier, maxTier := tierBounds(h.config, Controls{})
	input := executor.Input{
		ClientID: clientID, Request: decoded.Request, RequiredProvider: decoded.RequiredProvider,
		Models: append([]domain.Model(nil), h.config.Models...), MinTier: minTier, MaxTier: maxTier,
		SafeFallbackTier:    h.config.SafeFallbackTier,
		ProviderCredentials: providerCredentials(ctx, h.config), ProviderAvailability: cloneBools(h.config.ProviderAvailability),
	}
	if decoded.Request.Stream {
		writer := &messagesSSEWriter{response: w}
		if err := h.service.Stream(ctx, input, writer); err != nil && !writer.wrote {
			writeMessagesError(w, err)
		}
		return
	}
	output, err := h.service.Execute(ctx, input)
	if err != nil {
		writeMessagesError(w, err)
		return
	}
	body, err := messageFromResult(output.Result, output.Decision.Provider)
	if err != nil {
		writeMessagesError(w, err)
		return
	}
	setRoutingHeaders(w, executor.StreamMetadata{ResponseID: output.Result.ID, Model: output.Result.Model, Provider: output.Decision.Provider, DecisionID: output.AttemptID, Tier: output.Decision.Tier, Attempts: 1})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// reject writes a client error and records why at debug level. Only the
// gateway-authored reason is logged, never request content.
func (h *messagesHandler) reject(w http.ResponseWriter, err error) {
	if h.config.Logger != nil {
		h.config.Logger.Debug("messages.request.rejected", "error", err.Error())
	}
	writeMessagesError(w, err)
}
