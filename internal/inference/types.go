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
	Stream                                      bool
	MaxOutputTokens                             int64
}

// Item is a portable text, image, function-call, or function-result item.
// ProviderData remains opaque to the gateway at this layer.
type Item struct {
	Type, Role, Text, CallID, Name string
	ImageURL                       json.RawMessage
	Arguments, Output              json.RawMessage
	ProviderData                   json.RawMessage
}

// Tool describes a custom function tool.
type Tool struct {
	Type, Name, Description string
	Parameters              json.RawMessage
	Strict                  bool
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
