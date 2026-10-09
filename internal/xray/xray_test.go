package xray

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/savings"
)

func TestBreakdownMatchesReportedUsageAndCost(t *testing.T) {
	pricing := domain.Pricing{InputPerMillion: 3, CachedInputPerMillion: 0.3, CacheWriteInputPerMillion: 3.75, OutputPerMillion: 15, PerRequestUSD: 0.001}
	request := Request{
		Pricing:      pricing,
		Instructions: strings.Repeat("You are a coding agent. ", 200),
		Tools: []inference.Tool{
			{Name: "Read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "mcp__github__search", Description: strings.Repeat("Search GitHub. ", 100)},
		},
		Input: []inference.Item{
			{Type: "message", Role: "user", Text: "Read main.go"},
			{Type: "function_call", CallID: "call-1", Name: "Read", Arguments: json.RawMessage(`{"path":"main.go"}`)},
			{Type: "function_call_output", CallID: "call-1", Output: json.RawMessage(`"` + strings.Repeat("package main ", 300) + `"`)},
		},
		Output: []inference.Item{{Type: "message", Role: "assistant", Text: "It is a main package."}},
	}

	for _, test := range []struct {
		provider string
		usage    inference.Usage
	}{
		// Anthropic reports cache reads and writes beside uncached input.
		{"anthropic", inference.Usage{InputTokens: 500, CachedInputTokens: 3000, CacheWriteInputTokens: 700, OutputTokens: 400, Known: true}},
		// OpenAI includes cached tokens in input.
		{"openai", inference.Usage{InputTokens: 4200, CachedInputTokens: 3000, OutputTokens: 400, Known: true}},
		// Output below the visible estimate is scaled down, leaving no reasoning.
		{"openai", inference.Usage{InputTokens: 4200, OutputTokens: 2, Known: true}},
	} {
		request.Provider, request.Usage = test.provider, test.usage
		components := Breakdown(request)
		uncached, cacheRead, cacheWrite, output := savings.TokenClasses(test.provider, test.usage)

		var gotInput, gotRead, gotWrite, gotFresh, gotOutput int64
		cost := 0.0
		toolResult := false
		for _, component := range components {
			cost += component.Cost
			if component.Section == SectionOutput {
				gotOutput += component.Tokens
				continue
			}
			gotInput += component.Tokens
			gotRead += component.CacheRead
			gotWrite += component.CacheWrite
			gotFresh += component.Fresh
			if component.CacheRead+component.CacheWrite+component.Fresh != component.Tokens {
				t.Errorf("%s: %+v cache split does not sum to its tokens", test.provider, component)
			}
			toolResult = toolResult || component.Category == ToolResults && component.Tool == "Read"
		}

		if gotInput != uncached+cacheRead+cacheWrite || gotRead != cacheRead || gotWrite != cacheWrite || gotFresh != uncached {
			t.Errorf("%s: input %d (read %d, write %d, fresh %d), want %d (%d, %d, %d)",
				test.provider, gotInput, gotRead, gotWrite, gotFresh, uncached+cacheRead+cacheWrite, cacheRead, cacheWrite, uncached)
		}
		if gotOutput != output {
			t.Errorf("%s: output %d, want %d", test.provider, gotOutput, output)
		}
		if want := savings.Cost(pricing, test.usage, test.provider); math.Abs(cost-want) > 1e-12 {
			t.Errorf("%s: cost %.10f, want %.10f", test.provider, cost, want)
		}
		if !toolResult {
			t.Errorf("%s: tool result not attributed to its call's tool", test.provider)
		}
		// The prompt prefix starts with tool schemas, so they read the cache first.
		if test.usage.CachedInputTokens > 0 && (components[0].Category != ToolSchemas || components[0].CacheRead == 0) {
			t.Errorf("%s: first component %+v, want cached tool schema", test.provider, components[0])
		}
	}
}

func TestBreakdownUnknownUsage(t *testing.T) {
	if components := Breakdown(Request{Provider: "openai"}); components != nil {
		t.Errorf("unknown usage components = %+v, want nil", components)
	}
}

func TestBreakdownWithoutContentIsUnattributed(t *testing.T) {
	components := Breakdown(Request{Provider: "openai", Usage: inference.Usage{InputTokens: 10, Known: true}})
	if len(components) != 1 || components[0].Category != Unattributed || components[0].Tokens != 10 {
		t.Errorf("components = %+v, want all input unattributed", components)
	}
}

func TestToolGroup(t *testing.T) {
	for name, want := range map[string]string{"mcp__github__search": "mcp:github", "mcp__solo": "mcp:solo", "Read": "Read"} {
		if got := ToolGroup(name); got != want {
			t.Errorf("ToolGroup(%q) = %q, want %q", name, got, want)
		}
	}
}
