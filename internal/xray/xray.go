// Package xray attributes the tokens and cost of one provider request to its
// parts: instructions, tool schemas, conversation messages, tool calls and
// results, and the response.
//
// The approach follows cost-xray (https://github.com/tigerless-labs/cost-xray)
// by Tigerless Labs: estimate each part locally, calibrate the estimates so
// they sum exactly to the provider's reported usage, then walk the prompt
// prefix to split input into cache reads, cache writes, and fresh tokens.
package xray

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/savings"
)

// Sections, in prompt order, followed by the response.
const (
	SectionStatic   = "static"
	SectionMessages = "messages"
	SectionOutput   = "output"
	SectionRequest  = "request"
)

// Categories.
const (
	Instructions = "instructions"
	ToolSchemas  = "tool schemas"
	UserText     = "user text"
	Developer    = "developer text"
	Assistant    = "assistant text"
	Images       = "images"
	ToolCalls    = "tool calls"
	ToolResults  = "tool results"
	Reasoning    = "reasoning"
	Text         = "text"
	Unattributed = "unattributed"
	RequestFee   = "per-request fee"
)

// imageTokens is a flat estimate per image, near the upper bound providers
// bill for a typical screenshot.
// ponytail: flat image size, decode dimensions if images dominate a breakdown.
const imageTokens = 1600

// Component is one attributed part of a request. Tool is the tool name for
// tool schemas, calls, and results. Input components split Tokens into
// CacheRead, CacheWrite, and Fresh; output components bill Tokens as output.
// Cost is USD.
type Component struct {
	Section, Category, Tool      string
	Tokens                       int64
	CacheRead, CacheWrite, Fresh int64
	Cost                         float64
}

// Request is everything sent to and received from the provider for one
// attempt. Input is the full transcript the provider received, in order.
type Request struct {
	Provider     string
	Pricing      domain.Pricing
	Usage        inference.Usage
	Instructions string
	Tools        []inference.Tool
	Input        []inference.Item
	Output       []inference.Item
}

// Breakdown attributes the request's usage to its components. Token and cost
// totals match savings.Cost for the same usage. Unknown usage yields nil.
func Breakdown(request Request) []Component {
	if !request.Usage.Known {
		return nil
	}

	uncached, cacheRead, cacheWrite, output := savings.TokenClasses(request.Provider, request.Usage)
	input := inputComponents(request)
	weights := make([]int64, len(input))
	for index, component := range input {
		weights[index] = component.Tokens
	}
	for index, tokens := range allocate(uncached+cacheRead+cacheWrite, weights) {
		input[index].Tokens = tokens
	}
	// Cache reads cover the start of the prompt prefix and cache writes the
	// span after them; the rest is fresh.
	for index := range input {
		component := &input[index]
		component.CacheRead = min(component.Tokens, cacheRead)
		cacheRead -= component.CacheRead
		component.CacheWrite = min(component.Tokens-component.CacheRead, cacheWrite)
		cacheWrite -= component.CacheWrite
		component.Fresh = component.Tokens - component.CacheRead - component.CacheWrite
		component.Cost = perMillion(component.CacheRead, request.Pricing.CachedInputPerMillion) +
			perMillion(component.CacheWrite, request.Pricing.CacheWriteInputPerMillion) +
			perMillion(component.Fresh, request.Pricing.InputPerMillion)
	}

	components := append(input, outputComponents(request.Output, output, request.Pricing.OutputPerMillion)...)
	if request.Pricing.PerRequestUSD != 0 {
		components = append(components, Component{Section: SectionRequest, Category: RequestFee, Cost: request.Pricing.PerRequestUSD})
	}

	return components
}

