package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/storage"
	"github.com/storm-software/mindctl/internal/xray"
)

func newXrayCommand(settings *viper.Viper) *cobra.Command {
	var filter storage.HistoryFilter
	var since, until string
	command := &cobra.Command{
		Use:   "xray [response-id]",
		Short: "Break down what requests sent to their providers and what each part cost",
		Long: "Attribute each request's tokens and cost to its instructions, tool schemas, messages,\n" +
			"tool calls and results, and response. Parts are estimated locally, scaled to the\n" +
			"provider's reported usage, and split into cache reads, cache writes, and fresh input\n" +
			"along the prompt prefix. Output tokens not covered by visible text and tool calls are\n" +
			"shown as reasoning.\n\n" +
			"With a response ID (see `mindctl history --response-id`), break down that request alone.\n\n" +
			"Inspired by cost-xray (https://github.com/tigerless-labs/cost-xray).",
		Example: "  mindctl xray\n  mindctl xray --conversation conv_abc --limit 0\n  mindctl xray resp_abc",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			var err error

			filter.Since, err = parseHistoryTime("since", since)
			if err != nil {
				return err
			}

			filter.Until, err = parseHistoryTime("until", until)
			if err != nil {
				return err
			}

			if filter.Limit < 0 {
				return errors.New("limit must be nonnegative")
			}

			if !filter.Since.IsZero() && !filter.Until.IsZero() && filter.Since.After(filter.Until) {
				return errors.New("since must not be after until")
			}

			if len(args) == 1 {
				filter.ResponseID = args[0]
			}
			filter.Status, filter.WithContext = "succeeded", true

			cfg, err := readHistoryConfig(command, settings)
			if err != nil {
				return err
			}

			db, err := openHistoryStorage(command.Context(), cfg, os.LookupEnv)
			if err != nil {
				return err
			}

			defer db.Close()

			records, err := db.ListHistory(command.Context(), filter)
			if err != nil {
				return err
			}

			if filter.ResponseID != "" && len(records) == 0 {
				return fmt.Errorf("no succeeded request with response ID %q", filter.ResponseID)
			}

			pricing := make(map[string]domain.Pricing, len(cfg.Models))
			for _, model := range cfg.Models {
				pricing[model.Provider+"/"+model.ID] = domain.Pricing{
					InputPerMillion: model.InputPrice, CachedInputPerMillion: model.CachedInputPrice(),
					CacheWriteInputPerMillion: model.CacheWriteInputPrice(), OutputPerMillion: model.OutputPrice,
					PerRequestUSD: model.PerRequestPriceUSD,
				}
			}

			return writeXray(command.OutOrStdout(), records, pricing, filter.ResponseID != "")
		},
	}

	command.Flags().StringVar(&filter.Provider, "provider", "", "filter by serving provider")
	command.Flags().StringVar(&filter.ModelID, "model", "", "filter by serving model")
	command.Flags().StringVar(&filter.ConversationID, "conversation", "", "filter by conversation, including its routing session")
	command.Flags().StringVar(&since, "since", "", "include requests at or after an RFC3339 timestamp")
	command.Flags().StringVar(&until, "until", "", "include requests at or before an RFC3339 timestamp")
	command.Flags().IntVar(&filter.Limit, "limit", 100, "maximum requests to include (0 means all)")

	return command
}

// xrayComponents breaks down every succeeded attempt with retained usage and
// counts the requests left out because their usage was not retained.
func xrayComponents(records []storage.HistoryRecord, pricing map[string]domain.Pricing) (components []xray.Component, requests, skipped int64) {
	for _, record := range records {
		request := xray.Request{Input: append(slices.Clone(record.Prior), record.Request...)}
		if record.Context != nil {
			request.Instructions, request.Tools = record.Context.Instructions, record.Context.Tools
		}

		counted := false
		for _, attempt := range record.Attempts {
			if attempt.Status != "succeeded" || attempt.Result == nil || !attempt.Result.Usage.Known {
				continue
			}

			request.Provider, request.Pricing = attempt.Provider, pricing[attempt.Provider+"/"+attempt.ModelID]
			request.Usage, request.Output = attempt.Result.Usage, attempt.Result.Output
			components = append(components, xray.Breakdown(request)...)
			requests, counted = requests+1, true
		}

		if !counted {
			skipped++
		}
	}

	return components, requests, skipped
}

