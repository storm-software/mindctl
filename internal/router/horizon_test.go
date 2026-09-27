package router

import (
	"errors"
	"math"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
)

func pricedModel(id string, input, cached, write, output float64) domain.Model {
	m := policyModel(id, domain.T4, 0, 1)
	m.Pricing = domain.Pricing{InputPerMillion: input, CachedInputPerMillion: cached, CacheWriteInputPerMillion: write, OutputPerMillion: output}
	return m
}

// Model A caches well; model B has cheaper uncached input but a poor cache discount.
var (
	cachesWell   = pricedModel("caches-well", 3, .3, 3.75, 15)
	cachesPoorly = pricedModel("caches-poorly", 2.5, 1.25, 2.5, 15)
)

func horizonFeatures() domain.RequestFeatures {
	return domain.RequestFeatures{InputTokens: 100_000, MaxOutputTokens: 32_000, ExpectedOutputTokens: 2_000}
}

func candidate(t *testing.T, decision Decision, id string) CandidateScore {
	t.Helper()
	for _, score := range decision.Candidates {
		if score.ModelID == id {
			return score
		}
	}
	t.Fatalf("no candidate %s in %+v", id, decision.Candidates)
	return CandidateScore{}
}

func TestHorizonScoringPrefersCheaperCachedModel(t *testing.T) {
	models := []domain.Model{cachesWell, cachesPoorly}
	single, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Models: models, Features: horizonFeatures()})
	if err != nil || single.ModelID != "caches-poorly" {
		t.Fatalf("single-turn decision=%s err=%v; want caches-poorly", single.ModelID, err)
	}
	session, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Models: models, Features: horizonFeatures(),
		Session: &SessionEstimate{HorizonTurns: 3, CacheHitRatio: .9}})
	if err != nil || session.ModelID != "caches-well" {
		t.Fatalf("session decision=%s err=%v; want caches-well", session.ModelID, err)
	}
	for _, want := range []struct {
		id                   string
		turn, horizon, total float64
	}{
		{"caches-well", .375 + .03, 3 * (.027 + .0375 + .03), .6885},
		{"caches-poorly", .25 + .03, 3 * (.1125 + .025 + .03), .7825},
	} {
		got := candidate(t, session, want.id)
		if math.Abs(got.ExpectedTurnCost-want.turn) > 1e-12 || math.Abs(got.HorizonCost-want.horizon) > 1e-12 ||
			math.Abs(got.ExpectedTotalCost-want.total) > 1e-12 {
			t.Fatalf("%s score=%+v; want turn=%v horizon=%v total=%v", want.id, got, want.turn, want.horizon, want.total)
		}
	}
	if !hasReason(session.Reasons, "session horizon: turns=3 cache_hit_ratio=0.90") {
		t.Fatalf("session reasons=%v", session.Reasons)
	}
}

func TestExpectedOutputReplacesMaxTokensInRanking(t *testing.T) {
	cheapOutput := pricedModel("cheap-output", 3, 3, 3, 5)
	cheapInput := pricedModel("cheap-input", 1, 1, 1, 20)
	models := []domain.Model{cheapOutput, cheapInput}
	features := horizonFeatures()
	features.ExpectedOutputTokens = 0
	legacy, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Models: models, Features: features})
	if err != nil || legacy.ModelID != "cheap-output" {
		t.Fatalf("max_tokens ranking chose %s err=%v; want cheap-output", legacy.ModelID, err)
	}
	expected, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Models: models, Features: horizonFeatures()})
	if err != nil || expected.ModelID != "cheap-input" {
		t.Fatalf("expected-output ranking chose %s err=%v; want cheap-input", expected.ModelID, err)
	}
	if got := candidate(t, expected, "cheap-input"); math.Abs(got.DirectCost-.74) > 1e-12 || math.Abs(got.ExpectedTurnCost-.14) > 1e-12 {
		t.Fatalf("cheap-input score=%+v; want worst-case direct .74 and expected .14", got)
	}
}

