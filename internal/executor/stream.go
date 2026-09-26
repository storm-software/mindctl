package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
)

// StreamMetadata is content-free routing state published to the HTTP boundary
// before its first SSE frame is written.
type StreamMetadata struct {
	ResponseID, Model, DecisionID string
	Provider                      string
	Tier                          domain.Tier
	Attempts                      int
}

// EventWriter receives canonical stream events. Implementations must not expose
// ProviderRequestID or provider-native Event.Data to API callers.
type EventWriter interface {
	Start(StreamMetadata) error
	WriteEvent(context.Context, inference.Event) error
	WriteTerminalError(context.Context, error) error
}

// Stream executes a routed provider stream. A retry may begin only before the
// writer observes a visible event; afterwards a failure is terminal.
func (s *Service) Stream(ctx context.Context, in Input, writer EventWriter) error {
	if err := inference.ValidateRequest(in.Request); err != nil {
		return err
	}
	if !in.Request.Stream {
		return inference.Invalid("stream", "requires stream=true")
	}
	if s == nil || s.policy == nil || s.providers == nil || s.conversations == nil || writer == nil {
		return errors.New("executor: dependencies and event writer are required")
	}

	turn, decision, err := s.streamDecision(ctx, in)
	if err != nil {
		return err
	}
	decisions := s.streamCandidates(in, decision)
	s.trace("route.stream.candidates",
		"response_id", turn.ResponseID,
		"count", len(decisions),
		"candidates", slog.AnyValue(decisions),
	)
	explicitAlternativesExpanded := false
	scheduled := make(map[string]struct{}, len(decisions))
	for _, candidate := range decisions {
		scheduled[candidate.Provider+"\x00"+candidate.ModelID] = struct{}{}
	}
	emitted := false
	var lastErr error
	for index := 0; index < len(decisions); index++ {
		candidate := decisions[index]
		model, ok := selectedModel(in.Models, candidate)
		if !ok {
			lastErr = fmt.Errorf("executor: selected model %q is not configured", candidate.ModelID)
			break
		}
		adapter, ok := s.providers.Get(candidate.Provider)
		if !ok {
			lastErr = fmt.Errorf("executor: provider %q is unavailable", candidate.Provider)
			break
		}
		attempt, err := s.conversations.BeginAttempt(ctx, turn, candidate)
		if err != nil {
			return err
		}
		s.trace("route.attempt.started",
			"response_id", turn.ResponseID,
			"attempt_id", attempt.ID,
			"attempt_number", index+1,
			"provider", candidate.Provider,
			"model", candidate.ModelID,
			"tier", candidate.Tier.String(),
			"stream", true,
		)
		if err := writer.Start(StreamMetadata{ResponseID: turn.ResponseID, Model: candidate.ModelID, Provider: candidate.Provider, DecisionID: attempt.ID, Tier: candidate.Tier, Attempts: index + 1}); err != nil {
			return s.failStreamAttempt(ctx, attempt, "", err)
		}

		request := in.Request
		request.ID, request.Model, request.PreviousResponseID = turn.ResponseID, candidate.ModelID, ""
		request.Input = turn.TranscriptFor(candidate.Provider)
		stream, err := adapter.Stream(providerScopedContext(ctx, candidate.Provider), model, providerScopedRequest(request, candidate.Provider))
		var result inference.Result
		if err == nil {
			var completion inference.Event
			var attemptEmitted bool
			result, completion, attemptEmitted, err = drainStream(ctx, stream, turn.ResponseID, candidate.ModelID, candidate.Provider, writer)
			emitted = emitted || attemptEmitted
			if closeErr := stream.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
			if err == nil {
				result.ID, result.Model = turn.ResponseID, candidate.ModelID
				pin := router.Pin{ModelID: candidate.ModelID, Provider: candidate.Provider, Floor: candidate.Tier}
				persistCtx, cancel := persistenceContext(ctx)
				err = s.conversations.CommitResult(persistCtx, turn, pin, result)
				cancel()
				if err == nil {
					completion.ResponseID = turn.ResponseID
					completion.Status = result.Status
					completion.Usage = result.Usage
					completion.StopReason = result.StopReason
					completion.ProviderRequestID = result.ProviderRequestID
					if writeErr := writer.WriteEvent(ctx, completion); writeErr != nil {
						return writeErr
					}
					s.trace("route.attempt.completed",
						"response_id", turn.ResponseID,
						"attempt_id", attempt.ID,
						"attempt_number", index+1,
						"provider_request_id", result.ProviderRequestID,
						"status", result.Status,
						"input_tokens", result.Usage.InputTokens,
						"cached_input_tokens", result.Usage.CachedInputTokens,
						"output_tokens", result.Usage.OutputTokens,
						"stream", true,
					)
					return nil
				}
			}
		}

		lastErr = err
		observedProviderRequestID := result.ProviderRequestID
		if observedProviderRequestID == "" {
			observedProviderRequestID = providerRequestID(err)
		}
		if failErr := s.failStreamAttempt(ctx, attempt, observedProviderRequestID, err); failErr != nil {
			lastErr = errors.Join(lastErr, failErr)
		}
		attributes := []any{
			"response_id", turn.ResponseID,
			"attempt_id", attempt.ID,
			"attempt_number", index + 1,
			"provider_request_id", observedProviderRequestID,
			"error_kind", errorKind(err),
			"visible_output", emitted,
			"stream", true,
		}
		attributes = append(attributes, errorDiagnostic(err)...)
		s.trace("route.attempt.failed", attributes...)
		if emitted {
			persistCtx, cancel := persistenceContext(ctx)
			floorErr := s.conversations.RaiseFloor(persistCtx, turn, nextTier(candidate.Tier))
			cancel()
			if floorErr != nil {
				lastErr = errors.Join(lastErr, floorErr)
			}
			if ctx.Err() == nil {
				if writeErr := writer.WriteTerminalError(ctx, lastErr); writeErr != nil {
					lastErr = errors.Join(lastErr, writeErr)
				}
			}
			return lastErr
		}
		if retryableStreamError(err) && index == len(decisions)-1 && !explicitAlternativesExpanded {
			explicitAlternativesExpanded = true
			additional, candidateErr := s.explicitStreamCandidates(in, turn, decision)
			if candidateErr != nil {
				return errors.Join(lastErr, candidateErr)
			}
			for _, candidate := range additional {
				identity := candidate.Provider + "\x00" + candidate.ModelID
				if _, ok := scheduled[identity]; ok {
					continue
				}
				scheduled[identity] = struct{}{}
				decisions = append(decisions, candidate)
			}
			if index < len(decisions)-1 {
				s.trace("route.stream.retry",
					"response_id", turn.ResponseID,
					"completed_attempts", index+1,
					"remaining_candidates", len(decisions)-index-1,
				)
				continue
			}
		}
		if !retryableStreamError(err) || index == len(decisions)-1 {
			return lastErr
		}
		s.trace("route.stream.retry",
			"response_id", turn.ResponseID,
			"completed_attempts", index+1,
			"remaining_candidates", len(decisions)-index-1,
		)
	}
	if lastErr == nil {
		lastErr = errors.New("executor: no stream candidate")
	}
	return lastErr
}

