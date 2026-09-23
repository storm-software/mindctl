package router

import (
	"fmt"
	"math"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
)

// CandidateScore is the reproducible USD estimate for a hard-eligible model,
// including models subsequently rejected by policy budgets. EscalationCost is
// the conditional cost of escalation; ExpectedTotalCost weights it by failure.
type CandidateScore struct {
	ModelID, Provider                              string
	DirectCost, FailureProbability, EscalationCost float64
	SuccessProbability                             float64
	LatencyPenalty, ExpectedTotalCost              float64
	Latency                                        time.Duration
}

func scoreCandidate(model domain.Model, features domain.RequestFeatures, task domain.TaskType, cfg PolicyConfig) (CandidateScore, error) {
	score := CandidateScore{ModelID: model.ID, Provider: model.Provider, Latency: model.LatencyP95, EscalationCost: cfg.FailureEscalationCost}
	prices := []float64{model.Pricing.InputPerMillion, model.Pricing.CachedInputPerMillion, model.Pricing.OutputPerMillion, model.Pricing.PerRequestUSD}

	for _, price := range prices {
		if !nonnegativeFinite(price) {
			return score, fmt.Errorf("model price must be finite and nonnegative")
		}
	}

	prior := model.DefaultSuccessPrior
	if taskPrior, ok := model.SuccessPriors[task]; ok {
		prior = taskPrior
	}

	if !probability(prior) || model.LatencyP95 < 0 {
		return score, fmt.Errorf("model success prior or latency is invalid")
	}

	cached := min(features.CachedInputTokens, features.InputTokens)
	uncached := features.InputTokens - cached
	score.DirectCost = float64(uncached)/1e6*prices[0] + float64(cached)/1e6*prices[1] + float64(features.MaxOutputTokens)/1e6*prices[2] + prices[3]
	score.FailureProbability = 1 - prior
	score.SuccessProbability = prior
	score.LatencyPenalty = model.LatencyP95.Seconds() * cfg.LatencyPenaltyPerSecond
	score.ExpectedTotalCost = score.DirectCost + score.FailureProbability*score.EscalationCost + score.LatencyPenalty

	if !nonnegativeFinite(score.ExpectedTotalCost) {
		return score, fmt.Errorf("model cost estimate overflows")
	}
	return score, nil
}

func nonnegativeFinite(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func probability(value float64) bool { return nonnegativeFinite(value) && value <= 1 }