func TestWorstCaseCapUnchanged(t *testing.T) {
	cheapOutput := pricedModel("cheap-output", 3, 3, 3, 5)
	cheapInput := pricedModel("cheap-input", 1, 1, 1, 20)
	got, err := NewPolicy(PolicyConfig{MaxDirectCost: .5}).Decide(DecisionInput{Models: []domain.Model{cheapOutput, cheapInput}, Features: horizonFeatures()})
	if err != nil || got.ModelID != "cheap-output" || !hasRejection(got.Rejections, "cheap-input", RejectDirectCost) {
		t.Fatalf("decision=%s rejections=%+v err=%v; want the worst-case cap to reject cheap-input", got.ModelID, got.Rejections, err)
	}
}

func TestExpectedCostCapExcludesHorizon(t *testing.T) {
	got, err := NewPolicy(PolicyConfig{MaxExpectedCost: .5}).Decide(DecisionInput{Models: []domain.Model{cachesWell}, Features: horizonFeatures(),
		Session: &SessionEstimate{HorizonTurns: 3, CacheHitRatio: .9}})
	if err != nil || got.ModelID != "caches-well" {
		t.Fatalf("decision=%+v err=%v; the horizon must not make a request unroutable", got, err)
	}
}

func assertLegacyScores(t *testing.T, decision Decision) {
	t.Helper()
	for _, score := range decision.Candidates {
		if score.ExpectedTurnCost != score.DirectCost || score.HorizonCost != 0 ||
			score.ExpectedTotalCost != score.DirectCost+score.FailureProbability*score.EscalationCost+score.LatencyPenalty {
			t.Fatalf("legacy score changed: %+v", score)
		}
	}
}

func TestLegacyScoresIdentical(t *testing.T) {
	features := horizonFeatures()
	features.ExpectedOutputTokens = 0
	features.CachedInputTokens = 40_000
	for _, cfg := range []PolicyConfig{{}, {FailureEscalationCost: .2, LatencyPenaltyPerSecond: .01}} {
		models := []domain.Model{cachesWell, cachesPoorly, policyModel("per-request", domain.T4, .3, .8)}
		models[2].LatencyP95 = 1500 * 1e6
		got, err := NewPolicy(cfg).Decide(DecisionInput{Models: models, Features: features})
		if err != nil {
			t.Fatal(err)
		}
		assertLegacyScores(t, got)
		if hasReasonPrefix(got.Reasons, "session horizon") {
			t.Fatalf("legacy decision has session reasons: %v", got.Reasons)
		}
	}
}

func TestPolicyRejectsInvalidSessionInputs(t *testing.T) {
	nanWrite := cachesWell
	nanWrite.Pricing.CacheWriteInputPerMillion = math.NaN()
	for _, tc := range []struct {
		name string
		in   DecisionInput
	}{
		{"negative expected output", DecisionInput{Models: []domain.Model{cachesWell}, Features: domain.RequestFeatures{ExpectedOutputTokens: -1}}},
		{"horizon above maximum", DecisionInput{Models: []domain.Model{cachesWell}, Session: &SessionEstimate{HorizonTurns: 21}}},
		{"negative horizon", DecisionInput{Models: []domain.Model{cachesWell}, Session: &SessionEstimate{HorizonTurns: -1}}},
		{"hit ratio above one", DecisionInput{Models: []domain.Model{cachesWell}, Session: &SessionEstimate{CacheHitRatio: 1.1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPolicy(PolicyConfig{}).Decide(tc.in); err == nil {
				t.Fatal("invalid session input was accepted")
			}
		})
	}
	got, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Models: []domain.Model{nanWrite}, Session: &SessionEstimate{HorizonTurns: 3, CacheHitRatio: .9}})
	var noEligible *NoEligibleModelError
	if !errors.As(err, &noEligible) || !hasRejection(got.Rejections, "caches-well", RejectEstimate) {
		t.Fatalf("non-finite cache-write price decision=%+v err=%v", got, err)
	}
}

func hasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func hasReasonPrefix(reasons []string, prefix string) bool {
	for _, reason := range reasons {
		if len(reason) >= len(prefix) && reason[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
