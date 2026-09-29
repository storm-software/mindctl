package router

import (
	"math"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
)

func TestEffectiveSuccessProbability(t *testing.T) {
	model := policyModel("rated", domain.T4, 0, 0.8)
	tests := []struct {
		name     string
		evidence FeedbackEvidence
		want     float64
		applied  bool
		wantErr  bool
	}{
		{name: "nine ratings keep configured prior", evidence: FeedbackEvidence{ThumbsUpCount: 0, RatingCount: 9}, want: 0.8},
		{name: "ten ratings activate blend", evidence: FeedbackEvidence{ThumbsUpCount: 5, RatingCount: 10}, want: 0.7, applied: true},
		{name: "twenty positive ratings", evidence: FeedbackEvidence{ThumbsUpCount: 20, RatingCount: 20}, want: 0.9, applied: true},
		{name: "negative thumbs up count", evidence: FeedbackEvidence{ThumbsUpCount: -1, RatingCount: 10}, wantErr: true},
		{name: "negative rating count", evidence: FeedbackEvidence{RatingCount: -1}, wantErr: true},
		{name: "thumbs up exceeds ratings", evidence: FeedbackEvidence{ThumbsUpCount: 11, RatingCount: 10}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configured, got, applied, err := EffectiveSuccessProbability(model, domain.TaskCoding, test.evidence)
			if test.wantErr {
				if err == nil {
					t.Fatalf("EffectiveSuccessProbability() accepted evidence %+v", test.evidence)
				}
				return
			}
			if err != nil || math.Abs(configured-0.8) > 1e-12 || math.Abs(got-test.want) > 1e-12 || applied != test.applied {
				t.Fatalf("EffectiveSuccessProbability() = configured %v effective %v applied %v, err %v; want configured 0.8 effective %v applied %v", configured, got, applied, err, test.want, test.applied)
			}
		})
	}
}
