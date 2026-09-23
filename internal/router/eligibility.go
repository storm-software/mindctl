// Package router contains pure model-selection policy.
package router

import (
	"fmt"

	"github.com/storm-software/mindctl/internal/domain"
)

// EligibilityInput contains the hard constraints that a model must satisfy.
// A non-nil provider map is authoritative: a missing provider entry is false.
type EligibilityInput struct {
	Models               []domain.Model
	Features             domain.RequestFeatures
	Floor                domain.Tier
	MinTier, MaxTier     *domain.Tier
	ProviderCredentials  map[string]bool
	ProviderAvailability map[string]bool
}

// RejectionCode describes why a model cannot satisfy a request.
type RejectionCode string

const (
	RejectTier         RejectionCode = "tier"
	RejectBounds       RejectionCode = "bounds"
	RejectContext      RejectionCode = "context"
	RejectOutput       RejectionCode = "output"
	RejectModality     RejectionCode = "modality"
	RejectTools        RejectionCode = "tools"
	RejectCredentials  RejectionCode = "credentials"
	RejectAvailability RejectionCode = "availability"

	// RejectUnavailable is retained as a readable alias for availability.
	RejectUnavailable = RejectAvailability
)

// Rejection retains every hard-constraint failure for one model. Code is the
// first failure in deterministic evaluation order; Codes contains all failures.
type Rejection struct {
	ModelID string
	Code    RejectionCode
	Codes   []RejectionCode
	Reasons []string
}

// NoEligibleModelError reports that no configured model can satisfy the hard
// constraints, while preserving the complete rejection explanation.
type NoEligibleModelError struct {
	Rejections []Rejection
}

func (e *NoEligibleModelError) Error() string {
	return fmt.Sprintf("no eligible model (%d rejection(s))", len(e.Rejections))
}

// EligibleModels filters models without performing network, provider, or
// storage work. Eligible models retain their catalog order.
func EligibleModels(input EligibilityInput) ([]domain.Model, []Rejection) {
	minimum, maximum, boundCodes, boundReasons := eligibilityBounds(input)
	eligible := make([]domain.Model, 0, len(input.Models))
	rejected := make([]Rejection, 0, len(input.Models))

	for _, model := range input.Models {
		codes := append([]RejectionCode(nil), boundCodes...)
		reasons := append([]string(nil), boundReasons...)
		add := func(code RejectionCode, reason string) {
			codes = append(codes, code)
			reasons = append(reasons, reason)
		}

		if !model.Tier.Valid() {
			add(RejectTier, "model tier is invalid")
		} else if model.Tier < minimum {
			add(RejectTier, "model tier is below the required floor")
		}
		if maximum != nil && model.Tier.Valid() && model.Tier > *maximum {
			add(RejectBounds, "model tier exceeds the maximum bound")
		}

		requiredContext := input.Features.ContextTokens
		if input.Features.InputTokens > requiredContext {
			requiredContext = input.Features.InputTokens
		}
		if requiredContext > 0 && model.ContextWindow < requiredContext {
			add(RejectContext, "model context window is too small")
		}
		if input.Features.MaxOutputTokens > 0 && model.MaxOutputTokens < input.Features.MaxOutputTokens {
			add(RejectOutput, "model output limit is too small")
		}

		if input.Features.NeedsText && !model.Capabilities.Text {
			add(RejectModality, "model lacks text support")
		}
		if input.Features.NeedsImages && !model.Capabilities.Images {
			add(RejectModality, "model lacks image support")
		}
		if input.Features.NeedsFunctions && !model.Capabilities.Functions {
			add(RejectTools, "model lacks custom function support")
		}
		if input.Features.NeedsNativeTools && model.Provider != "openai" {
			add(RejectTools, "model provider lacks native Responses tool support")
		}
		if input.Features.NeedsJSONSchema && !model.Capabilities.JSONSchema {
			add(RejectTools, "model lacks JSON Schema support")
		}
		if input.Features.NeedsHostedTools || len(input.Features.HostedToolTypes) > 0 {
			if len(input.Features.HostedToolTypes) == 0 {
				enabled := false
				for _, available := range model.Capabilities.HostedTools {
					if available {
						enabled = true
						break
					}
				}
				if !enabled {
					add(RejectTools, "model lacks hosted tool support")
				}
			} else {
				for _, toolType := range input.Features.HostedToolTypes {
					if !model.Capabilities.HostedTools[toolType] {
						add(RejectTools, "model lacks hosted tool support: "+toolType)
					}
				}
			}
		}

		if input.ProviderCredentials != nil && !input.ProviderCredentials[model.Provider] {
			add(RejectCredentials, "provider credentials are unavailable")
		}
		if !model.Available {
			add(RejectAvailability, "model is unavailable")
		}
		if input.ProviderAvailability != nil && !input.ProviderAvailability[model.Provider] {
			add(RejectAvailability, "provider is unavailable")
		}

		if len(codes) == 0 {
			eligible = append(eligible, model)
			continue
		}
		rejected = append(rejected, Rejection{
			ModelID: model.ID,
			Code:    codes[0],
			Codes:   codes,
			Reasons: reasons,
		})
	}

	return eligible, rejected
}

// RequireEligible converts an empty eligible set into a typed policy error.
func RequireEligible(models []domain.Model, rejections []Rejection) ([]domain.Model, error) {
	if len(models) != 0 {
		return models, nil
	}
	return nil, &NoEligibleModelError{Rejections: cloneRejections(rejections)}
}

func eligibilityBounds(input EligibilityInput) (domain.Tier, *domain.Tier, []RejectionCode, []string) {
	minimum := input.Floor
	var codes []RejectionCode
	var reasons []string
	add := func(code RejectionCode, reason string) {
		codes = append(codes, code)
		reasons = append(reasons, reason)
	}

	if !minimum.Valid() {
		add(RejectTier, "requested floor is invalid")
		minimum = domain.T6
	}
	if input.MinTier != nil {
		if !input.MinTier.Valid() {
			add(RejectBounds, "minimum tier bound is invalid")
		} else if *input.MinTier > minimum {
			minimum = *input.MinTier
		}
	}
	if input.MaxTier != nil && !input.MaxTier.Valid() {
		add(RejectBounds, "maximum tier bound is invalid")
	}
	if input.MaxTier != nil && input.MaxTier.Valid() && minimum > *input.MaxTier {
		add(RejectBounds, "minimum tier exceeds the maximum bound")
	}

	return minimum, input.MaxTier, codes, reasons
}

func cloneRejections(rejections []Rejection) []Rejection {
	cloned := make([]Rejection, len(rejections))
	for index, rejection := range rejections {
		cloned[index] = Rejection{
			ModelID: rejection.ModelID,
			Code:    rejection.Code,
			Codes:   append([]RejectionCode(nil), rejection.Codes...),
			Reasons: append([]string(nil), rejection.Reasons...),
		}
	}
	return cloned
}
