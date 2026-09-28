package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// routerOrigin is the local gateway origin that harness setup writes and
// status checks compare against.
const routerOrigin = "http://127.0.0.1:8080"

// harness describes a command-line client that can be pointed at the router.
type harness struct {
	ID          string
	Name        string
	Description string
	Connected   func() (bool, error)
	Setup       func(home string) (string, error)
}

var harnesses = []harness{
	{
		ID:          "claude",
		Name:        "Claude Code",
		Description: "patches ~/.claude/settings.json",
		Connected:   claudeConnected,
		Setup:       setupClaude,
	},
	{
		ID:          "codex",
		Name:        "Codex",
		Description: "patches ~/.codex/config.toml",
		Connected:   codexConnected,
		Setup:       setupCodex,
	},
}

// runSetup configures the requested harnesses, or asks which ones to
// configure when none were requested.
func runSetup(command *cobra.Command, requested []string) error {
	selected := requested
	if len(selected) == 0 {
		var err error
		selected, err = promptHarnesses(command.InOrStdin(), command.OutOrStdout())
		if err != nil {
			return err
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}

	output := command.OutOrStdout()
	for _, candidate := range harnesses {
		if !slices.Contains(selected, candidate.ID) {
			continue
		}

		summary, err := candidate.Setup(home)
		if err != nil {
			return fmt.Errorf("set up %s: %w", candidate.Name, err)
		}

		if _, err := fmt.Fprintf(output, "✓ %s: %s\n", candidate.Name, summary); err != nil {
			return err
		}
	}

	return writeSetupNextSteps(output, selected)
}

// promptHarnesses lists the harnesses and reads one or more selections,
// asking again after invalid input.
func promptHarnesses(input io.Reader, output io.Writer) ([]string, error) {
	if _, err := fmt.Fprintln(output, "Select the harnesses to route through Mindctl:"); err != nil {
		return nil, err
	}

	for index, candidate := range harnesses {
		status := "unknown"
		if connected, err := candidate.Connected(); err == nil {
			status = connectionStatus(connected)
		}

		if _, err := fmt.Fprintf(output, "  %d) %-12s — %s (%s)\n", index+1, candidate.Name, candidate.Description, status); err != nil {
			return nil, err
		}
	}

	reader := bufio.NewReader(input)
	for {
		if _, err := fmt.Fprintf(output, "Choose one or more [1-%d, comma separated, or \"all\"]: ", len(harnesses)); err != nil {
			return nil, err
		}

		line, readErr := reader.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			selected, err := parseHarnessSelection(line)
			if err == nil {
				return selected, nil
			}

			if _, err := fmt.Fprintln(output, err); err != nil {
				return nil, err
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, errors.New("no harness selected; pass --claude or --codex to skip the menu")
			}
			return nil, fmt.Errorf("read harness selection: %w", readErr)
		}
	}
}

// parseHarnessSelection accepts menu numbers, harness IDs, or "all",
// separated by commas or spaces, and returns harness IDs in menu order.
func parseHarnessSelection(line string) ([]string, error) {
	fields := strings.FieldsFunc(strings.ToLower(line), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
	})
	chosen := make(map[string]bool, len(harnesses))
	for _, field := range fields {
		if field == "all" || field == "a" {
			for _, candidate := range harnesses {
				chosen[candidate.ID] = true
			}
			continue
		}

		index, err := strconv.Atoi(field)
		switch {
		case err == nil && index >= 1 && index <= len(harnesses):
			chosen[harnesses[index-1].ID] = true
		case slices.ContainsFunc(harnesses, func(candidate harness) bool { return candidate.ID == field }):
			chosen[field] = true
		default:
			return nil, fmt.Errorf("invalid choice %q", field)
		}
	}

	if len(chosen) == 0 {
		return nil, errors.New("select at least one harness")
	}

	selected := make([]string, 0, len(chosen))
	for _, candidate := range harnesses {
		if chosen[candidate.ID] {
			selected = append(selected, candidate.ID)
		}
	}

	return selected, nil
}