func (s *Service) streamDecision(ctx context.Context, in Input) (conversation.Turn, router.Decision, error) {
	turn, err := s.conversations.Start(ctx, in.ClientID, in.Request)
	if err != nil {
		return conversation.Turn{}, router.Decision{}, err
	}
	s.trace("route.started",
		"client_id", in.ClientID,
		"response_id", turn.ResponseID,
		"conversation_id", turn.ConversationID,
		"requested_model", in.Request.Model,
		"stream", true,
	)
	features := normalizedFeatures(in.Features, in.Request, turn)
	s.traceFeatures(turn.ResponseID, features)
	decisionInput := router.DecisionInput{
		Features: features, Models: in.Models, MinTier: in.MinTier, MaxTier: in.MaxTier,
		RequiredProvider:    in.RequiredProvider,
		ProviderCredentials: in.ProviderCredentials, ProviderAvailability: providerAvailability(in.Models, in.ProviderAvailability, s.providers),
	}
	if in.Request.Model != automaticModel {
		decisionInput.ModelID, decisionInput.Floor = in.Request.Model, turn.Floor
		decision, err := s.policy.Decide(decisionInput)
		if err != nil {
			s.traceDecision(turn.ResponseID, decision, false)
			return conversation.Turn{}, router.Decision{}, explicitError(in.Request.Model, decision, err)
		}
		s.traceDecision(turn.ResponseID, decision, true)
		return turn, decision, nil
	}
	if pin := turnPin(turn); pin != nil {
		decisionInput.Pin, decisionInput.Floor = pin, pin.Floor
		decision, err := s.policy.Decide(decisionInput)
		if err == nil && decision.ModelID == pin.ModelID && decision.Provider == pin.Provider {
			s.traceDecision(turn.ResponseID, decision, true)
			return turn, decision, nil
		}
	}
	judgment, classifyErr := s.classify(ctx, in.Request, turn, features, in.Models)
	if classifyErr == nil {
		s.traceClassifier(turn.ResponseID, judgment)
		decisionInput.Judgment, decisionInput.TaskType = &judgment, judgment.TaskType
		decisionInput.Floor = router.FloorFromJudgment(&judgment, decisionInput.Pin, fallbackTier(in.SafeFallbackTier))
	} else {
		s.trace("route.classifier.fallback", "response_id", turn.ResponseID, "error_kind", errorKind(classifyErr))
		decisionInput.Floor = router.FloorFromJudgment(nil, decisionInput.Pin, fallbackTier(in.SafeFallbackTier))
	}
	if turn.Floor.Valid() && turn.Floor > decisionInput.Floor {
		decisionInput.Floor = turn.Floor
	}
	decision, err := s.policy.Decide(decisionInput)
	if err != nil {
		s.traceDecision(turn.ResponseID, decision, false)
		return conversation.Turn{}, router.Decision{}, err
	}
	s.traceDecision(turn.ResponseID, decision, true)
	return turn, decision, nil
}

