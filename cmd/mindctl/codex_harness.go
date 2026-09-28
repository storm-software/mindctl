package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

const codexProviderComment = "# Added by mindctl."

const codexProviderBlock = codexProviderComment + `
[model_providers.mindctl]
name = "Mindctl"
base_url = "` + routerOrigin + `/v1"
wire_api = "responses"
requires_openai_auth = true
env_http_headers = { "X-Mindctl-Token" = "MINDCTL_GATEWAY_TOKEN" }
`

// codexOffMarker ends a top-level line that `mindctl off` commented out.
const codexOffMarker = "# mindctl: off"

var (
	codexTableHeader    = regexp.MustCompile(`^\s*\[`)
	codexProviderHeader = regexp.MustCompile(`^\s*\[\s*model_providers\s*\.\s*"?mindctl"?\s*\]\s*(#.*)?$`)
	codexActiveLine     = regexp.MustCompile(`^\s*(model_provider\s*=\s*"mindctl"|model\s*=\s*"mindctl-auto")\s*(#.*)?$`)
	codexParkedLine     = regexp.MustCompile(`^\s*#\s*(model_provider|model)\s*=.*` + regexp.QuoteMeta(codexOffMarker) + `\s*$`)
)

// codexDocument is Codex's global config.toml as lines, edited in place so
// comments and unrelated settings survive.
type codexDocument struct {
	path     string
	original []byte
	perm     os.FileMode
	lines    []string
}

func readCodexDocument(home string) (*codexDocument, error) {
	path := filepath.Join(home, ".codex", "config.toml")
	body, perm, err := readSetupFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Codex config: %w", err)
	}

	if _, err := parseCodexConfig(body); err != nil {
		return nil, err
	}

	document := &codexDocument{path: path, original: body, perm: perm}
	if trimmed := strings.TrimRight(string(body), "\n"); trimmed != "" {
		document.lines = strings.Split(trimmed, "\n")
	}

	return document, nil
}

func parseCodexConfig(body []byte) (*viper.Viper, error) {
	settings := viper.New()
	settings.SetConfigType("toml")
	if err := settings.ReadConfig(bytes.NewReader(body)); err != nil {
		return nil, fmt.Errorf("read Codex config: %w", err)
	}

	return settings, nil
}

// topLevelEnd returns the index of the first table header, which ends the
// top-level keys.
func (d *codexDocument) topLevelEnd() int {
	if index := slices.IndexFunc(d.lines, codexTableHeader.MatchString); index >= 0 {
		return index
	}

	return len(d.lines)
}

// setTopLevel assigns top-level string values. An existing assignment is
// replaced when replace is set and kept otherwise; missing assignments are
// added at the top of the file in the given order.
func (d *codexDocument) setTopLevel(settings ...codexSetting) {
	var missing []string
	for _, setting := range settings {
		assignment := fmt.Sprintf("%s = %q", setting.key, setting.value)
		pattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(setting.key) + `\s*=`)
		index := slices.IndexFunc(d.lines[:d.topLevelEnd()], pattern.MatchString)
		switch {
		case index < 0:
			missing = append(missing, assignment)
		case setting.replace:
			d.lines[index] = assignment
		}
	}

	d.lines = slices.Insert(d.lines, 0, missing...)
}

type codexSetting struct {
	key, value string
	replace    bool
}

// removeTopLevel deletes top-level lines that match pattern and returns the
// removed lines.
func (d *codexDocument) removeTopLevel(pattern *regexp.Regexp) []string {
	var removed []string
	end := d.topLevelEnd()
	kept := make([]string, 0, len(d.lines))
	for index, line := range d.lines {
		if index < end && pattern.MatchString(line) {
			removed = append(removed, line)
			continue
		}
		kept = append(kept, line)
	}

	d.lines = kept
	return removed
}

func (d *codexDocument) hasProvider() bool {
	return slices.ContainsFunc(d.lines, codexProviderHeader.MatchString)
}

func (d *codexDocument) addProvider() {
	if d.hasProvider() {
		return
	}

	if len(d.lines) > 0 {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, strings.Split(strings.TrimRight(codexProviderBlock, "\n"), "\n")...)
}

// removeProvider deletes the [model_providers.mindctl] table and the comment
// setup placed above it, and reports whether the table existed.
func (d *codexDocument) removeProvider() bool {
	start := slices.IndexFunc(d.lines, codexProviderHeader.MatchString)
	if start < 0 {
		return false
	}

	end := len(d.lines)
	if next := slices.IndexFunc(d.lines[start+1:], codexTableHeader.MatchString); next >= 0 {
		end = start + 1 + next
	}

	if start > 0 && strings.TrimSpace(d.lines[start-1]) == codexProviderComment {
		start--
	}

	d.lines = slices.Delete(d.lines, start, end)
	for len(d.lines) > 0 && strings.TrimSpace(d.lines[len(d.lines)-1]) == "" {
		d.lines = d.lines[:len(d.lines)-1]
	}

	return true
}

// write validates the edited document with check and replaces the config
// file only when both pass.
func (d *codexDocument) write(check func(settings *viper.Viper) error) error {
	var updated []byte
	if len(d.lines) > 0 {
		updated = []byte(strings.Join(d.lines, "\n") + "\n")
	}

	settings, err := parseCodexConfig(updated)
	if err != nil {
		return fmt.Errorf("update Codex config: %w", err)
	}

	if err := check(settings); err != nil {
		return fmt.Errorf("update Codex config: %w", err)
	}

	if err := writeSetupFile(d.path, d.original, updated, d.perm); err != nil {
		return fmt.Errorf("write Codex config: %w", err)
	}

	return nil
}