func writeSetupNextSteps(output io.Writer, selected []string) error {
	lines := []string{
		"",
		"Next steps:",
		"  Start the gateway with `mindctl serve`.",
		"  Export MINDCTL_GATEWAY_TOKEN with the gateway's client token.",
	}
	if slices.Contains(selected, "claude") {
		lines = append(lines,
			"  Claude Code: set `client_auth.header: X-Mindctl-Token` and `claude_messages.enabled: true`",
			"  in the gateway config, and launch Claude with",
			"  ANTHROPIC_CUSTOM_HEADERS=\"X-Mindctl-Token: ${MINDCTL_GATEWAY_TOKEN}\".",
		)
	}

	_, err := io.WriteString(output, strings.Join(lines, "\n")+"\n")
	return err
}

// setupClaude points Claude Code's global settings at the router, keeping
// every other setting and its order.
func setupClaude(home string) (string, error) {
	path := filepath.Join(home, ".claude", "settings.json")
	body, perm, err := readSetupFile(path)
	if err != nil {
		return "", fmt.Errorf("read Claude settings: %w", err)
	}

	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}

	settings, err := decodeOrderedObject(body)
	if err != nil {
		return "", fmt.Errorf("read Claude settings: %w", err)
	}

	env := orderedObject{}
	if raw, exists := settings.get("env"); exists && string(raw) != "null" {
		env, err = decodeOrderedObject(raw)
		if err != nil {
			return "", fmt.Errorf("read Claude settings: env: %w", err)
		}
	}

	previous := ""
	if raw, exists := env.get("ANTHROPIC_BASE_URL"); exists {
		_ = json.Unmarshal(raw, &previous)
	}

	origin, _ := json.Marshal(routerOrigin)
	env.set("ANTHROPIC_BASE_URL", origin)
	encodedEnv, err := env.marshal()
	if err != nil {
		return "", err
	}

	settings.set("env", encodedEnv)
	encoded, err := settings.marshal()
	if err != nil {
		return "", err
	}

	var formatted bytes.Buffer
	if err := json.Indent(&formatted, encoded, "", "  "); err != nil {
		return "", err
	}

	formatted.WriteByte('\n')
	if err := writeSetupFile(path, body, formatted.Bytes(), perm); err != nil {
		return "", fmt.Errorf("write Claude settings: %w", err)
	}

	summary := fmt.Sprintf("set env.ANTHROPIC_BASE_URL to %s in %s", routerOrigin, path)
	if previous != "" && strings.TrimSuffix(previous, "/") != routerOrigin {
		summary += fmt.Sprintf(" (was %s)", previous)
	}

	return summary, nil
}

const codexProviderBlock = `# Added by mindctl.
[model_providers.mindctl]
name = "Mindctl"
base_url = "` + routerOrigin + `/v1"
wire_api = "responses"
requires_openai_auth = true
env_http_headers = { "X-Mindctl-Token" = "MINDCTL_GATEWAY_TOKEN" }
`

var (
	codexTableHeader    = regexp.MustCompile(`^\s*\[`)
	codexProviderHeader = regexp.MustCompile(`^\s*\[\s*model_providers\s*\.\s*"?mindctl"?\s*\]\s*(#.*)?$`)
)

