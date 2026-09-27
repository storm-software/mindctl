// Package savings estimates what automatic routing and Headroom compression
// saved for one successful provider attempt.
//
// Routing savings follow the Weave Router convention: the observed token usage
// is priced at both the baseline model's and the served model's rates, and the
// difference is the saving. The baseline is never re-run, and a negative
// difference (serving on a pricier model) is kept rather than clamped.
//
// Compression savings price the tokens Headroom removed at the served model's
// uncached input rate. The two parts are additive because routing savings are
// computed from the already-compressed usage.
package savings

import (
	"math"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

// Record is the content-free savings telemetry for one successful attempt.
// Costs are USD. An empty BaselineModelID means no baseline could be priced,
// so BaselineCost equals ActualCost and routing saved nothing.
type Record struct {
	InputTokens, CachedInputTokens, OutputTokens    int64
	ActualCost, BaselineCost                        float64
	BaselineProvider, BaselineModelID               string
	CompressionTokensBefore, CompressionTokensSaved int64
	CompressionSavings                              float64
}

// RoutingSavings is the baseline cost minus the served cost. It is negative
// when the request was served by a model pricier than the baseline.
func (r Record) RoutingSavings() float64 { return r.BaselineCost - r.ActualCost }

// TotalSavings adds routing and compression savings.
func (r Record) TotalSavings() float64 { return r.RoutingSavings() + r.CompressionSavings }

// Compression is the token accounting reported by Headroom for a request.
type Compression struct {
	TokensBefore, TokensSaved int64
}

// Input is everything needed to price one successful attempt.
type Input struct {
	// Served is the model that produced the result, and Baseline the model
	// the request is compared against; a nil Baseline has no known price.
	Served      domain.Model
	Baseline    *domain.Model
	Usage       inference.Usage
	Compression Compression
}

// Compute prices one attempt. Unknown provider usage prices no tokens, so it
// contributes no routing savings; compression savings do not depend on usage.
func Compute(in Input) Record {
	var record Record
	if in.Usage.Known {
		record.InputTokens = max(in.Usage.InputTokens, 0)
		record.CachedInputTokens = max(in.Usage.CachedInputTokens, 0)
		record.OutputTokens = max(in.Usage.OutputTokens, 0)
		// Token semantics depend on the provider that reported the usage, so
		// both sides are priced as the served provider counted them.
		record.ActualCost = Cost(in.Served.Pricing, in.Usage, in.Served.Provider)
		record.BaselineCost = record.ActualCost
		if in.Baseline != nil {
			record.BaselineProvider, record.BaselineModelID = in.Baseline.Provider, in.Baseline.ID
			record.BaselineCost = Cost(in.Baseline.Pricing, in.Usage, in.Served.Provider)
		}
	} else if in.Baseline != nil {
		record.BaselineProvider, record.BaselineModelID = in.Baseline.Provider, in.Baseline.ID
	}
	if in.Compression.TokensSaved > 0 && in.Compression.TokensBefore >= in.Compression.TokensSaved {
		record.CompressionTokensBefore = in.Compression.TokensBefore
		record.CompressionTokensSaved = in.Compression.TokensSaved
		record.CompressionSavings = finite(float64(in.Compression.TokensSaved) / 1e6 * in.Served.Pricing.InputPerMillion)
	}
	return record
}

// Cost prices usage as reported by provider. Anthropic reports cache reads
// separately from input tokens; other providers include them in input tokens.
func Cost(pricing domain.Pricing, usage inference.Usage, provider string) float64 {
	if !usage.Known {
		return 0
	}
	input := max(usage.InputTokens, 0)
	cached := max(usage.CachedInputTokens, 0)
	uncached := input
	if provider != "anthropic" {
		cached = min(cached, input)
		uncached = input - cached
	}
	cost := float64(uncached)/1e6*pricing.InputPerMillion +
		float64(cached)/1e6*pricing.CachedInputPerMillion +
		float64(max(usage.OutputTokens, 0))/1e6*pricing.OutputPerMillion +
		pricing.PerRequestUSD
	return finite(cost)
}

// Baseline selects the model a request is compared against. An explicitly
// requested model is its own baseline. Automatic requests use the configured
// baseline, falling back to the highest-tier model eligible for automatic
// routing, which is what the request would cost without routing down.
func Baseline(models []domain.Model, requested, configured string) *domain.Model {
	if requested != "" && requested != inference.AutomaticModel {
		return find(models, requested)
	}
	if configured != "" {
		if model := find(models, configured); model != nil {
			return model
		}
	}
	var best *domain.Model
	for index := range models {
		model := &models[index]
		if !model.Available || model.ExplicitOnly {
			continue
		}
		if best == nil || model.Tier > best.Tier || (model.Tier == best.Tier && model.Order < best.Order) {
			best = model
		}
	}
	if best == nil {
		return nil
	}
	clone := *best
	return &clone
}

func find(models []domain.Model, id string) *domain.Model {
	for _, model := range models {
		if model.ID == id {
			return &model
		}
	}
	return nil
}

func finite(value float64) float64 {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
