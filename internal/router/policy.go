package router

import (
	"fmt"
	"sort"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
)

// SignalFloor raises the required tier when a Jev score reaches Threshold.
// Score scales are supplied by the classifier contract; rules have no defaults.
type SignalFloor struct {
	Threshold float64
	Floor     domain.Tier
}

// PolicyConfig contains offline policy parameters. Costs are USD; the latency
// penalty is USD per second of model P95 latency. Zero maxima disable those
// limits. MinJevConfidence defaults to 0.7 when zero. Score-based rules only
// raise floors, even when the separate tier-choice confidence is low.
type PolicyConfig struct {
	FailureEscalationCost, LatencyPenaltyPerSecond                      float64
	MinSuccessProbability, MaxDirectCost, MaxExpectedCost               float64
	MaxLatency                                                          time.Duration
	MinJevConfidence                                                    float64
	ReasoningFloors, CodingFloors, RiskFloors, UnderspecificationFloors []SignalFloor
}

// Policy has an immutable configuration snapshot; Decide performs no I/O.
type Policy struct{ config PolicyConfig }

func NewPolicy(cfg PolicyConfig) *Policy {
	if cfg.MinJevConfidence == 0 {
		cfg.MinJevConfidence = .7
	}
	cfg.ReasoningFloors = append([]SignalFloor(nil), cfg.ReasoningFloors...)
	cfg.CodingFloors = append([]SignalFloor(nil), cfg.CodingFloors...)
	cfg.RiskFloors = append([]SignalFloor(nil), cfg.RiskFloors...)
	cfg.UnderspecificationFloors = append([]SignalFloor(nil), cfg.UnderspecificationFloors...)
	return &Policy{config: cfg}
}

// DecisionInput supplies already extracted requirements. Floor includes any
// deterministic capability floor. Provider maps have EligibleModels semantics.
// A nil Judgment adds no signal; Jev-unavailability fallback is caller-owned.
type DecisionInput struct {
	Features                                  domain.RequestFeatures
	Models                                    []domain.Model
	Floor                                     domain.Tier
	TaskType                                  domain.TaskType
	Pin                                       *Pin
	MinTier, MaxTier                          *domain.Tier
	Judgment                                  *domain.JevJudgment
	ProviderCredentials, ProviderAvailability map[string]bool
}

type Pin struct {
	ModelID, Provider string
	Floor             domain.Tier
}

type Decision struct {
	Tier              domain.Tier
	ModelID, Provider string
	Reasons           []string
	Candidates        []CandidateScore
	Rejections        []Rejection
}

const (
	RejectSuccess      RejectionCode = "success_probability"
	RejectDirectCost   RejectionCode = "direct_cost"
	RejectExpectedCost RejectionCode = "expected_cost"
	RejectLatency      RejectionCode = "latency"
	RejectEstimate     RejectionCode = "invalid_estimate"
)

