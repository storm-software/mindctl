// Package inference defines the provider-neutral request, result, and tool
// shapes shared by the gateway and all provider adapters.
package inference

import "encoding/json"

// AutomaticModel is the request model that asks the gateway to route; any
// other model is an explicit selection by the client.
const AutomaticModel = "mindctl-auto"

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
	Thinking                                    *ThinkingOptions
	AnthropicSystem                             []NativeSystemBlock
	AnthropicToolChoice                         *NativeToolChoice
	// AnthropicExtra holds caller Messages fields without a portable meaning.
	// They are forwarded verbatim and only to Anthropic.
	AnthropicExtra map[string]json.RawMessage
	// SessionKey is the gateway-internal routing-session digest. It is never
	// serialized, so no provider or sidecar receives it.
	SessionKey string `json:"-"`
}

type ThinkingOptions struct {
	Type         string `json:"type"`
	BudgetTokens int64  `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type NativeSystemBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// Raw is the caller's block, forwarded verbatim when it carries fields
	// without a typed equivalent here.
	Raw json.RawMessage `json:"-"`
}

func (b NativeSystemBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) != 0 {
		return b.Raw, nil
	}
	type plain NativeSystemBlock
	return json.Marshal(plain(b))
}

type NativeToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
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
	Author, Recipient                                    string
	ContinuationProvider                                 string
	// Raw JSON fields omit empty values so a JSON copy keeps them absent
	// instead of turning them into a non-empty null literal.
	ImageURL         json.RawMessage `json:",omitempty"`
	Arguments        json.RawMessage `json:",omitempty"`
	Output           json.RawMessage `json:",omitempty"`
	Summary          json.RawMessage `json:",omitempty"`
	EncryptedContent json.RawMessage `json:",omitempty"`
	Content          []ContentPart
	Tools            []Tool
	ProviderData     json.RawMessage `json:",omitempty"`
}

// ContentPart preserves the ordering and shape of a Responses message or
// agent message content array while the gateway derives portable features.
type ContentPart struct {
	Type             string
	Text             string
	EncryptedContent string
	ImageURL         json.RawMessage `json:",omitempty"`
}

// Tool describes a custom function tool.
type Tool struct {
	Type, Name, Description string
	Parameters              json.RawMessage `json:",omitempty"`
	Format                  *ToolFormat
	DeferLoading            *bool
	ExternalWebAccess       *bool
	Tools                   []Tool
	Strict                  bool
	CacheControl            json.RawMessage `json:",omitempty"`
	// AnthropicNative is the caller's verbatim Messages tool definition when
	// it carries fields, such as defer_loading, without a portable meaning.
	AnthropicNative json.RawMessage `json:",omitempty"`
}

// ToolFormat defines the syntax accepted by a custom freeform tool.
type ToolFormat struct {
	Type, Syntax, Definition string
}

// JSONSchemaFormat requests JSON Schema structured output.
type JSONSchemaFormat struct {
	Type, Name, Description string
	Schema                  json.RawMessage `json:",omitempty"`
	Strict                  bool
}

// Result is the provider-neutral result returned by an adapter.
type Result struct {
	ID, Model, ProviderRequestID, Status, StopReason string
	Output                                           []Item
	Usage                                            Usage
}

// Usage is normalized provider token accounting. Known distinguishes absent
// provider usage metadata from a reported zero. CacheWriteInputTokens are the
// input tokens written to a prompt cache, which only Anthropic reports.
type Usage struct {
	InputTokens, OutputTokens, CachedInputTokens int64
	CacheWriteInputTokens                        int64
	Known                                        bool
}