func (s *Service) streamCandidates(in Input, initial router.Decision) []router.Decision {
	decisions := []router.Decision{initial}
	allow := streamEscalationAllowed(in)
	if !allow {
		return decisions
	}
	if in.Request.Model != automaticModel {
		return decisions
	}
	// A transport failure before the first frame may be transient even when no
	// other model can satisfy the request. Bound the original candidate to one
	// re-open before considering equal-or-stronger alternatives.
	decisions = append(decisions, initial)
	rejected := make(map[string]bool, len(initial.Rejections))
	for _, rejection := range initial.Rejections {
		rejected[rejection.ModelID] = true
	}
	for _, score := range initial.Candidates {
		if score.ModelID == initial.ModelID || rejected[score.ModelID] {
			continue
		}
		for _, model := range in.Models {
			if model.ID != score.ModelID || model.Provider != score.Provider || model.Tier < initial.Tier {
				continue
			}
			candidate := initial
			candidate.ModelID, candidate.Provider, candidate.Tier = model.ID, model.Provider, model.Tier
			candidate.Reasons = append(append([]string(nil), initial.Reasons...), "retry selected an equal-or-stronger candidate before stream emission")
			decisions = append(decisions, candidate)
			break
		}
	}
	return decisions
}

func streamEscalationAllowed(in Input) bool {
	allow := in.Request.Model == automaticModel
	if in.AllowEscalation != nil {
		allow = *in.AllowEscalation
	}
	return allow
}

