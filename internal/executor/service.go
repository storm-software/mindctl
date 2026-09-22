// Package executor composes routing, conversations, and provider adapters for
// one durable non-streaming gateway attempt.
package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
)

const automaticModel = "mindctl-auto"

// Input is the fully authenticated, canonical request plus the immutable
// routing snapshot supplied by application wiring. Features may add gateway
// requirements that are not represented in the portable request, such as a
// provider-hosted tool type.
type Input struct {
	ClientID                                  string
	Request                                   inference.Request
	Models                                    []domain.Model
	Features                                  domain.RequestFeatures
	MinTier, MaxTier                          *domain.Tier
	SafeFallbackTier                          domain.Tier
	AllowEscalation                           *bool
	ProviderCredentials, ProviderAvailability map[string]bool
}

// Service owns no provider or storage state; its dependencies are injected so
// selection stays deterministic and provider I/O remains testable.
type Service struct {
	classifier    classifier.Classifier
	policy        *router.Policy
	providers     *provider.Registry
	conversations conversation.Service
}

// New composes the dependencies needed for one routed execution path.
func New(classifier classifier.Classifier, policy *router.Policy, providers *provider.Registry, conversations conversation.Service) *Service {
	return &Service{classifier: classifier, policy: policy, providers: providers, conversations: conversations}
}

// Output is returned only after the encrypted result and conversation pin have
// committed successfully. Result.ID is always the gateway-owned response ID.
type Output struct {
	Result    inference.Result
	Decision  router.Decision
	AttemptID string
}

// Execute performs exactly one non-streaming provider attempt. It deliberately
// does not retry, verify, or escalate; those behaviors are layered on later.
func (s *Service) Execute(ctx context.Context, in Input) (Output, error) {
	if err := inference.ValidateRequest(in.Request); err != nil {
		return Output{}, err
	}
	if in.Request.Stream {
		return Output{}, inference.Invalid("stream", "requires the streaming executor")
	}
	if s == nil || s.policy == nil || s.providers == nil || s.conversations == nil {
		return Output{}, errors.New("executor: dependencies are required")
	}

	turn, err := s.conversations.Start(ctx, in.ClientID, in.Request)
	if err != nil {
		return Output{}, err
	}
	features := normalizedFeatures(in.Features, in.Request, turn)
	availability := providerAvailability(in.Models, in.ProviderAvailability, s.providers)
	decisionInput := router.DecisionInput{
		Features: features, Models: in.Models, MinTier: in.MinTier, MaxTier: in.MaxTier,
		ProviderCredentials: in.ProviderCredentials, ProviderAvailability: availability,
	}

	automatic := in.Request.Model == automaticModel
	var decision router.Decision
	if !automatic {
		// Explicit requests may replace a pin only at or above the persisted
		// conversation floor. The policy then validates that one exact model.
		decisionInput.ModelID = in.Request.Model
		decisionInput.Floor = turn.Floor
		decision, err = s.policy.Decide(decisionInput)
		if err != nil {
			return Output{}, explicitError(in.Request.Model, decision, err)
		}
	} else {
		pin := turnPin(turn)
		if pin != nil {
			decisionInput.Pin = pin
			decisionInput.Floor = pin.Floor
			// A policy-confirmed compatible pin is deterministic and does not
			// need a classifier call.
			decision, err = s.policy.Decide(decisionInput)
			if err == nil && decision.ModelID == pin.ModelID && decision.Provider == pin.Provider {
				return s.executeDecision(ctx, in, turn, decision)
			}
		}

		judgment, classifyErr := s.classify(ctx, in.Request, turn, features, in.Models)
		if classifyErr == nil {
			decisionInput.Judgment = &judgment
			decisionInput.Floor = router.FloorFromJudgment(&judgment, decisionInput.Pin, fallbackTier(in.SafeFallbackTier))
			decisionInput.TaskType = judgment.TaskType
		} else {
			decisionInput.Floor = router.FloorFromJudgment(nil, decisionInput.Pin, fallbackTier(in.SafeFallbackTier))
		}
		decision, err = s.policy.Decide(decisionInput)
		if err != nil {
			return Output{}, err
		}
	}
	return s.executeDecision(ctx, in, turn, decision)
}

func (s *Service) executeDecision(ctx context.Context, in Input, turn conversation.Turn, decision router.Decision) (Output, error) {
	model, ok := selectedModel(in.Models, decision)
	if !ok {
		return Output{}, fmt.Errorf("executor: selected model %q is not configured", decision.ModelID)
	}
	adapter, ok := s.providers.Get(decision.Provider)
	if !ok {
		return Output{}, fmt.Errorf("executor: provider %q is unavailable", decision.Provider)
	}
	attempt, err := s.conversations.BeginAttempt(ctx, turn, decision)
	if err != nil {
		return Output{}, err
	}
	request := in.Request
	request.ID = turn.ResponseID
	request.Model = decision.ModelID
	request.PreviousResponseID = ""
	request.Input = turn.TranscriptFor(decision.Provider)
	result, err := adapter.Execute(ctx, model, request)
	if err != nil {
		if failErr := s.conversations.FailAttempt(ctx, attempt, providerRequestID(err), err); failErr != nil {
			return Output{}, errors.Join(err, failErr)
		}
		return Output{}, err
	}
	// Provider IDs remain encrypted attempt metadata; callers receive only the
	// gateway response identity and the actual selected concrete model.
	result.ID = turn.ResponseID
	result.Model = decision.ModelID
	pin := router.Pin{ModelID: decision.ModelID, Provider: decision.Provider, Floor: decision.Tier}
	if err := s.conversations.CommitResult(ctx, turn, pin, result); err != nil {
		return Output{}, err
	}
	callerResult := result
	callerResult.ProviderRequestID = ""
	return Output{Result: callerResult, Decision: decision, AttemptID: attempt.ID}, nil
}