// writeXray renders the totals, the breakdown by category, and the cost of
// each tool or MCP server. A single request also shows its identifiers.
func writeXray(output io.Writer, records []storage.HistoryRecord, pricing map[string]domain.Pricing, single bool) error {
	components, requests, skipped := xrayComponents(records, pricing)
	if requests == 0 {
		_, err := io.WriteString(output, "No requests with retained usage.\n")
		return err
	}

	var total xray.Component
	var outputTokens int64
	type categoryKey struct{ section, category string }
	categories := map[categoryKey]*xray.Component{}
	type toolTotals struct {
		name        string
		schema, use float64
		called      bool
	}
	tools := map[string]*toolTotals{}
	for _, component := range components {
		total.Cost += component.Cost
		if component.Section == xray.SectionOutput {
			outputTokens += component.Tokens
		} else {
			total.Tokens += component.Tokens
			total.CacheRead += component.CacheRead
			total.CacheWrite += component.CacheWrite
			total.Fresh += component.Fresh
		}

		key := categoryKey{component.Section, component.Category}
		if categories[key] == nil {
			categories[key] = &xray.Component{Section: component.Section, Category: component.Category}
		}
		category := categories[key]
		category.Tokens += component.Tokens
		category.CacheRead += component.CacheRead
		category.CacheWrite += component.CacheWrite
		category.Fresh += component.Fresh
		category.Cost += component.Cost

		if component.Tool == "" {
			continue
		}
		group := xray.ToolGroup(component.Tool)
		if tools[group] == nil {
			tools[group] = &toolTotals{name: group}
		}
		if component.Category == xray.ToolSchemas {
			tools[group].schema += component.Cost
		} else {
			tools[group].use += component.Cost
			tools[group].called = tools[group].called || component.Category == xray.ToolCalls
		}
	}

	var summary [][]string
	if single {
		record := records[0]
		var models []string
		for _, attempt := range record.Attempts {
			if attempt.Status == "succeeded" {
				models = append(models, attempt.Provider+"/"+attempt.ModelID)
			}
		}
		summary = append(summary,
			[]string{"Response", record.ResponseID},
			[]string{"Conversation", record.ConversationID},
			[]string{"Created", record.CreatedAt.Local().Format(historyCreatedLayout)},
			[]string{"Model", strings.Join(models, ", ")},
		)
	}
	requestsValue := formatCount(requests)
	if skipped > 0 {
		requestsValue += " (" + formatCount(skipped) + " without retained usage)"
	}
	summary = append(summary,
		[]string{"Requests", requestsValue},
		[]string{"Input tokens", formatCount(total.Tokens) + " (" + formatCount(total.CacheRead) + " cache reads, " +
			formatCount(total.CacheWrite) + " cache writes, " + formatCount(total.Fresh) + " fresh)"},
		[]string{"Output tokens", formatCount(outputTokens)},
		[]string{"Cost", formatUSD(total.Cost)},
	)
	if err := writeBoxTable(output, []string{"XRAY", "VALUE"}, [][][]string{summary}); err != nil {
		return err
	}

	sections := []string{xray.SectionStatic, xray.SectionMessages, xray.SectionOutput, xray.SectionRequest}
	sorted := make([]*xray.Component, 0, len(categories))
	for _, category := range categories {
		sorted = append(sorted, category)
	}
	slices.SortFunc(sorted, func(a, b *xray.Component) int {
		return cmp.Or(
			cmp.Compare(slices.Index(sections, a.Section), slices.Index(sections, b.Section)),
			cmp.Compare(b.Cost, a.Cost),
			cmp.Compare(a.Category, b.Category),
		)
	})
	rows := make([][][]string, 0, len(sorted))
	for _, category := range sorted {
		cacheRead, cacheWrite, fresh := formatCount(category.CacheRead), formatCount(category.CacheWrite), formatCount(category.Fresh)
		if category.Section == xray.SectionOutput || category.Section == xray.SectionRequest {
			cacheRead, cacheWrite, fresh = "-", "-", "-"
		}
		rows = append(rows, [][]string{{
			category.Section, category.Category, formatCount(category.Tokens),
			cacheRead, cacheWrite, fresh, formatUSD(category.Cost), xrayShare(category.Cost, total.Cost),
		}})
	}
	if err := writeBoxTable(output, []string{
		"SECTION", "CATEGORY", "TOKENS", "CACHE READ", "CACHE WRITE", "FRESH", "COST", "SHARE",
	}, rows); err != nil {
		return err
	}

	if len(tools) == 0 {
		return nil
	}

	toolRows := make([]*toolTotals, 0, len(tools))
	for _, tool := range tools {
		toolRows = append(toolRows, tool)
	}
	slices.SortFunc(toolRows, func(a, b *toolTotals) int {
		return cmp.Or(cmp.Compare(b.schema+b.use, a.schema+a.use), cmp.Compare(a.name, b.name))
	})
	rows = rows[:0]
	for _, tool := range toolRows {
		// A tool sent with every request but never called is dead weight.
		note := ""
		if !tool.called {
			note = "unused"
		}
		rows = append(rows, [][]string{{
			tool.name, formatUSD(tool.schema), formatUSD(tool.use), formatUSD(tool.schema + tool.use),
			xrayShare(tool.schema+tool.use, total.Cost), note,
		}})
	}

	return writeBoxTable(output, []string{"TOOL", "SCHEMA", "CALLS + RESULTS", "TOTAL", "SHARE", "NOTE"}, rows)
}

func xrayShare(value, total float64) string {
	if total <= 0 {
		return "-"
	}

	return fmt.Sprintf("%.1f%%", value/total*100)
}
