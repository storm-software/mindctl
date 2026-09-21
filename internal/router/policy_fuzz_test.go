package router

import (
	"github.com/storm-software/mindctl/internal/domain"
	"testing"
)

func FuzzPolicyNeverSelectsBelowFloor(f *testing.F) {
	f.Add(uint8(4), uint8(2))
	f.Add(uint8(0), uint8(6))
	f.Add(uint8(6), uint8(6))
	f.Fuzz(func(t *testing.T, floorRaw, modelRaw uint8) {
		floor, modelTier := domain.Tier(floorRaw%7), domain.Tier(modelRaw%7)
		got, err := NewPolicy(PolicyConfig{}).Decide(DecisionInput{Floor: floor, Models: []domain.Model{policyModel("model", modelTier, 0, 1)}})
		if err == nil && got.Tier < floor {
			t.Fatalf("selected %s below %s", got.Tier, floor)
		}
		if modelTier >= floor && err != nil {
			t.Fatalf("eligible tier %s rejected: %v", modelTier, err)
		}
	})
}
