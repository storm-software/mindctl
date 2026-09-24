// Package inference defines the provider-neutral request, result, and tool
// shapes shared by the gateway and all provider adapters.
package inference

import "encoding/json"

// Request is the portable Responses subset accepted by the gateway.
type Request struct {
	ID, Model, Instructions, PreviousResponseID string
	Input                                       []Item
	Tools                                       []Tool
	TextFormat                                  *JSONSchemaFormat
	Reasoning                                   *ReasoningOptions
	StreamOptions                               *StreamOptions
	AccessPrograms                              *AccessPrograms
	ToolChoice, ServiceTier, PromptCacheKey     string
	TextVerbosity                               string
	Include                                     []string
	ClientMetadata                              map[string]string
	ParallelToolCalls                           *bool
	Stream                                      bool
	MaxOutputTokens                             int64
}

// ReasoningOptions carries Responses API reasoning controls without coupling
// them to a particular authentication mode.
type ReasoningOptions struct {
	Effort, Summary, Context string
}

// StreamOptions controls delivery of streamed reasoning summaries.
type StreamOptions struct {
	ReasoningSummaryDelivery string
}

// AccessPrograms selects an account-authorized Responses access program.
type AccessPrograms struct {
	Cyber string
}

// Item is a portable text, image, reasoning, function-call, or function-result
// item. EncryptedContent is opaque provider continuation state. Its source is
// recorded internally so adapters never forward it to another provider.
type Item struct {
	ID, Type, Role, Text, CallID, Name, Namespace, Input string
	ContinuationProvider                                 string
	ImageURL                                             json.RawMessage
	Arguments, Output, EncryptedContent                  json.RawMessage
	Content                                              []ContentPart
	Tools                                                []Tool
	ProviderData                                         json.RawMessage
}

// ContentPart preserves the ordering and shape of a Responses message's
// content array while the gateway derives portable text and image features.
type ContentPart struct {
	Type     string
	Text     string
	ImageURL json.RawMessage
}

// Tool describes a custom function tool.
type Tool struct {
	Type, Name, Description string
	Parameters              json.RawMessage
	Format                  *ToolFormat
	DeferLoading            *bool
	ExternalWebAccess       *bool
	Tools                   []Tool
	Strict                  bool
}

// ToolFormat defines the syntax accepted by a custom freeform tool.
type ToolFormat struct {
	Type, Syntax, Definition string
}

// JSONSchemaFormat requests JSON Schema structured output.
type JSONSchemaFormat struct {
	Type, Name, Description string
	Schema                  json.RawMessage
	Strict                  bool
}

// Result is the provider-neutral result returned by an adapter.
type Result struct {
	ID, Model, ProviderRequestID, Status string
	Output                               []Item
	Usage                                Usage
}

// Usage is normalized provider token accounting. Known distinguishes absent
// provider usage metadata from a reported zero.
type Usage struct {
	InputTokens, OutputTokens, CachedInputTokens int64
	Known                                        bool
}
