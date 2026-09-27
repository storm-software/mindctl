package executor

import (
	"context"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/headroom"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/savings"
)

// recordSavings prices a committed attempt against its baseline. The result
// is already durable, so a telemetry failure is traced rather than returned.
func (s *Service) recordSavings(
	ctx context.Context,
	in Input,
	attempt conversation.Attempt,
	served domain.Model,
	usage inference.Usage,
	compression headroom.Metrics,
) {
	record := savings.Compute(savings.Input{
		Served:      served,
		Baseline:    savings.Baseline(in.Models, in.Request.Model, in.SavingsBaseline),
		Usage:       usage,
		Compression: savings.Compression{TokensBefore: compression.TokensBefore, TokensSaved: compression.TokensSaved},
	})
	if err := s.conversations.RecordSavings(ctx, attempt, record); err != nil {
		s.trace("route.savings.failed", "response_id", attempt.ResponseID, "attempt_id", attempt.ID, "error_kind", errorKind(err))
		return
	}
	s.trace("route.savings.recorded",
		"response_id", attempt.ResponseID,
		"attempt_id", attempt.ID,
		"baseline_provider", record.BaselineProvider,
		"baseline_model", record.BaselineModelID,
		"actual_cost_usd", record.ActualCost,
		"baseline_cost_usd", record.BaselineCost,
		"routing_savings_usd", record.RoutingSavings(),
		"compression_tokens_saved", record.CompressionTokensSaved,
		"compression_savings_usd", record.CompressionSavings,
	)
}
