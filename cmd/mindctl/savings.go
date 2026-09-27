package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/storm-software/mindctl/internal/storage"
)

func newSavingsCommand(settings *viper.Viper) *cobra.Command {
	var filter storage.SavingsFilter
	var since, until, selection string
	command := &cobra.Command{
		Use:   "savings",
		Short: "Summarize token usage savings from routing and Headroom compression",
		Long: "Summarize token usage savings. Routing savings price each request's observed tokens at\n" +
			"the baseline model's rates and subtract the served model's cost; the baseline is the\n" +
			"requested model, or savings.baseline_model for automatically routed requests. Headroom\n" +
			"savings price the tokens removed by compression at the served model's input rate.",
		Args: noArgs("unexpected arguments for savings"),
		RunE: func(command *cobra.Command, _ []string) error {
			var err error

			filter.Since, err = parseHistoryTime("since", since)
			if err != nil {
				return err
			}

			filter.Until, err = parseHistoryTime("until", until)
			if err != nil {
				return err
			}

			if !filter.Since.IsZero() && !filter.Until.IsZero() && filter.Since.After(filter.Until) {
				return errors.New("since must not be after until")
			}

			switch selection {
			case "":
			case "explicit", "auto":
				explicit := selection == "explicit"
				filter.ExplicitModel = &explicit
			default:
				return errors.New("selection must be explicit or auto")
			}

			cfg, err := readHistoryConfig(command, settings)
			if err != nil {
				return err
			}

			db, err := openHistoryStorage(command.Context(), cfg, os.LookupEnv)
			if err != nil {
				return err
			}

			defer db.Close()

			summary, err := db.SummarizeSavings(command.Context(), filter)
			if err != nil {
				return err
			}

			return writeSavings(command.OutOrStdout(), summary, cfg.Headroom.Enabled)
		},
	}

	command.Flags().StringVar(&filter.Provider, "provider", "", "filter by serving provider")
	command.Flags().StringVar(&filter.ModelID, "model", "", "filter by serving model")
	command.Flags().StringVar(&selection, "selection", "", "filter by model selection (explicit or auto)")
	command.Flags().StringVar(&since, "since", "", "include requests at or after an RFC3339 timestamp")
	command.Flags().StringVar(&until, "until", "", "include requests at or before an RFC3339 timestamp")

	return command
}

// writeSavings renders the overall savings followed by a per-model breakdown.
// Negative routing savings mean requests were served by pricier models than
// their baselines, and are shown as such rather than clamped.
func writeSavings(output io.Writer, summary storage.SavingsSummary, headroomEnabled bool) error {
	total := summary.Total
	if total.Attempts == 0 {
		_, err := io.WriteString(output, "No savings recorded.\n")
		return err
	}

	headroom := formatUSD(total.CompressionSavings) + " · " + formatCount(total.CompressionTokensSaved) + " tokens" +
		percentOf(float64(total.CompressionTokensSaved), float64(total.CompressionTokensBefore), " of eligible")
	if !headroomEnabled && total.CompressionTokensSaved == 0 {
		headroom = "disabled"
	}

	rows := [][]string{
		{"Requests", formatCount(total.Attempts) + " (" + formatCount(total.AutomaticAttempts) + " auto-routed)"},
		{"Input tokens", formatCount(total.InputTokens) + " (" + formatCount(total.CachedInputTokens) + " cached, " + formatCount(total.CacheWriteInputTokens) + " cache writes)"},
		{"Output tokens", formatCount(total.OutputTokens)},
		{"Actual cost", formatUSD(total.ActualCost)},
		{"Baseline cost", formatUSD(total.BaselineCost)},
		{"Routing savings", formatUSD(total.RoutingSavings()) + percentOf(total.RoutingSavings(), total.BaselineCost, " of baseline")},
		{"Headroom savings", headroom},
		{"Total savings", formatUSD(total.TotalSavings()) + percentOf(total.TotalSavings(), total.BaselineCost+total.CompressionSavings, "")},
	}
	if err := writeBoxTable(output, []string{"SAVINGS", "VALUE"}, [][][]string{rows}); err != nil {
		return err
	}

	models := make([][][]string, 0, len(summary.Models))
	for _, model := range summary.Models {
		models = append(models, [][]string{{
			model.Provider + "/" + model.ModelID,
			formatCount(model.Attempts),
			formatUSD(model.ActualCost),
			formatUSD(model.BaselineCost),
			formatUSD(model.RoutingSavings()),
			formatCount(model.CompressionTokensSaved),
			formatUSD(model.CompressionSavings),
			formatUSD(model.TotalSavings()),
		}})
	}

	return writeBoxTable(output, []string{
		"MODEL", "REQUESTS", "ACTUAL", "BASELINE", "ROUTING SAVED", "HEADROOM TOKENS", "HEADROOM SAVED", "TOTAL SAVED",
	}, models)
}

// formatUSD keeps sub-dollar amounts legible, since single requests commonly
// cost fractions of a cent.
func formatUSD(value float64) string {
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}

	if value != 0 && value < 1 {
		return sign + "$" + strconv.FormatFloat(value, 'f', 4, 64)
	}

	return sign + "$" + strconv.FormatFloat(value, 'f', 2, 64)
}

func percentOf(value, base float64, suffix string) string {
	if base <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return ""
	}

	return fmt.Sprintf(" (%.1f%%%s)", value/base*100, suffix)
}

// formatCount groups digits in threes, such as 1,234,567.
func formatCount(value int64) string {
	digits := strconv.FormatInt(value, 10)
	sign := ""
	if value < 0 {
		sign, digits = "-", digits[1:]
	}

	grouped := make([]byte, 0, len(digits)+len(digits)/3)
	for index := range len(digits) {
		if index > 0 && (len(digits)-index)%3 == 0 {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, digits[index])
	}

	return sign + string(grouped)
}