// setupCodex selects the Mindctl provider and automatic routing in Codex's
// global configuration. It edits lines in place so comments and unrelated
// settings survive, and keeps an existing [model_providers.mindctl] table.
func setupCodex(home string) (string, error) {
	path := filepath.Join(home, ".codex", "config.toml")
	body, perm, err := readSetupFile(path)
	if err != nil {
		return "", fmt.Errorf("read Codex config: %w", err)
	}

	if _, err := parseCodexConfig(body); err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}

	topLevelEnd := slices.IndexFunc(lines, codexTableHeader.MatchString)
	if topLevelEnd < 0 {
		topLevelEnd = len(lines)
	}

	var assignments []string
	for _, setting := range []struct{ key, value string }{
		{"model_provider", "mindctl"},
		{"model", "mindctl-auto"},
	} {
		assignment := fmt.Sprintf("%s = %q", setting.key, setting.value)
		pattern := regexp.MustCompile(`^\s*` + setting.key + `\s*=`)
		if index := slices.IndexFunc(lines[:topLevelEnd], pattern.MatchString); index >= 0 {
			lines[index] = assignment
			continue
		}

		assignments = append(assignments, assignment)
	}

	lines = slices.Insert(lines, 0, assignments...)
	if !slices.ContainsFunc(lines, codexProviderHeader.MatchString) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(strings.TrimRight(codexProviderBlock, "\n"), "\n")...)
	}

	updated := []byte(strings.Join(lines, "\n") + "\n")
	settings, err := parseCodexConfig(updated)
	if err != nil {
		return "", fmt.Errorf("update Codex config: %w", err)
	}

	if settings.GetString("model_provider") != "mindctl" || !settings.IsSet("model_providers.mindctl.base_url") {
		return "", errors.New("update Codex config: the mindctl provider is defined in a form setup cannot edit; configure it manually")
	}

	if err := writeSetupFile(path, body, updated, perm); err != nil {
		return "", fmt.Errorf("write Codex config: %w", err)
	}

	return fmt.Sprintf("set model_provider = \"mindctl\" and model = \"mindctl-auto\" in %s", path), nil
}

func parseCodexConfig(body []byte) (*viper.Viper, error) {
	settings := viper.New()
	settings.SetConfigType("toml")
	if err := settings.ReadConfig(bytes.NewReader(body)); err != nil {
		return nil, fmt.Errorf("read Codex config: %w", err)
	}

	return settings, nil
}

// readSetupFile returns a file's contents and permissions, or no contents and
// owner-only permissions when it does not exist yet.
func readSetupFile(path string) ([]byte, os.FileMode, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0600, nil
	}

	if err != nil {
		return nil, 0, err
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}

	return body, info.Mode().Perm(), nil
}

// writeSetupFile atomically replaces path with updated. When it changes an
// existing file, the original is first saved beside it with a
// ".mindctl-backup" suffix.
func writeSetupFile(path string, original, updated []byte, perm os.FileMode) error {
	if original != nil && bytes.Equal(original, updated) {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	if original != nil {
		if err := os.WriteFile(path+".mindctl-backup", original, perm); err != nil {
			return fmt.Errorf("back up original: %w", err)
		}
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(perm); err != nil {
		temporary.Close()
		return err
	}

	if _, err := temporary.Write(updated); err != nil {
		temporary.Close()
		return err
	}

	if err := temporary.Close(); err != nil {
		return err
	}

	return os.Rename(temporaryPath, path)
}

// orderedObject is a JSON object whose members keep their original order and
// raw values.
type orderedObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func decodeOrderedObject(body []byte) (orderedObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return orderedObject{}, err
	}

	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return orderedObject{}, errors.New("expected a JSON object")
	}

	object := orderedObject{values: map[string]json.RawMessage{}}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return orderedObject{}, err
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return orderedObject{}, err
		}

		object.set(token.(string), value)
	}

	if _, err := decoder.Token(); err != nil {
		return orderedObject{}, err
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return orderedObject{}, errors.New("unexpected data after JSON object")
	}

	return object, nil
}

func (o *orderedObject) get(key string) (json.RawMessage, bool) {
	value, exists := o.values[key]
	return value, exists
}

func (o *orderedObject) set(key string, value json.RawMessage) {
	if o.values == nil {
		o.values = map[string]json.RawMessage{}
	}

	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}

	o.values[key] = value
}

func (o *orderedObject) marshal() (json.RawMessage, error) {
	var body bytes.Buffer
	body.WriteByte('{')
	for index, key := range o.keys {
		if index > 0 {
			body.WriteByte(',')
		}

		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}

		body.Write(encodedKey)
		body.WriteByte(':')
		if err := json.Compact(&body, o.values[key]); err != nil {
			return nil, err
		}
	}

	body.WriteByte('}')
	return body.Bytes(), nil
}
