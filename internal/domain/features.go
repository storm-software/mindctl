package domain

import (
	"sort"
	"time"
)

// RequestFeatures are deterministic requirements extracted from one request.
type RequestFeatures struct {
	InputTokens, ContextTokens, MaxOutputTokens int64
	// CachedInputTokens is a subset of InputTokens, capped at InputTokens for pricing.
	CachedInputTokens                      int64
	NeedsText, NeedsImages, NeedsFunctions bool
	NeedsJSONSchema, NeedsHostedTools      bool
	NeedsNativeTools                       bool
	HostedToolTypes                        []string
}

// Normalize returns a stable feature snapshot suitable for policy input. It
// keeps invalid numeric values intact for policy validation, caps a valid cache
// estimate at valid input usage, and canonicalizes hosted tool requirements.
func (f RequestFeatures) Normalize() RequestFeatures {
	if f.InputTokens >= 0 && f.CachedInputTokens > f.InputTokens {
		f.CachedInputTokens = f.InputTokens
	}
	tools := append([]string(nil), f.HostedToolTypes...)
	sort.Strings(tools)
	f.HostedToolTypes = tools[:0]
	for _, tool := range tools {
		if tool == "" || (len(f.HostedToolTypes) > 0 && f.HostedToolTypes[len(f.HostedToolTypes)-1] == tool) {
			continue
		}
		f.HostedToolTypes = append(f.HostedToolTypes, tool)
	}
	if len(f.HostedToolTypes) > 0 {
		f.NeedsHostedTools = true
	}
	return f
}

// ClassifierJudgment is the classifier signal consumed by deterministic routing
// policy. It intentionally contains no provider-specific behavior.
type ClassifierJudgment struct {
	MinimumTier                         Tier
	TierConfidence                      float64
	TierProbabilities                   map[Tier]float64
	TaskType                            TaskType
	TaskTypeConfidence                  float64
	TaskTypeProbabilities               map[TaskType]float64
	ReasoningScore, ReasoningConfidence float64
	CodingScore, CodingConfidence       float64
	BlastRadius, BlastRadiusConfidence  float64
	Underspecified                      float64
	Classifier, ResolvedModel           string
	ModelRevision                       string
	Latency                             time.Duration
}