// inputComponents lists the prompt in prefix order, tools then instructions
// then messages, with estimated Tokens. Content without a portable form, such
// as native thinking blocks, is not estimated; calibration spreads its tokens
// over the estimated parts.
func inputComponents(request Request) []Component {
	var components []Component
	for _, tool := range request.Tools {
		components = append(components, Component{Section: SectionStatic, Category: ToolSchemas, Tool: tool.Name, Tokens: toolTokens(tool)})
	}
	if request.Instructions != "" {
		components = append(components, Component{Section: SectionStatic, Category: Instructions, Tokens: estimate(request.Instructions)})
	}

	callNames := map[string]string{}
	for _, item := range request.Input {
		message := Component{Section: SectionMessages}
		switch {
		case item.Type == "message" || item.Type == "agent_message":
			message.Category = UserText
			switch item.Role {
			case "assistant":
				message.Category = Assistant
			case "developer", "system":
				message.Category = Developer
			}
			if item.Type == "agent_message" {
				message.Category = Assistant
			}
			message.Tokens = estimate(itemText(item))
			if len(item.ImageURL) != 0 {
				components = append(components, Component{Section: SectionMessages, Category: Images, Tokens: imageTokens})
			}
		case item.Type == "input_image":
			message.Category, message.Tokens = Images, imageTokens
		case item.Type == "function_call" || item.Type == "custom_tool_call":
			callNames[item.CallID] = item.Name
			message.Category, message.Tool = ToolCalls, item.Name
			message.Tokens = estimate(item.Name + string(item.Arguments) + item.Input)
		case strings.HasSuffix(item.Type, "_call_output"):
			message.Category, message.Tool = ToolResults, item.Name
			if message.Tool == "" {
				message.Tool = callNames[item.CallID]
			}
			message.Tokens = estimate(outputText(item.Output))
		case item.Type == "reasoning":
			message.Category = Reasoning
			message.Tokens = estimate(string(item.Summary) + string(item.EncryptedContent))
		case item.Type == "additional_tools":
			for _, tool := range item.Tools {
				components = append(components, Component{Section: SectionMessages, Category: ToolSchemas, Tool: tool.Name, Tokens: toolTokens(tool)})
			}
			continue
		default:
			continue
		}
		components = append(components, message)
	}

	// Calibration needs at least one weight to carry the reported tokens.
	total := int64(0)
	for _, component := range components {
		total += component.Tokens
	}
	if total == 0 {
		components = append(components, Component{Section: SectionMessages, Category: Unattributed, Tokens: 1})
	}

	return components
}

// outputComponents attributes outputTokens to the visible response and bills
// the remainder as reasoning, which providers either hide or return only as
// opaque signatures.
// ponytail: estimation error also lands in reasoning; calibrate against
// provider-reported reasoning tokens once usage carries them.
func outputComponents(items []inference.Item, outputTokens int64, rate float64) []Component {
	var components []Component
	for _, item := range items {
		switch item.Type {
		case "message", "agent_message":
			components = append(components, Component{Section: SectionOutput, Category: Text, Tokens: estimate(itemText(item))})
		case "function_call", "custom_tool_call":
			components = append(components, Component{Section: SectionOutput, Category: ToolCalls, Tool: item.Name,
				Tokens: estimate(item.Name + string(item.Arguments) + item.Input)})
		}
	}

	visible := int64(0)
	weights := make([]int64, len(components))
	for index, component := range components {
		visible += component.Tokens
		weights[index] = component.Tokens
	}
	if visible > outputTokens {
		for index, tokens := range allocate(outputTokens, weights) {
			components[index].Tokens = tokens
		}
		visible = outputTokens
	}
	if outputTokens > visible {
		components = append(components, Component{Section: SectionOutput, Category: Reasoning, Tokens: outputTokens - visible})
	}
	for index := range components {
		components[index].Cost = perMillion(components[index].Tokens, rate)
	}

	return components
}

// allocate splits total in proportion to weights. Rounding the running sum
// keeps the parts summing exactly to total.
func allocate(total int64, weights []int64) []int64 {
	sum := int64(0)
	for _, weight := range weights {
		sum += weight
	}
	parts := make([]int64, len(weights))
	if sum == 0 {
		return parts
	}

	running, assigned := int64(0), int64(0)
	for index, weight := range weights {
		running += weight
		next := (total*running + sum/2) / sum
		parts[index] = next - assigned
		assigned = next
	}

	return parts
}

// estimate approximates tokens as four runes each.
// ponytail: runes/4 heuristic; calibration fixes totals but not the split, use
// a real tokenizer if per-part precision matters.
func estimate(text string) int64 {
	return int64(utf8.RuneCountInString(text)+3) / 4
}

func toolTokens(tool inference.Tool) int64 {
	if len(tool.AnthropicNative) != 0 {
		return estimate(string(tool.AnthropicNative))
	}

	text := tool.Name + tool.Description + string(tool.Parameters)
	if tool.Format != nil {
		text += tool.Format.Definition
	}
	tokens := estimate(text)
	for _, nested := range tool.Tools {
		tokens += toolTokens(nested)
	}

	return tokens
}

func itemText(item inference.Item) string {
	if item.Text != "" {
		return item.Text
	}

	var text strings.Builder
	for _, part := range item.Content {
		text.WriteString(part.Text)
		text.WriteString(part.EncryptedContent)
	}

	return text.String()
}

// outputText unwraps a JSON string tool output; other shapes, such as content
// part arrays, are estimated from their JSON.
func outputText(output json.RawMessage) string {
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text
	}

	return string(output)
}

func perMillion(tokens int64, rate float64) float64 {
	return float64(tokens) / 1e6 * rate
}

// ToolGroup groups MCP tools named mcp__<server>__<tool> by server; other
// tools stand alone.
func ToolGroup(name string) string {
	if server, ok := strings.CutPrefix(name, "mcp__"); ok {
		server, _, _ = strings.Cut(server, "__")
		return "mcp:" + server
	}

	return name
}