// explicitStreamCandidates reconstructs the unconstrained policy candidate
// set only after an opted-in concrete-model stream fails before emission.
func (s *Service) explicitStreamCandidates(in Input, turn conversation.Turn, initial router.Decision) ([]router.Decision, error) {
	if in.Request.Model == automaticModel || !streamEscalationAllowed(in) {
		return nil, nil
	}
	features := normalizedFeatures(in.Features, in.Request, turn)
	floor := initial.Tier
	if turn.Floor.Valid() && turn.Floor > floor {
		floor = turn.Floor
	}
	decision, err := s.policy.Decide(router.DecisionInput{
		Features: features, Models: in.Models, Floor: floor, MinTier: in.MinTier, MaxTier: in.MaxTier,
		RequiredProvider:    in.RequiredProvider,
		ProviderCredentials: in.ProviderCredentials, ProviderAvailability: providerAvailability(in.Models, in.ProviderAvailability, s.providers),
	})
	if err != nil {
		return nil, err
	}
	rejected := make(map[string]bool, len(decision.Rejections))
	for _, rejection := range decision.Rejections {
		rejected[rejection.ModelID] = true
	}
	var alternatives []router.Decision
	for _, score := range decision.Candidates {
		if score.ModelID == initial.ModelID || rejected[score.ModelID] {
			continue
		}
		for _, model := range in.Models {
			if model.ID != score.ModelID || model.Provider != score.Provider || model.Tier < initial.Tier {
				continue
			}
			candidate := decision
			candidate.ModelID, candidate.Provider, candidate.Tier = model.ID, model.Provider, model.Tier
			candidate.Reasons = append(append([]string(nil), decision.Reasons...), "retry selected an equal-or-stronger candidate after explicit stream failure")
			alternatives = append(alternatives, candidate)
			break
		}
	}
	return alternatives, nil
}

func (s *Service) failStreamAttempt(ctx context.Context, attempt conversation.Attempt, observedProviderRequestID string, cause error) error {
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	if observedProviderRequestID == "" {
		observedProviderRequestID = providerRequestID(cause)
	}
	return s.conversations.FailAttempt(persistCtx, attempt, observedProviderRequestID, cause)
}

func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func retryableStreamError(err error) bool {
	var providerErr *provider.Error
	if !errors.As(err, &providerErr) {
		return false
	}
	switch providerErr.Kind {
	case provider.ErrorRetryable, provider.ErrorRateLimit, provider.ErrorOverloaded:
		return true
	default:
		return false
	}
}

func nextTier(tier domain.Tier) domain.Tier {
	if tier >= domain.T6 {
		return domain.T6
	}
	return tier + 1
}

func drainStream(ctx context.Context, stream provider.Stream, responseID, model, selectedProvider string, writer EventWriter) (inference.Result, inference.Event, bool, error) {
	accumulator := streamAccumulator{model: model, text: make(map[string]int), functions: make(map[string]int), customTools: make(map[string]int)}
	var completion inference.Event
	emitted := false
	for {
		event, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				result := accumulator.result(responseID)
				if len(accumulator.pendingNative) != 0 || accumulator.currentNative != nil {
					return result, completion, emitted, inference.Invalid("output", "incomplete signed thinking continuation")
				}
				if !accumulator.terminal || !provider.IsSuccessfulCompletion(result.Status) {
					return result, completion, emitted, provider.UnsuccessfulCompletionError(result.ProviderRequestID)
				}
				if completion.Type == "" {
					completion = inference.Event{Type: "response.completed", ResponseID: responseID, Status: result.Status, Usage: result.Usage, ProviderRequestID: result.ProviderRequestID}
				}
				return result, completion, emitted, nil
			}
			return accumulator.result(responseID), completion, emitted, err
		}
		rawEvent := event
		if selectedProvider == "anthropic" {
			if nativeWriter, ok := writer.(interface{ MessagesEvents() bool }); ok && nativeWriter.MessagesEvents() {
				if err := accumulator.observeNative(rawEvent); err != nil {
					return accumulator.result(responseID), completion, emitted, err
				}
			}
		}
		event, visible := canonicalStreamEvent(event)
		accumulator.observe(event)
		if event.Status != "" && event.Status != "in_progress" && !provider.IsSuccessfulCompletion(event.Status) {
			return accumulator.result(responseID), completion, emitted, provider.UnsuccessfulCompletionError(accumulator.providerRequestID)
		}
		if nativeWriter, ok := writer.(interface{ MessagesEvents() bool }); ok && nativeWriter.MessagesEvents() {
			if strings.HasPrefix(rawEvent.Type, "content_block_") || rawEvent.Type == "message_start" {
				event, visible = rawEvent, true
			} else if rawEvent.Type == "message_delta" || rawEvent.Type == "message_stop" {
				visible = false
			}
		}
		if !visible {
			continue
		}
		event.ResponseID = responseID
		if event.Type == "response.completed" {
			completion = event
			continue
		}
		emitted = true
		if err := writer.WriteEvent(ctx, event); err != nil {
			return accumulator.result(responseID), completion, emitted, err
		}
	}
}