func codexState(home string) (harnessState, error) {
	document, err := readCodexDocument(home)
	if err != nil {
		return "", err
	}

	settings, err := parseCodexConfig(document.original)
	if err != nil {
		return "", err
	}

	provider := settings.GetString("model_provider")
	if provider == "mindctl" {
		return harnessConnected, nil
	}

	topLevel := document.lines[:document.topLevelEnd()]
	if provider == "" && slices.ContainsFunc(topLevel, codexParkedLine.MatchString) {
		return harnessOff, nil
	}

	return harnessDisconnected, nil
}

// setupCodex selects the Mindctl provider and automatic routing in Codex's
// global configuration, keeping an existing [model_providers.mindctl] table.
func setupCodex(home string) (actionResult, error) {
	document, err := readCodexDocument(home)
	if err != nil {
		return actionResult{}, err
	}

	document.removeTopLevel(codexParkedLine)
	document.setTopLevel(
		codexSetting{key: "model_provider", value: "mindctl", replace: true},
		codexSetting{key: "model", value: "mindctl-auto", replace: true},
	)
	document.addProvider()
	if err := document.write(requireCodexRouted); err != nil {
		return actionResult{}, err
	}

	return actionResult{
		Changed: true,
		Message: fmt.Sprintf("set model_provider = \"mindctl\" and model = \"mindctl-auto\" in %s", document.path),
	}, nil
}

// offCodex comments out the top-level Mindctl selection so Codex uses its
// default provider; the [model_providers.mindctl] table stays for onCodex.
func offCodex(home string) (actionResult, error) {
	state, err := codexState(home)
	if err != nil {
		return actionResult{}, err
	}

	switch state {
	case harnessOff:
		return actionResult{Message: "already off; nothing to do"}, nil
	case harnessDisconnected:
		return actionResult{Message: "not set up for the router; run `mindctl --codex` first"}, nil
	}

	document, err := readCodexDocument(home)
	if err != nil {
		return actionResult{}, err
	}

	for index, line := range document.lines[:document.topLevelEnd()] {
		if codexActiveLine.MatchString(line) {
			document.lines[index] = "# " + strings.TrimSpace(line) + "  " + codexOffMarker
		}
	}

	if err := document.write(requireCodexUnrouted); err != nil {
		return actionResult{}, err
	}

	return actionResult{Changed: true, Message: "off; Codex uses its default provider from its next run"}, nil
}

// onCodex restores the Mindctl selection commented out by offCodex. A
// top-level model chosen while routing was off is kept.
func onCodex(home string) (actionResult, error) {
	state, err := codexState(home)
	if err != nil {
		return actionResult{}, err
	}

	switch state {
	case harnessConnected:
		return actionResult{Message: "already on; nothing to do"}, nil
	case harnessDisconnected:
		return actionResult{Message: "not set up for the router; run `mindctl --codex` first"}, nil
	}

	document, err := readCodexDocument(home)
	if err != nil {
		return actionResult{}, err
	}

	// Uncomment parked lines in place. A parked model is dropped when another
	// top-level model was chosen while routing was off.
	end := document.topLevelEnd()
	activeModel := regexp.MustCompile(`^\s*model\s*=`)
	hasModel := slices.ContainsFunc(document.lines[:end], activeModel.MatchString)
	restored := make([]string, 0, len(document.lines))
	for index, line := range document.lines {
		if index < end && codexParkedLine.MatchString(line) {
			key := codexParkedLine.FindStringSubmatch(line)[1]
			switch {
			case key == "model_provider":
				restored = append(restored, `model_provider = "mindctl"`)
			case !hasModel:
				restored = append(restored, `model = "mindctl-auto"`)
				hasModel = true
			}
			continue
		}
		restored = append(restored, line)
	}

	document.lines = restored
	document.setTopLevel(
		codexSetting{key: "model_provider", value: "mindctl", replace: true},
		codexSetting{key: "model", value: "mindctl-auto"},
	)
	document.addProvider()
	if err := document.write(requireCodexRouted); err != nil {
		return actionResult{}, err
	}

	return actionResult{Changed: true, Message: "on; Codex routes through Mindctl from its next run"}, nil
}

// uninstallCodex removes the Mindctl selection, including one turned off,
// and the [model_providers.mindctl] table.
func uninstallCodex(home string) (actionResult, error) {
	document, err := readCodexDocument(home)
	if err != nil {
		return actionResult{}, err
	}

	removed := len(document.removeTopLevel(codexActiveLine)) + len(document.removeTopLevel(codexParkedLine))
	if !document.removeProvider() && removed == 0 {
		return actionResult{Message: "not set up for the router; nothing to remove"}, nil
	}

	if err := document.write(requireCodexUnrouted); err != nil {
		return actionResult{}, err
	}

	return actionResult{Changed: true, Message: "removed the Mindctl router from " + document.path}, nil
}

// requireCodexRouted fails when the provider is defined in a form the line
// edits cannot reach, so Codex would not route through Mindctl.
func requireCodexRouted(settings *viper.Viper) error {
	if settings.GetString("model_provider") != "mindctl" || !settings.IsSet("model_providers.mindctl.base_url") {
		return errors.New("the mindctl provider is defined in a form mindctl cannot edit; configure it manually")
	}

	return nil
}

// requireCodexUnrouted fails when model_provider still selects Mindctl in a
// form the line edits cannot reach.
func requireCodexUnrouted(settings *viper.Viper) error {
	if settings.GetString("model_provider") == "mindctl" {
		return errors.New("model_provider selects mindctl in a form mindctl cannot edit; change it manually")
	}

	return nil
}
