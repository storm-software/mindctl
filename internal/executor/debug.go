package executor

import (
	"context"
	"errors"
	"log/slog"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
)

func (s *Service) trace(message string, attributes ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Debug(message, attributes...)
}

func (s *Service) traceFeatures(responseID string, features domain.RequestFeatures) {
	s.trace("route.features",
		"response_id", responseID,
		"input_tokens", features.InputTokens,
		"cached_input_tokens", features.CachedInputTokens,
		"context_tokens", features.ContextTokens,
		"max_output_tokens", features.MaxOutputTokens,
		"needs_text", features.NeedsText,
		"needs_images", features.NeedsImages,
		"needs_functions", features.NeedsFunctions,
		"needs_json_schema", features.NeedsJSONSchema,
		"needs_hosted_tools", features.NeedsHostedTools,
		"needs_native_tools", features.NeedsNativeTools,
		"hosted_tool_types", slog.AnyValue(features.HostedToolTypes),
	)
}

func (s *Service) traceClassifier(responseID string, judgment domain.ClassifierJudgment) {
	s.trace("route.classifier.completed",
		"response_id", responseID,
		"minimum_tier", judgment.MinimumTier.String(),
		"tier_confidence", judgment.TierConfidence,
		"task_type", string(judgment.TaskType),
		"task_type_confidence", judgment.TaskTypeConfidence,
		"reasoning_score", judgment.ReasoningScore,
		"reasoning_confidence", judgment.ReasoningConfidence,
		"coding_score", judgment.CodingScore,
		"coding_confidence", judgment.CodingConfidence,
		"blast_radius", judgment.BlastRadius,
		"blast_radius_confidence", judgment.BlastRadiusConfidence,
		"underspecified", judgment.Underspecified,
		"classifier", judgment.Classifier,
		"classifier_model", judgment.ResolvedModel,
		"classifier_revision", judgment.ModelRevision,
		"latency", judgment.Latency,
	)
}

func (s *Service) traceDecision(responseID string, decision router.Decision, selected bool) {
	s.trace("route.decision",
		"response_id", responseID,
		"selected", selected,
		"provider", decision.Provider,
		"model", decision.ModelID,
		"tier", decision.Tier.String(),
		"reasons", slog.AnyValue(decision.Reasons),
		"candidates", slog.AnyValue(decision.Candidates),
		"rejections", slog.AnyValue(decision.Rejections),
	)
}

func errorKind(err error) string {
	var providerErr *provider.Error
	switch {
	case errors.As(err, &providerErr):
		return string(providerErr.Kind)
	case errors.Is(err, classifier.ErrUnavailable):
		return "classifier_unavailable"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "internal"
	}
}

func errorDiagnostic(err error) []any {
	var providerErr *provider.Error
	if !errors.As(err, &providerErr) {
		return nil
	}
	return []any{
		"upstream_status", providerErr.Status,
		"upstream_code", providerErr.UpstreamCode,
		"upstream_param", providerErr.UpstreamParam,
		"upstream_message", providerErr.UpstreamMessage,
	}
}