// canonicalStreamEvent translates adapter lifecycle names into the portable
// Responses event vocabulary. Provider-native payload bytes remain internal.
func canonicalStreamEvent(event inference.Event) (inference.Event, bool) {
	switch event.Type {
	case "content_block_delta":
		if event.Delta != "" {
			event.Type = "response.output_text.delta"
			return event, true
		}
		if event.ArgumentsDelta != "" {
			event.Type = "response.function_call_arguments.delta"
			return event, true
		}
		return event, false
	case "message_start":
		event.Type = "response.in_progress"
		return event, true
	case "message_delta", "message_stop":
		if event.Status == "" {
			return event, false
		}
		event.Type = "response.completed"
		return event, true
	default:
		if strings.HasPrefix(event.Type, "response.") {
			return event, true
		}
		if event.Status == "" {
			return event, false
		}
		if event.Status == "in_progress" {
			event.Type = "response.in_progress"
		} else {
			event.Type = "response.completed"
		}
		return event, true
	}
}

type streamAccumulator struct {
	model, status, providerRequestID, stopReason string
	usage                                        inference.Usage
	terminal                                     bool
	items                                        []inference.Item
	outputIndexes                                []int
	text, functions                              map[string]int
	customTools                                  map[string]int
	currentNative                                *nativeThinkingBlock
	pendingNative                                []nativeThinkingBlock
	nativeFor                                    map[int][]nativeThinkingBlock
}

type nativeThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

func (a *streamAccumulator) observeNative(event inference.Event) error {
	switch event.Type {
	case "content_block_start":
		if event.ItemType == "thinking" {
			a.currentNative = &nativeThinkingBlock{Type: "thinking"}
		}
	case "content_block_delta":
		if a.currentNative != nil {
			a.currentNative.Thinking += event.Thinking
			a.currentNative.Signature += event.Signature
		}
	case "content_block_stop":
		if a.currentNative != nil {
			if a.currentNative.Signature == "" {
				return inference.Invalid("output", "incomplete signed thinking continuation")
			}
			a.pendingNative = append(a.pendingNative, *a.currentNative)
			a.currentNative = nil
		}
	}
	return nil
}

func (a *streamAccumulator) attachNative(index int) {
	if len(a.pendingNative) == 0 {
		return
	}
	if a.nativeFor == nil {
		a.nativeFor = make(map[int][]nativeThinkingBlock)
	}
	a.nativeFor[index] = append([]nativeThinkingBlock(nil), a.pendingNative...)
	a.pendingNative = nil
}

func (a *streamAccumulator) observe(event inference.Event) {
	if event.ProviderRequestID != "" {
		a.providerRequestID = event.ProviderRequestID
	}
	if event.Status != "" {
		a.status = event.Status
		if event.Status != "in_progress" {
			a.terminal = true
		}
	}
	if event.StopReason != "" {
		a.stopReason = event.StopReason
	}
	if event.Usage.Known {
		a.usage = event.Usage
	}
	switch event.Type {
	case "response.output_text.delta":
		a.appendText(event, false)
	case "response.output_text.done":
		a.appendText(event, true)
	case "response.function_call_arguments.delta":
		a.appendFunctionArguments(event, false)
	case "response.function_call_arguments.done":
		a.appendFunctionArguments(event, true)
	case "response.output_item.done":
		switch event.ItemType {
		case "message":
			event.Delta = event.ItemText
			a.appendText(event, true)
		case "reasoning":
			a.appendReasoning(event)
		case "function_call":
			a.appendFunctionArguments(event, true)
		case "custom_tool_call":
			a.appendCustomToolCall(event)
		}
	}
}

