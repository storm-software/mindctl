package router

import "github.com/storm-software/mindctl/internal/domain"

// FloorFromJudgment supplies a baseline for Policy.Decide. On classifier
// unavailability, a conversation pin takes precedence over the configured
// no-pin fallback (normally T4). Successful judgments are passed separately to
// DecisionInput.Judgment so policy owns confidence gates and all signal floors.
func FloorFromJudgment(judgment *domain.ClassifierJudgment, pin *Pin, fallback domain.Tier) domain.Tier {
	if pin != nil {
		return pin.Floor
	}
	if judgment == nil {
		return fallback
	}
	return domain.T0
}
