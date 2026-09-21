package router

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
)

func policyModel(id string, tier domain.Tier, price, success float64) domain.Model {
	return domain.Model{ID: id, Provider: "provider", Tier: tier, Available: true,
		Pricing: domain.Pricing{PerRequestUSD: price}, DefaultSuccessPrior: success,
		Capabilities: domain.Capabilities{Text: true}, ContextWindow: 1000000, MaxOutputTokens: 1000000}
}

func TestPolicyChoosesLowerExpectedTotalCost(t *testing.T) {
	p := NewPolicy(PolicyConfig{FailureEscalationCost: 0.002})
	got, err := p.Decide(DecisionInput{Floor: domain.T4, TaskType: domain.TaskCoding,
		Models: []domain.Model{policyModel("cheap", domain.T4, 0.0001, 0.75), policyModel("reliable", domain.T4, 0.0004, 0.99)}})
	if err != nil || got.ModelID != "reliable" {
		t.Fatalf("decision=%+v err=%v", got, err)
	}
	if len(got.Candidates) != 2 || math.Abs(got.Candidates[0].ExpectedTotalCost-0.00042) > 1e-12 {
		t.Fatalf("scores=%+v", got.Candidates)
	}
}

func TestPolicyNeverDowngradesPin(t *testing.T) {
	got, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Floor: domain.T2,
		Pin: &Pin{ModelID: "strong", Floor: domain.T5}, Models: []domain.Model{
			policyModel("cheap", domain.T2, 0, 1), policyModel("strong", domain.T5, 1, 1)}})
	if err != nil || got.Tier < domain.T5 {
		t.Fatalf("decision=%+v err=%v", got, err)
	}
}

func TestPolicyTieBreaksByDirectCostLatencyThenConfigOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []domain.Model
		want   string
	}{
		{"direct", []domain.Model{policyModel("expensive", domain.T4, .5, 1), policyModel("lower-direct-cost", domain.T4, .25, .5)}, "lower-direct-cost"},
		{"latency", func() []domain.Model {
			a, b := policyModel("slow", domain.T4, .5, 1), policyModel("fast", domain.T4, .5, 1)
			a.LatencyP95 = 2 * time.Second
			b.LatencyP95 = time.Second
			return []domain.Model{a, b}
		}(), "fast"},
		{"config order", func() []domain.Model {
			a, b := policyModel("later", domain.T4, .5, 1), policyModel("earlier", domain.T4, .5, 1)
			a.Order = 2
			b.Order = 1
			return []domain.Model{a, b}
		}(), "earlier"},
		{"stable order", []domain.Model{policyModel("first", domain.T4, .5, 1), policyModel("second", domain.T4, .5, 1)}, "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewPolicy(PolicyConfig{FailureEscalationCost: .5}).Decide(DecisionInput{Models: tc.models})
			if err != nil || got.ModelID != tc.want {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
		})
	}
}

func TestPolicyPricesUncachedCachedOutputAndRequest(t *testing.T) {
	m := policyModel("priced", domain.T4, .25, 1)
	m.Pricing.InputPerMillion = 2
	m.Pricing.CachedInputPerMillion = .5
	m.Pricing.OutputPerMillion = 4
	for _, tc := range []struct {
		name   string
		cached int64
		want   float64
	}{{"partial cache", 100000, .25 + 1.8 + .05 + .8}, {"cache capped at input", 2000000, .25 + .5 + .8}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Models: []domain.Model{m}, Features: domain.RequestFeatures{InputTokens: 1000000, CachedInputTokens: tc.cached, MaxOutputTokens: 200000}})
			if err != nil || math.Abs(got.Candidates[0].DirectCost-tc.want) > 1e-12 {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
		})
	}
}

