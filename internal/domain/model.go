package domain

import "time"

// Model describes a configured provider model without provider implementation
// details.
type Model struct {
	ID, Provider, UpstreamID string
	Tier                     Tier
	Capabilities             Capabilities
	ContextWindow            int64
	MaxOutputTokens          int64
	Pricing                  Pricing
	SuccessPriors            map[TaskType]float64
	DefaultSuccessPrior      float64
	LatencyP95               time.Duration
	Order                    int
	Available                bool
}

// Capabilities records portable request features a model supports.
type Capabilities struct {
	Text, Images, Functions, JSONSchema bool
	HostedTools                         map[string]bool
}

// Pricing records provider-independent USD pricing terms.
type Pricing struct {
	InputPerMillion, CachedInputPerMillion, OutputPerMillion float64
	PerRequestUSD                                            float64
}

// TaskType classifies the work used to select the appropriate success prior.
type TaskType string

const (
	TaskUnknown        TaskType = "unknown"
	TaskExtraction     TaskType = "extraction"
	TaskClassification TaskType = "classification"
	TaskGeneration     TaskType = "generation"
	TaskReasoning      TaskType = "reasoning"
	TaskCoding         TaskType = "coding"
	TaskToolUse        TaskType = "tool_use"
	TaskMultimodal     TaskType = "multimodal"
)
