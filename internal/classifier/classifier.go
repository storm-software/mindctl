// Package classifier defines requirement signals independently of routing policy.
package classifier

import (
	"context"
	"errors"

	"github.com/storm-software/mindctl/internal/domain"
)

var ErrUnavailable = errors.New("classifier unavailable")

type Classifier interface {
	Classify(context.Context, Input) (domain.ClassifierJudgment, error)
}

type Input struct {
	Prompt          string                 `json:"prompt"`
	Features        domain.RequestFeatures `json:"features"`
	CurrentModel    string                 `json:"current_model"`
	AvailableModels []string               `json:"available_models"`
}