// Decide preserves hard eligibility failures and all valid candidate estimates,
// sorted by expected cost, direct cost, latency, then stable model.Order. A
// compatible pin takes precedence over cost optimization. On failure, callers
// should retain the returned decision for its reasons and rejections.
func (p *Policy) Decide(in DecisionInput) (Decision, error) {
	var decision Decision
	if err := p.validate(in); err != nil {
		return decision, err
	}
	floor := in.Floor
	decision.Reasons = append(decision.Reasons, fmt.Sprintf("request floor: %s", floor))
	raise := func(next domain.Tier, source string) {
		if next > floor {
			decision.Reasons = append(decision.Reasons, fmt.Sprintf("%s raised floor from %s to %s", source, floor, next))
			floor = next
		}
	}
	// Keep an invalid input floor intact so eligibility remains authoritative.
	if in.MinTier != nil && in.MinTier.Valid() {
		raise(*in.MinTier, "caller minimum")
	}
	if in.Pin != nil {
		raise(in.Pin.Floor, "conversation pin")
		for _, model := range in.Models {
			if matchesPin(model, in.Pin) {
				raise(model.Tier, "pinned model capability")
			}
		}
	}
	if j := in.Judgment; j != nil {
		if j.TierConfidence >= p.config.MinJevConfidence {
			raise(j.MinimumTier, "Jev minimum")
		} else {
			decision.Reasons = append(decision.Reasons, "Jev tier choice ignored below confidence threshold; existing floor retained")
		}
		for _, signal := range []struct {
			name  string
			value float64
			rules []SignalFloor
		}{
			{"reasoning", j.ReasoningScore, p.config.ReasoningFloors},
			{"coding", j.CodingScore, p.config.CodingFloors},
			{"risk", j.BlastRadius, p.config.RiskFloors},
			{"underspecification", j.Underspecified, p.config.UnderspecificationFloors},
		} {
			for _, rule := range signal.rules {
				if signal.value >= rule.Threshold {
					raise(rule.Floor, signal.name)
				}
			}
		}
	}
	eligible, rejections := EligibleModels(EligibilityInput{Models: in.Models, Features: in.Features, Floor: floor, MinTier: in.MinTier, MaxTier: in.MaxTier, ProviderCredentials: in.ProviderCredentials, ProviderAvailability: in.ProviderAvailability})
	decision.Rejections = rejections
	type candidate struct {
		model   domain.Model
		score   CandidateScore
		allowed bool
	}
	candidates := make([]candidate, 0, len(eligible))
	for _, model := range eligible {
		score, err := scoreCandidate(model, in.Features, in.TaskType, p.config)
		if err != nil {
			decision.Rejections = append(decision.Rejections, Rejection{ModelID: model.ID, Code: RejectEstimate, Codes: []RejectionCode{RejectEstimate}, Reasons: []string{err.Error()}})
			continue
		}
		r := Rejection{ModelID: model.ID}
		add := func(code RejectionCode, reason string) {
			r.Codes = append(r.Codes, code)
			r.Reasons = append(r.Reasons, reason)
		}
		if score.SuccessProbability < p.config.MinSuccessProbability {
			add(RejectSuccess, "success prior is below the configured minimum")
		}
		if p.config.MaxDirectCost > 0 && score.DirectCost > p.config.MaxDirectCost {
			add(RejectDirectCost, "direct cost exceeds the configured maximum")
		}
		if p.config.MaxExpectedCost > 0 && score.ExpectedTotalCost > p.config.MaxExpectedCost {
			add(RejectExpectedCost, "expected total cost exceeds the configured maximum")
		}
		if p.config.MaxLatency > 0 && score.Latency > p.config.MaxLatency {
			add(RejectLatency, "latency exceeds the configured maximum")
		}
		if len(r.Codes) > 0 {
			r.Code = r.Codes[0]
			decision.Rejections = append(decision.Rejections, r)
		}
		candidates = append(candidates, candidate{model: model, score: score, allowed: len(r.Codes) == 0})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.score.ExpectedTotalCost != b.score.ExpectedTotalCost {
			return a.score.ExpectedTotalCost < b.score.ExpectedTotalCost
		}
		if a.score.DirectCost != b.score.DirectCost {
			return a.score.DirectCost < b.score.DirectCost
		}
		if a.score.Latency != b.score.Latency {
			return a.score.Latency < b.score.Latency
		}
		return a.model.Order < b.model.Order
	})
	selected := -1
	for i, candidate := range candidates {
		decision.Candidates = append(decision.Candidates, candidate.score)
		if candidate.allowed && selected < 0 {
			selected = i
		}
	}
	for _, r := range decision.Rejections {
		for _, reason := range r.Reasons {
			decision.Reasons = append(decision.Reasons, fmt.Sprintf("rejected %s: %s", r.ModelID, reason))
		}
	}
	if in.Pin != nil {
		reused := false
		for i, candidate := range candidates {
			if candidate.allowed && matchesPin(candidate.model, in.Pin) {
				selected = i
				reused = true
				break
			}
		}
		if reused {
			decision.Reasons = append(decision.Reasons, "compatible conversation pin reused")
		} else {
			decision.Reasons = append(decision.Reasons, "conversation pin unavailable or incompatible with current constraints; replacement must meet or exceed its floor")
		}
	}
	if selected < 0 {
		return decision, &NoEligibleModelError{Rejections: cloneRejections(decision.Rejections)}
	}
	chosen := candidates[selected].model
	decision.Tier, decision.ModelID, decision.Provider = chosen.Tier, chosen.ID, chosen.Provider
	decision.Reasons = append(decision.Reasons, fmt.Sprintf("selected %s/%s at %s", chosen.Provider, chosen.ID, chosen.Tier))
	return decision, nil
}

func matchesPin(model domain.Model, pin *Pin) bool {
	return model.ID == pin.ModelID && (pin.Provider == "" || model.Provider == pin.Provider)
}

func (p *Policy) validate(in DecisionInput) error {
	cfg := p.config
	for _, v := range []float64{cfg.FailureEscalationCost, cfg.LatencyPenaltyPerSecond, cfg.MaxDirectCost, cfg.MaxExpectedCost} {
		if !nonnegativeFinite(v) {
			return fmt.Errorf("policy costs must be finite and nonnegative")
		}
	}
	if !probability(cfg.MinSuccessProbability) || !probability(cfg.MinJevConfidence) || cfg.MaxLatency < 0 {
		return fmt.Errorf("invalid policy probability or latency")
	}
	for _, rules := range [][]SignalFloor{cfg.ReasoningFloors, cfg.CodingFloors, cfg.RiskFloors, cfg.UnderspecificationFloors} {
		for _, r := range rules {
			if !nonnegativeFinite(r.Threshold) || !r.Floor.Valid() {
				return fmt.Errorf("invalid signal floor rule")
			}
		}
	}
	f := in.Features
	if f.InputTokens < 0 || f.CachedInputTokens < 0 || f.ContextTokens < 0 || f.MaxOutputTokens < 0 {
		return fmt.Errorf("request token counts must be nonnegative")
	}
	if in.Pin != nil && !in.Pin.Floor.Valid() {
		return fmt.Errorf("invalid conversation pin floor")
	}
	if j := in.Judgment; j != nil {
		if !j.MinimumTier.Valid() || !probability(j.TierConfidence) {
			return fmt.Errorf("invalid Jev tier or confidence")
		}
		for _, v := range []float64{j.ReasoningScore, j.CodingScore, j.BlastRadius, j.Underspecified} {
			if !nonnegativeFinite(v) {
				return fmt.Errorf("invalid Jev score")
			}
		}
		for _, confidence := range []float64{j.ReasoningConfidence, j.CodingConfidence, j.BlastRadiusConfidence} {
			if !probability(confidence) {
				return fmt.Errorf("invalid Jev score confidence")
			}
		}
	}
	return nil
}