func TestPolicyCombinesFloorsAndConfidence(t *testing.T) {
	models := []domain.Model{}
	for tier := domain.T0; tier <= domain.T6; tier++ {
		models = append(models, policyModel(tier.String(), tier, float64(tier), 1))
	}
	config := PolicyConfig{MinJevConfidence: .8, ReasoningFloors: []SignalFloor{{Threshold: 4, Floor: domain.T4}}, CodingFloors: []SignalFloor{{Threshold: 5, Floor: domain.T5}}, RiskFloors: []SignalFloor{{Threshold: 3, Floor: domain.T6}}, UnderspecificationFloors: []SignalFloor{{Threshold: .7, Floor: domain.T5}}}
	for _, tc := range []struct {
		name  string
		input DecisionInput
		want  domain.Tier
	}{
		{"caller", DecisionInput{MinTier: tierPtr(domain.T3)}, domain.T3},
		{"base", DecisionInput{Floor: domain.T4}, domain.T4},
		{"jev", DecisionInput{Judgment: &domain.JevJudgment{MinimumTier: domain.T5, TierConfidence: .8}}, domain.T5},
		{"low confidence cannot lower", DecisionInput{Floor: domain.T4, Judgment: &domain.JevJudgment{MinimumTier: domain.T1, TierConfidence: .1}}, domain.T4},
		{"confidence gate", DecisionInput{Floor: domain.T2, Judgment: &domain.JevJudgment{MinimumTier: domain.T6, TierConfidence: .79}}, domain.T2},
		{"reasoning", DecisionInput{Judgment: &domain.JevJudgment{ReasoningScore: 4}}, domain.T4},
		{"coding", DecisionInput{Judgment: &domain.JevJudgment{CodingScore: 5}}, domain.T5},
		{"risk", DecisionInput{Judgment: &domain.JevJudgment{BlastRadius: 3}}, domain.T6},
		{"underspecified", DecisionInput{Judgment: &domain.JevJudgment{Underspecified: .7}}, domain.T5},
		{"below threshold", DecisionInput{Judgment: &domain.JevJudgment{ReasoningScore: 3.9, CodingScore: 4.9, BlastRadius: 2.9, Underspecified: .69}}, domain.T0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.input.Models = models
			got, err := NewPolicy(config).Decide(tc.input)
			if err != nil || got.Tier != tc.want {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
		})
	}
}

func TestPolicyPinReuseAndReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DecisionInput)
		want   string
	}{
		{"compatible pin beats cheaper", func(*DecisionInput) {}, "pinned"},
		{"unavailable", func(in *DecisionInput) { in.Models[0].Available = false }, "equal"},
		{"capability mismatch", func(in *DecisionInput) { in.Features.NeedsText = true; in.Models[0].Capabilities.Text = false }, "equal"},
		{"wrong provider", func(in *DecisionInput) { in.Pin.Provider = "other" }, "equal"},
		{"raised floor", func(in *DecisionInput) { in.Floor = domain.T6 }, "stronger"},
		{"credentials", func(in *DecisionInput) { in.ProviderCredentials = map[string]bool{"replacement": true} }, "equal"},
		{"provider unavailable", func(in *DecisionInput) { in.ProviderAvailability = map[string]bool{"replacement": true} }, "equal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := DecisionInput{Pin: &Pin{ModelID: "pinned", Provider: "provider", Floor: domain.T3}, Models: []domain.Model{policyModel("pinned", domain.T5, 2, 1), policyModel("equal", domain.T5, 1, 1), policyModel("stronger", domain.T6, 3, 1), policyModel("cheap", domain.T3, 0, 1)}}
			in.Models[1].Provider = "replacement"
			in.Models[2].Provider = "replacement"
			tc.mutate(&in)
			// A missing provider identity still carries the persisted pin floor.
			if tc.name == "wrong provider" {
				in.Pin.Floor = domain.T5
			}
			got, err := NewPolicy(PolicyConfig{}).Decide(in)
			if err != nil || got.ModelID != tc.want || got.Tier < domain.T5 {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
			if !strings.Contains(strings.Join(got.Reasons, " "), "pin") {
				t.Fatalf("missing pin explanation: %+v", got)
			}
		})
	}
}

