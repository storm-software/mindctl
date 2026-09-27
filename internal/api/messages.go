package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	if decoded.Request.Model != inference.AutomaticModel {
		id, ok := resolveMessagesModel(h.config.Models, decoded.Request.Model, decoded.RequiredProvider)
		if !ok {
			h.reject(w, inference.Invalid("model", fmt.Sprintf("%q is unknown or not served by required provider %q", decoded.Request.Model, decoded.RequiredProvider)))
			return
		}
		decoded.Request.Model = id
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
		SavingsBaseline: h.config.SavingsBaseline,
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

// resolveMessagesModel returns the configured ID for requested. Claude Code
// sends dated Anthropic snapshots such as claude-haiku-4-5-20251001, which
// resolve to the undated catalog entry when no exact entry exists.
func resolveMessagesModel(models []domain.Model, requested, requiredProvider string) (string, bool) {
	find := func(id, provider string) bool {
		for _, model := range models {
			if model.ID == id && (provider == "" || model.Provider == provider) {
				return true
			}
		}
		return false
	}
	if find(requested, requiredProvider) {
		return requested, true
	}
	index := strings.LastIndexByte(requested, '-')
	if index <= 0 || len(requested)-index != 9 || strings.Trim(requested[index+1:], "0123456789") != "" {
		return "", false
	}
	if requiredProvider != "" && requiredProvider != "anthropic" {
		return "", false
	}
	base := requested[:index]
	return base, find(base, "anthropic")
}

// reject writes a client error and records why at debug level. Only the
// gateway-authored reason is logged, never request content.
func (h *messagesHandler) reject(w http.ResponseWriter, err error) {
	if h.config.Logger != nil {
		h.config.Logger.Debug("messages.request.rejected", "error", err.Error())
	}
	writeMessagesError(w, err)
}
