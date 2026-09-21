package router

import (
	"errors"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
)

func TestEligibleModelsRejectsCapabilityAndFloorViolations(t *testing.T) {
	models := []domain.Model{
		{ID: "text-t3", Tier: domain.T3, Capabilities: domain.Capabilities{Text: true}},
		{ID: "vision-t4", Tier: domain.T4, Capabilities: domain.Capabilities{Text: true, Images: true}, ContextWindow: 4096},
	}

	got, rejected := EligibleModels(EligibilityInput{
		Models: models,
		Floor:  domain.T4,
		Features: domain.RequestFeatures{
			NeedsImages:   true,
			ContextTokens: 8192,
		},
	})

	if len(got) != 0 || len(rejected) != 2 {
		t.Fatalf("got=%v rejected=%v", got, rejected)
	}
	if !hasRejection(rejected, "text-t3", RejectTier) || !hasRejection(rejected, "text-t3", RejectModality) {
		t.Fatalf("text-t3 rejections=%v", rejected)
	}
	if !hasRejection(rejected, "vision-t4", RejectContext) {
		t.Fatalf("vision-t4 rejections=%v", rejected)
	}
}

func TestEligibleModelsPreservesEveryHardConstraint(t *testing.T) {
	model := domain.Model{
		ID:              "restricted",
		Provider:        "provider-a",
		Tier:            domain.T3,
		ContextWindow:   100,
		MaxOutputTokens: 10,
		Capabilities: domain.Capabilities{
			Text:        false,
			Images:      false,
			Functions:   false,
			JSONSchema:  false,
			HostedTools: map[string]bool{"web_search": false},
		},
		Available: false,
	}

	got, rejected := EligibleModels(EligibilityInput{
		Models:              []domain.Model{model},
		Floor:               domain.T4,
		MinTier:             tierPtr(domain.T5),
		MaxTier:             tierPtr(domain.T4),
		ProviderCredentials: map[string]bool{"provider-a": false},
		ProviderAvailability: map[string]bool{
			"provider-a": false,
		},
		Features: domain.RequestFeatures{
			InputTokens:      101,
			MaxOutputTokens:  11,
			NeedsText:        true,
			NeedsImages:      true,
			NeedsFunctions:   true,
			NeedsJSONSchema:  true,
			NeedsHostedTools: true,
			HostedToolTypes:  []string{"web_search"},
		},
	})

	if len(got) != 0 {
		t.Fatalf("eligible models=%v", got)
	}
	for _, code := range []RejectionCode{
		RejectTier,
		RejectBounds,
		RejectContext,
		RejectOutput,
		RejectModality,
		RejectTools,
		RejectCredentials,
		RejectAvailability,
	} {
		if !hasRejection(rejected, "restricted", code) {
			t.Fatalf("missing %q rejection: %v", code, rejected)
		}
	}
}

func TestEligibleModelsAppliesMinimumAndMaximumTierBounds(t *testing.T) {
	models := []domain.Model{
		{ID: "low", Tier: domain.T3, Available: true},
		{ID: "within", Tier: domain.T4, Available: true},
		{ID: "high", Tier: domain.T5, Available: true},
	}
	minTier, maxTier := domain.T4, domain.T4

	got, rejected := EligibleModels(EligibilityInput{Models: models, MinTier: &minTier, MaxTier: &maxTier})
	if len(got) != 1 || got[0].ID != "within" {
		t.Fatalf("eligible models=%v", got)
	}
	if !hasRejection(rejected, "low", RejectTier) || !hasRejection(rejected, "high", RejectBounds) {
		t.Fatalf("rejections=%v", rejected)
	}
}

func TestRequireEligibleReturnsTypedError(t *testing.T) {
	_, err := RequireEligible(nil, []Rejection{{ModelID: "m", Code: RejectContext}})
	var noModel *NoEligibleModelError
	if !errors.As(err, &noModel) || len(noModel.Rejections) != 1 {
		t.Fatalf("expected typed error, got %v", err)
	}
}

func TestRequireEligibleLeavesNonemptyCandidatesUntouched(t *testing.T) {
	models := []domain.Model{{ID: "eligible", Tier: domain.T4}}
	got, err := RequireEligible(models, nil)
	if err != nil || len(got) != 1 || got[0].ID != "eligible" {
		t.Fatalf("models=%v err=%v", got, err)
	}
}

func hasRejection(rejections []Rejection, modelID string, code RejectionCode) bool {
	for _, rejection := range rejections {
		if rejection.ModelID == modelID && rejection.Code == code {
			return true
		}
		for _, rejectionCode := range rejection.Codes {
			if rejection.ModelID == modelID && rejectionCode == code {
				return true
			}
		}
	}
	return false
}

func tierPtr(tier domain.Tier) *domain.Tier {
	return &tier
}
