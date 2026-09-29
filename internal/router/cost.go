package router

import (
	"fmt"
	"math"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
)

const (
	feedbackMinimumSamples int64   = 10
	feedbackPriorWeight    float64 = 20
)

// CandidateScore is the reproducible USD estimate for a hard-eligible model,
// including models subsequently rejected by policy budgets. DirectCost is the
// worst case for this request, with max_tokens of output. ExpectedTurnCost uses
// the expected output instead, and HorizonCost prices the session turns a
// sticky model is expected to serve next. EscalationCost is the conditional
// cost of escalation; ExpectedTotalCost weights it by failure and ranks
// candidates.
type CandidateScore struct {
	ModelID, Provider                                string
	DirectCost, FailureProbability, EscalationCost   float64
	ExpectedTurnCost, HorizonCost                    float64
	ConfiguredSuccessProbability, SuccessProbability float64
	FeedbackThumbsUpCount, FeedbackRatingCount       int64
	FeedbackApplied                                  bool
	LatencyPenalty, ExpectedTotalCost                float64
	Latency                                          time.Duration
}

// EffectiveSuccessProbability blends mature feedback evidence into the
// configured model prior. Smaller samples leave the configured prior intact.
func EffectiveSuccessProbability(model domain.Model, task domain.TaskType, evidence FeedbackEvidence) (configured, effective float64, applied bool, err error) {
	configured = model.DefaultSuccessPrior
	if taskPrior, ok := model.SuccessPriors[task]; ok {
		configured = taskPrior
	}
	if !probability(configured) {
		return configured, 0, false, fmt.Errorf("model success prior is invalid")
	}
	if evidence.ThumbsUpCount < 0 || evidence.RatingCount < 0 || evidence.ThumbsUpCount > evidence.RatingCount {
		return configured, 0, false, fmt.Errorf("feedback counts are invalid")
	}
	if evidence.RatingCount < feedbackMinimumSamples {
		return configured, configured, false, nil
	}
	effective = (configured*feedbackPriorWeight + float64(evidence.ThumbsUpCount)) /
		(feedbackPriorWeight + float64(evidence.RatingCount))
	return configured, effective, true, nil
}

func scoreCandidate(model domain.Model, features domain.RequestFeatures, task domain.TaskType, evidence FeedbackEvidence, cfg PolicyConfig, session *SessionEstimate) (CandidateScore, error) {
	score := CandidateScore{ModelID: model.ID, Provider: model.Provider, Latency: model.LatencyP95, EscalationCost: cfg.FailureEscalationCost}
	prices := []float64{model.Pricing.InputPerMillion, model.Pricing.CachedInputPerMillion, model.Pricing.OutputPerMillion, model.Pricing.PerRequestUSD}

	if session != nil {
		prices = append(prices, model.Pricing.CacheWriteInputPerMillion)
	}
	for _, price := range prices {
		if !nonnegativeFinite(price) {
			return score, fmt.Errorf("model price must be finite and nonnegative")
		}
	}

	configured, prior, applied, err := EffectiveSuccessProbability(model, task, evidence)
	if err != nil || model.LatencyP95 < 0 {
		return score, fmt.Errorf("model success prior or latency is invalid")
	}
	score.ConfiguredSuccessProbability = configured
	score.FeedbackThumbsUpCount = evidence.ThumbsUpCount
	score.FeedbackRatingCount = evidence.RatingCount
	score.FeedbackApplied = applied

	cached := min(features.CachedInputTokens, features.InputTokens)
	uncached := features.InputTokens - cached
	score.DirectCost = float64(uncached)/1e6*prices[0] + float64(cached)/1e6*prices[1] + float64(features.MaxOutputTokens)/1e6*prices[2] + prices[3]
	output := features.MaxOutputTokens
	if features.ExpectedOutputTokens > 0 {
		output = features.ExpectedOutputTokens
	}
	if session == nil {
		// Same expression as DirectCost, so it is bit-identical without an
		// expected output estimate.
		score.ExpectedTurnCost = float64(uncached)/1e6*prices[0] + float64(cached)/1e6*prices[1] + float64(output)/1e6*prices[2] + prices[3]
	} else {
		outputCost := float64(output)/1e6*prices[2] + prices[3]
		// A newly pinned model writes the whole prompt to its cache, then
		// reads the cached share and writes the rest on each later turn.
		input, write := float64(features.InputTokens)/1e6, prices[4]
		warmInput := session.CacheHitRatio*input*prices[1] + (1-session.CacheHitRatio)*input*write
		score.ExpectedTurnCost = input*write + outputCost
		score.HorizonCost = float64(session.HorizonTurns) * (warmInput + outputCost)
	}
	score.FailureProbability = 1 - prior
	score.SuccessProbability = prior
	score.LatencyPenalty = model.LatencyP95.Seconds() * cfg.LatencyPenaltyPerSecond
	score.ExpectedTotalCost = score.ExpectedTurnCost + score.HorizonCost + score.FailureProbability*score.EscalationCost + score.LatencyPenalty

	if !nonnegativeFinite(score.ExpectedTotalCost) {
		return score, fmt.Errorf("model cost estimate overflows")
	}
	return score, nil
}

func nonnegativeFinite(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func probability(value float64) bool { return nonnegativeFinite(value) && value <= 1 }
