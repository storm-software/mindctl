package domain

import "time"

// RequestFeatures are deterministic requirements extracted from one request.
type RequestFeatures struct {
	InputTokens, ContextTokens, MaxOutputTokens int64
	// CachedInputTokens is a subset of InputTokens, capped at InputTokens for pricing.
	CachedInputTokens                      int64
	NeedsText, NeedsImages, NeedsFunctions bool
	NeedsJSONSchema, NeedsHostedTools      bool
	HostedToolTypes                        []string
}

// JevJudgment is the classifier signal consumed by deterministic routing
// policy. It intentionally contains no provider-specific behavior.
type JevJudgment struct {
	MinimumTier                                              Tier
	TierConfidence                                           float64
	TierProbabilities                                        map[Tier]float64
	TaskType                                                 TaskType
	TaskTypeConfidence                                       float64
	TaskTypeProbabilities                                    map[TaskType]float64
	ReasoningScore, CodingScore, BlastRadius, Underspecified float64
	ResolvedModel                                            string
	InputTokens, OutputTokens                                int64
	Latency                                                  time.Duration
}