func (s *Service) classify(ctx context.Context, request inference.Request, turn conversation.Turn, features domain.RequestFeatures, models []domain.Model) (domain.JevJudgment, error) {
	if s.classifier == nil {
		return domain.JevJudgment{}, classifier.ErrUnavailable
	}
	available := make([]string, 0, len(models))
	for _, model := range models {
		if model.Available {
			available = append(available, model.ID)
		}
	}
	return s.classifier.Classify(ctx, classifier.Input{
		Prompt:          classifierPrompt(request, turn),
		Features:        features,
		CurrentModel:    turn.Pin.ModelID,
		AvailableModels: available,
	})
}

func normalizedFeatures(supplied domain.RequestFeatures, request inference.Request, turn conversation.Turn) domain.RequestFeatures {
	features := supplied.Normalize()
	items := turn.Transcript
	if len(items) == 0 {
		items = request.Input
	}
	var tokens int64
	for _, item := range items {
		tokens += estimatedTokens(item.Text)
		if item.Type == "input_image" || len(item.ImageURL) != 0 {
			features.NeedsImages = true
		}
		if item.Type == "function_call" || item.Type == "function_call_output" {
			features.NeedsFunctions = true
		}
	}
	if request.Instructions != "" {
		tokens += estimatedTokens(request.Instructions)
	}
	if tokens > features.InputTokens {
		features.InputTokens = tokens
	}
	if tokens > features.ContextTokens {
		features.ContextTokens = tokens
	}
	if request.MaxOutputTokens > features.MaxOutputTokens {
		features.MaxOutputTokens = request.MaxOutputTokens
	}
	features.NeedsFunctions = features.NeedsFunctions || len(request.Tools) > 0
	features.NeedsJSONSchema = features.NeedsJSONSchema || request.TextFormat != nil
	for _, item := range items {
		if item.Text != "" {
			features.NeedsText = true
			break
		}
	}
	return features.Normalize()
}

func estimatedTokens(text string) int64 {
	if text == "" {
		return 0
	}
	return int64((utf8.RuneCountInString(text) + 3) / 4)
}

func providerAvailability(models []domain.Model, configured map[string]bool, providers *provider.Registry) map[string]bool {
	availability := make(map[string]bool, len(models))
	for _, model := range models {
		available := true
		if configured != nil {
			available = configured[model.Provider]
		}
		_, registered := providers.Get(model.Provider)
		availability[model.Provider] = available && registered
	}
	return availability
}

func turnPin(turn conversation.Turn) *router.Pin {
	if turn.Pin.ModelID == "" || turn.Pin.Provider == "" || !turn.Pin.Floor.Valid() {
		return nil
	}
	pin := turn.Pin
	if turn.Floor.Valid() && turn.Floor > pin.Floor {
		pin.Floor = turn.Floor
	}
	return &pin
}

func fallbackTier(tier domain.Tier) domain.Tier {
	if tier.Valid() {
		return tier
	}
	return domain.T4
}

func selectedModel(models []domain.Model, decision router.Decision) (domain.Model, bool) {
	for _, model := range models {
		if model.ID == decision.ModelID && model.Provider == decision.Provider {
			return model, true
		}
	}
	return domain.Model{}, false
}

func explicitError(modelID string, decision router.Decision, err error) error {
	for _, rejection := range decision.Rejections {
		if rejection.ModelID != modelID {
			continue
		}
		for _, code := range rejection.Codes {
			if code == router.RejectTools || code == router.RejectModality {
				return &provider.UnsupportedFeatureError{Feature: strings.Join(rejection.Reasons, "; ")}
			}
		}
	}
	return err
}

func providerRequestID(err error) string {
	var providerErr *provider.Error
	if errors.As(err, &providerErr) {
		return providerErr.RequestID
	}
	return ""
}

func classifierPrompt(request inference.Request, turn conversation.Turn) string {
	var prompt strings.Builder
	if request.Instructions != "" {
		prompt.WriteString(request.Instructions)
		prompt.WriteByte('\n')
	}
	items := turn.Transcript
	if len(items) == 0 {
		items = request.Input
	}
	for _, item := range items {
		if item.Role == "user" && item.Text != "" {
			prompt.WriteString(item.Text)
			prompt.WriteByte('\n')
		}
	}
	return strings.TrimSpace(prompt.String())
}
