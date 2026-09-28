package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/savings"
)

//go:embed cc-statusline.sh
var claudeStatuslineScript string

// claudeStatuslineMarker is part of the installed script, so a user-owned
// script at the same path can be told apart from one mindctl manages.
const claudeStatuslineMarker = "# Claude Code statusline for the Mindctl router."

const claudeStatuslinePricingPlaceholder = "__MINDCTL_PRICING__"

func claudeStatuslinePath(home string) string {
	return filepath.Join(home, ".mindctl", "cc-statusline.sh")
}

// statuslinePricing is the price table embedded in the statusline script.
type statuslinePricing struct {
	BaselineModel string                          `json:"baseline_model,omitempty"`
	Models        map[string]statuslineModelPrice `json:"models"`
}

// statuslineModelPrice holds USD per million tokens.
type statuslineModelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// claudeStatuslinePricing reads model prices and the savings baseline from the
// user's gateway config. Without a readable config the table is empty and the
// statusline shows models and tokens but no savings.
func claudeStatuslinePricing() statuslinePricing {
	pricing := statuslinePricing{Models: map[string]statuslineModelPrice{}}
	path, err := userConfigPath()
	if err != nil {
		return pricing
	}

	cfg, err := config.Read(path)
	if err != nil {
		return pricing
	}

	models := make([]domain.Model, 0, len(cfg.Models))
	for index, model := range cfg.Models {
		tier, _ := domain.ParseTier(model.Tier)
		models = append(models, domain.Model{ID: model.ID, Tier: tier, Available: model.Available, ExplicitOnly: model.ExplicitOnly, Order: index})
		pricing.Models[model.ID] = statuslineModelPrice{
			Input:      model.InputPrice,
			Output:     model.OutputPrice,
			CacheRead:  model.CachedInputPrice(),
			CacheWrite: model.CacheWriteInputPrice(),
		}
	}

	if baseline := savings.Baseline(models, "", cfg.Savings.BaselineModel); baseline != nil {
		pricing.BaselineModel = baseline.ID
	}

	return pricing
}

// renderClaudeStatusline fills the script's price table. The table sits in a
// single-quoted shell string, so any single quote is closed, escaped, and
// reopened.
func renderClaudeStatusline(pricing statuslinePricing) ([]byte, error) {
	encoded, err := json.Marshal(pricing)
	if err != nil {
		return nil, err
	}

	quoted := strings.ReplaceAll(string(encoded), "'", `'\''`)
	return []byte(strings.Replace(claudeStatuslineScript, claudeStatuslinePricingPlaceholder, quoted, 1)), nil
}

// claudeStatuslineCommand returns the command of the statusLine setting, and
// whether any non-null statusLine is configured.
func claudeStatuslineCommand(settings *orderedObject) (string, bool) {
	raw, exists := settings.get("statusLine")
	if !exists || string(raw) == "null" {
		return "", false
	}

	var statusLine struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &statusLine)
	return statusLine.Command, true
}

// installClaudeStatusline writes the statusline script and points settings at
// it, unless the user already has a statusline of their own. It reports what
// it did for the setup message.
func installClaudeStatusline(home string, settings *orderedObject) (string, error) {
	path := claudeStatuslinePath(home)
	if command, configured := claudeStatuslineCommand(settings); configured && command != path {
		return "kept the existing Claude Code statusLine", nil
	}

	script, err := renderClaudeStatusline(claudeStatuslinePricing())
	if err != nil {
		return "", err
	}

	written, err := writeManagedFile(path, claudeStatuslineMarker, script, 0755)
	if err != nil {
		return "", fmt.Errorf("write Claude statusline: %w", err)
	}

	if written == "" {
		return fmt.Sprintf("left %s alone because mindctl did not write it", path), nil
	}

	statusLine, err := json.Marshal(struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}{"command", path})
	if err != nil {
		return "", err
	}
	settings.set("statusLine", statusLine)
	return written + " the statusline at " + path, nil
}

// uninstallClaudeStatusline removes the statusLine setting when it points at
// mindctl's script, and deletes the script when mindctl wrote it. It reports
// whether settings changed.
func uninstallClaudeStatusline(home string, settings *orderedObject) (bool, error) {
	path := claudeStatuslinePath(home)
	changed := false
	if command, _ := claudeStatuslineCommand(settings); command == path {
		settings.delete("statusLine")
		changed = true
	}

	if _, err := removeManagedFile(path, claudeStatuslineMarker); err != nil {
		return changed, err
	}

	return changed, nil
}