func TestPolicyEnforcesConstraintsAndRetainsScores(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  PolicyConfig
		code RejectionCode
	}{
		{"success", PolicyConfig{MinSuccessProbability: .95}, RejectSuccess},
		{"direct", PolicyConfig{MaxDirectCost: .09}, RejectDirectCost},
		{"expected", PolicyConfig{FailureEscalationCost: 1, MaxExpectedCost: .15}, RejectExpectedCost},
		{"latency", PolicyConfig{MaxLatency: time.Second}, RejectLatency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := policyModel("blocked", domain.T5, .1, .9)
			m.LatencyP95 = 2 * time.Second
			got, err := NewPolicy(tc.cfg).Decide(DecisionInput{Models: []domain.Model{m}, Floor: domain.T5})
			var noEligible *NoEligibleModelError
			if !errors.As(err, &noEligible) || len(got.Candidates) != 1 || !hasRejection(got.Rejections, "blocked", tc.code) {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
		})
	}
	got, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Floor: domain.T5, MaxTier: tierPtr(domain.T4), Models: []domain.Model{policyModel("low", domain.T4, 0, 1), policyModel("high", domain.T5, 1, 1)}})
	if err == nil || len(got.Rejections) != 2 || !hasRejection(got.Rejections, "high", RejectBounds) {
		t.Fatalf("decision=%+v err=%v", got, err)
	}
}

func TestPolicyUsesTaskPriorsAndLatencyPenalty(t *testing.T) {
	m := policyModel("task", domain.T4, .1, .1)
	m.SuccessPriors = map[domain.TaskType]float64{domain.TaskCoding: .9}
	m.LatencyP95 = 2 * time.Second
	got, err := NewPolicy(PolicyConfig{FailureEscalationCost: 1, LatencyPenaltyPerSecond: .05}).Decide(DecisionInput{Models: []domain.Model{m}, TaskType: domain.TaskCoding})
	if err != nil || math.Abs(got.Candidates[0].ExpectedTotalCost-.3) > 1e-12 {
		t.Fatalf("decision=%+v err=%v", got, err)
	}
}

func TestPolicyAcceptsExactSuccessMinimum(t *testing.T) {
	got, err := NewPolicy(PolicyConfig{MinSuccessProbability: .1}).Decide(DecisionInput{
		Models: []domain.Model{policyModel("boundary", domain.T4, 0, .1)},
	})
	if err != nil || got.ModelID != "boundary" {
		t.Fatalf("exact minimum rejected: decision=%+v err=%v", got, err)
	}
}

func TestPolicyReplayIsDeterministicAndDoesNotMutateInputs(t *testing.T) {
	rules := []SignalFloor{{Threshold: 4, Floor: domain.T4}}
	p := NewPolicy(PolicyConfig{ReasoningFloors: rules})
	rules[0].Floor = domain.T6
	in := DecisionInput{Models: []domain.Model{policyModel("first", domain.T4, 1, 1), policyModel("second", domain.T6, 2, 1)}, Judgment: &domain.JevJudgment{ReasoningScore: 4}}
	want, err := p.Decide(in)
	if err != nil || want.ModelID != "first" {
		t.Fatalf("decision=%+v err=%v", want, err)
	}
	for i := 0; i < 10; i++ {
		got, err := p.Decide(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("replay=%+v err=%v", got, err)
		}
	}
	if in.Models[0].ID != "first" || in.Floor != domain.T0 || in.Judgment.ReasoningScore != 4 {
		t.Fatalf("mutated input: %+v", in)
	}
}

func TestPolicyRejectsInvalidNumericInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    PolicyConfig
		mutate func(*DecisionInput)
	}{
		{"NaN policy", PolicyConfig{FailureEscalationCost: math.NaN()}, func(*DecisionInput) {}},
		{"negative policy", PolicyConfig{MaxDirectCost: -1}, func(*DecisionInput) {}},
		{"invalid threshold", PolicyConfig{ReasoningFloors: []SignalFloor{{Threshold: 1, Floor: domain.Tier(7)}}}, func(*DecisionInput) {}},
		{"negative tokens", PolicyConfig{}, func(in *DecisionInput) { in.Features.InputTokens = -1 }},
		{"NaN prior", PolicyConfig{}, func(in *DecisionInput) { in.Models[0].DefaultSuccessPrior = math.NaN() }},
		{"infinite price", PolicyConfig{}, func(in *DecisionInput) { in.Models[0].Pricing.PerRequestUSD = math.Inf(1) }},
		{"invalid pin", PolicyConfig{}, func(in *DecisionInput) { in.Pin = &Pin{Floor: domain.Tier(7)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := DecisionInput{Models: []domain.Model{policyModel("bad", domain.T4, 1, 1)}}
			tc.mutate(&in)
			if got, err := NewPolicy(tc.cfg).Decide(in); err == nil {
				t.Fatalf("accepted invalid input: %+v", got)
			}
		})
	}
}