func (a *streamAccumulator) appendReasoning(event inference.Event) {
	if event.ItemID == "" || len(event.EncryptedContent) == 0 {
		return
	}
	for _, item := range a.items {
		if item.ID == event.ItemID {
			return
		}
	}
	a.appendItem(inference.Item{
		ID: event.ItemID, Type: "reasoning", Summary: append(json.RawMessage(nil), event.Summary...), EncryptedContent: append(json.RawMessage(nil), event.EncryptedContent...),
	}, event.OutputIndex)
}

func (a *streamAccumulator) appendItem(item inference.Item, outputIndex int) int {
	a.items = append(a.items, item)
	a.outputIndexes = append(a.outputIndexes, outputIndex)
	return len(a.items) - 1
}

func (a *streamAccumulator) appendCustomToolCall(event inference.Event) {
	key := event.CallID
	if key == "" {
		key = event.ItemID
	}
	if key == "" {
		return
	}
	if _, exists := a.customTools[key]; exists {
		return
	}
	a.customTools[key] = a.appendItem(inference.Item{
		ID: event.ItemID, Type: "custom_tool_call", CallID: event.CallID,
		Name: event.Name, Namespace: event.Namespace, Input: event.Input,
	}, event.OutputIndex)
}

func (a *streamAccumulator) appendText(event inference.Event, onlyIfAbsent bool) {
	key := event.ItemID
	if key == "" {
		key = "text"
	}
	index, ok := a.text[key]
	if !ok {
		index = a.appendItem(inference.Item{Type: "message", Role: "assistant"}, event.OutputIndex)
		a.text[key] = index
		a.attachNative(index)
	}
	if event.ItemID != "" {
		a.items[index].ID = event.ItemID
	}
	if onlyIfAbsent && ok {
		return
	}
	a.items[index].Text += event.Delta
}

func (a *streamAccumulator) appendFunctionArguments(event inference.Event, onlyIfAbsent bool) {
	index, ok := 0, false
	if event.ItemID != "" {
		index, ok = a.functions[event.ItemID]
	}
	if !ok && event.CallID != "" {
		index, ok = a.functions[event.CallID]
	}
	if event.ItemID == "" && event.CallID == "" {
		return
	}
	if !ok {
		index = a.appendItem(inference.Item{Type: "function_call"}, event.OutputIndex)
		a.attachNative(index)
	}
	if event.ItemID != "" {
		a.functions[event.ItemID] = index
		a.items[index].ID = event.ItemID
	}
	if event.CallID != "" {
		a.functions[event.CallID] = index
		a.items[index].CallID = event.CallID
	}
	if event.Name != "" {
		a.items[index].Name = event.Name
	}
	if event.Namespace != "" {
		a.items[index].Namespace = event.Namespace
	}
	if onlyIfAbsent && len(a.items[index].Arguments) != 0 {
		return
	}
	a.items[index].Arguments = append(a.items[index].Arguments, event.ArgumentsDelta...)
}

func (a *streamAccumulator) result(responseID string) inference.Result {
	status := a.status
	items := make([]inference.Item, 0, len(a.items))
	positions := make([]int, len(a.items))
	for index := range a.items {
		positions[index] = index
	}
	sort.SliceStable(positions, func(left, right int) bool {
		return a.outputIndexes[positions[left]] < a.outputIndexes[positions[right]]
	})
	for _, position := range positions {
		item := a.items[position]
		if item.Type == "function_call" && len(item.Arguments) != 0 && !json.Valid(item.Arguments) {
			continue
		}
		if prefix := a.nativeFor[position]; len(prefix) != 0 {
			content := map[string]any{"type": "text", "text": item.Text}
			if item.Type == "function_call" {
				content = map[string]any{"type": "tool_use", "id": item.CallID, "name": item.Name, "input": item.Arguments}
			}
			item.ProviderData, _ = json.Marshal(map[string]any{"anthropic_content_block": content, "anthropic_prefix": prefix})
			item.ContinuationProvider = "anthropic"
		}
		items = append(items, item)
	}
	return inference.Result{ID: responseID, Model: a.model, ProviderRequestID: a.providerRequestID, Status: status, StopReason: a.stopReason, Output: items, Usage: a.usage}
}
