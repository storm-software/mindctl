package router

import (
	"github.com/storm-software/mindctl/internal/domain"
	"testing"
)

func TestUnavailableClassifierFallsBackToT4(t *testing.T) {
	got, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Floor: FloorFromJudgment(nil, nil, domain.T4), Models: []domain.Model{policyModel("cheap", domain.T0, 0, 1), policyModel("safe", domain.T4, 1, 1)}})
	if err != nil || got.Tier != domain.T4 {
		t.Fatalf("decision=%+v err=%v", got, err)
	}
}

func TestFloorFromJudgmentPreservesPinAndPolicyConfidence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		judgment       *domain.ClassifierJudgment
		pin            *Pin
		fallback, want domain.Tier
	}{
		{"strong pin", nil, &Pin{ModelID: "pinned", Provider: "openai", Floor: domain.T5}, domain.T4, domain.T5},
		{"pin wins over fallback", nil, &Pin{Floor: domain.T2}, domain.T4, domain.T2},
		{"configured fallback", nil, nil, domain.T6, domain.T6},
		{"successful signal left to policy", &domain.ClassifierJudgment{MinimumTier: domain.T6, TierConfidence: .1}, nil, domain.T4, domain.T0},
		{"successful signal preserves pin", &domain.ClassifierJudgment{MinimumTier: domain.T1, TierConfidence: 1}, &Pin{Floor: domain.T5}, domain.T4, domain.T5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FloorFromJudgment(tc.judgment, tc.pin, tc.fallback); got != tc.want {
				t.Fatalf("floor=%s want=%s", got, tc.want)
			}
		})
	}
}
