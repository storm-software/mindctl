package savings

import (
	"math"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestCostHonorsProviderCacheSemantics(t *testing.T) {
	pricing := domain.Pricing{InputPerMillion: 10, CachedInputPerMillion: 1, OutputPerMillion: 20, PerRequestUSD: 0.5}
	usage := inference.Usage{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000, Known: true}

	// OpenAI and Gemini input tokens include cached tokens.
	approx(t, "openai cost", Cost(pricing, usage, "openai"), 6+0.4+2+0.5)
	// Anthropic reports cache reads in addition to input tokens.
	approx(t, "anthropic cost", Cost(pricing, usage, "anthropic"), 10+0.4+2+0.5)
	approx(t, "unknown usage cost", Cost(pricing, inference.Usage{InputTokens: 1_000_000}, "openai"), 0)
}

func TestComputePricesObservedUsageAtBaselineAndServedRates(t *testing.T) {
	served := domain.Model{ID: "small", Provider: "openai", Pricing: domain.Pricing{InputPerMillion: 1, CachedInputPerMillion: 0.1, OutputPerMillion: 4}}
	baseline := domain.Model{ID: "large", Provider: "anthropic", Pricing: domain.Pricing{InputPerMillion: 10, CachedInputPerMillion: 1, OutputPerMillion: 40}}
	record := Compute(Input{
		Served:      served,
		Baseline:    &baseline,
		Usage:       inference.Usage{InputTokens: 2_000_000, CachedInputTokens: 1_000_000, OutputTokens: 500_000, Known: true},
		Compression: Compression{TokensBefore: 3_000_000, TokensSaved: 1_000_000},
	})

	approx(t, "actual cost", record.ActualCost, 1+0.1+2)
	// The baseline is priced with the served provider's token semantics.
	approx(t, "baseline cost", record.BaselineCost, 10+1+20)
	approx(t, "routing savings", record.RoutingSavings(), 31-3.1)
	approx(t, "compression savings", record.CompressionSavings, 1)
	approx(t, "total savings", record.TotalSavings(), 27.9+1)
	if record.BaselineProvider != "anthropic" || record.BaselineModelID != "large" {
		t.Errorf("baseline = %s/%s", record.BaselineProvider, record.BaselineModelID)
	}
	if record.InputTokens != 2_000_000 || record.CachedInputTokens != 1_000_000 || record.OutputTokens != 500_000 {
		t.Errorf("tokens = %d/%d/%d", record.InputTokens, record.CachedInputTokens, record.OutputTokens)
	}
	if record.CompressionTokensBefore != 3_000_000 || record.CompressionTokensSaved != 1_000_000 {
		t.Errorf("compression tokens = %d/%d", record.CompressionTokensBefore, record.CompressionTokensSaved)
	}
}

func TestComputeKeepsNegativeRoutingSavingsAndHandlesMissingInputs(t *testing.T) {
	cheap := domain.Model{ID: "cheap", Provider: "openai", Pricing: domain.Pricing{InputPerMillion: 1}}
	pricey := domain.Model{ID: "pricey", Provider: "openai", Pricing: domain.Pricing{InputPerMillion: 5}}
	usage := inference.Usage{InputTokens: 1_000_000, Known: true}

	escalated := Compute(Input{Served: pricey, Baseline: &cheap, Usage: usage})
	approx(t, "escalated routing savings", escalated.RoutingSavings(), -4)

	unpriced := Compute(Input{Served: cheap, Usage: usage})
	if unpriced.BaselineModelID != "" {
		t.Errorf("unpriced baseline = %q", unpriced.BaselineModelID)
	}
	approx(t, "unpriced routing savings", unpriced.RoutingSavings(), 0)

	unknown := Compute(Input{Served: cheap, Baseline: &pricey, Compression: Compression{TokensBefore: 10, TokensSaved: 20}})
	if unknown.BaselineModelID != "pricey" || unknown.ActualCost != 0 || unknown.BaselineCost != 0 {
		t.Errorf("unknown usage record = %+v", unknown)
	}
	if unknown.CompressionTokensSaved != 0 || unknown.CompressionSavings != 0 {
		t.Errorf("inconsistent compression metrics were recorded: %+v", unknown)
	}
}

func TestBaselineSelection(t *testing.T) {
	models := []domain.Model{
		{ID: "mid", Provider: "openai", Tier: domain.T3, Available: true, Order: 0},
		{ID: "top-b", Provider: "openai", Tier: domain.T5, Available: true, Order: 2},
		{ID: "top-a", Provider: "anthropic", Tier: domain.T5, Available: true, Order: 1},
		{ID: "disabled", Provider: "openai", Tier: domain.T6, Order: 3},
		{ID: "explicit-only", Provider: "openai", Tier: domain.T6, Available: true, ExplicitOnly: true, Order: 4},
	}
	for _, test := range []struct {
		name, requested, configured, want string
	}{
		{"explicit request is its own baseline", "mid", "top-b", "mid"},
		{"explicit-only request is its own baseline", "explicit-only", "", "explicit-only"},
		{"configured baseline for automatic routing", inference.AutomaticModel, "mid", "mid"},
		{"highest routable tier by catalog order", inference.AutomaticModel, "", "top-a"},
		{"unknown configured baseline falls back", inference.AutomaticModel, "missing", "top-a"},
		{"unknown explicit model has no baseline", "missing", "mid", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Baseline(models, test.requested, test.configured)
			if test.want == "" {
				if got != nil {
					t.Fatalf("baseline = %q, want none", got.ID)
				}
				return
			}
			if got == nil || got.ID != test.want {
				t.Fatalf("baseline = %v, want %q", got, test.want)
			}
		})
	}
	if Baseline(nil, inference.AutomaticModel, "") != nil {
		t.Error("empty catalog produced a baseline")
	}
}
