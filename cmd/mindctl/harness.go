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
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// routerOrigin is the local gateway origin that harness setup writes and
// status checks compare against.
const routerOrigin = "http://127.0.0.1:8080"

// harnessState describes how a harness is configured relative to the router.
type harnessState string

const (
	// harnessConnected means the harness sends requests to the router.
	harnessConnected harnessState = "connected"
	// harnessOff means the router settings are parked and can be restored
	// with `mindctl on`.
	harnessOff harnessState = "off"
	// harnessDisconnected means the harness is not set up for the router.
	harnessDisconnected harnessState = "disconnected"
)

// actionResult reports what a harness action did. Changed is false when the
// harness was already in the requested state.
type actionResult struct {
	Changed bool
	Message string
}

// harness describes a command-line client that can be pointed at the router.
type harness struct {
	ID          string
	Name        string
	Description string
	State       func(home string) (harnessState, error)
	Setup       func(home string) (actionResult, error)
	Off         func(home string) (actionResult, error)
	On          func(home string) (actionResult, error)
	Uninstall   func(home string) (actionResult, error)
}

var harnesses = []harness{
	{
		ID:          "claude",
		Name:        "Claude Code",
		Description: "patches ~/.claude/settings.json and adds a statusline",
		State:       claudeState,
		Setup:       setupClaude,
		Off:         offClaude,
		On:          onClaude,
		Uninstall:   uninstallClaude,
	},
	{
		ID:          "codex",
		Name:        "Codex",
		Description: "patches ~/.codex/config.toml and adds status and toggle hooks",
		State:       codexState,
		Setup:       setupCodex,
		Off:         offCodex,
		On:          onCodex,
		Uninstall:   uninstallCodex,
	},
}

func findHarness(id string) (harness, bool) {
	index := slices.IndexFunc(harnesses, func(candidate harness) bool { return candidate.ID == id })
	if index < 0 {
		return harness{}, false
	}

	return harnesses[index], true
}

// harnessAction is one operation that can be applied to several harnesses.
type harnessAction struct {
	// Name completes "Select the harnesses to ...".
	Name string
	Run  func(candidate harness, home string) (actionResult, error)
	// NextSteps, when set, is printed after any selected harness changed.
	NextSteps func(output io.Writer, selected []string) error
}

var (
	setupAction = harnessAction{
		Name:      "set up",
		Run:       func(candidate harness, home string) (actionResult, error) { return candidate.Setup(home) },
		NextSteps: writeSetupNextSteps,
	}
	offAction = harnessAction{
		Name: "turn off",
		Run:  func(candidate harness, home string) (actionResult, error) { return candidate.Off(home) },
	}
	onAction = harnessAction{
		Name: "turn on",
		Run:  func(candidate harness, home string) (actionResult, error) { return candidate.On(home) },
	}
	uninstallAction = harnessAction{
		Name: "uninstall",
		Run:  func(candidate harness, home string) (actionResult, error) { return candidate.Uninstall(home) },
	}
)

// addHarnessFlags adds one boolean flag per harness, such as --claude.
func addHarnessFlags(command *cobra.Command, verb string) {
	for _, candidate := range harnesses {
		command.Flags().Bool(candidate.ID, false, verb+" "+candidate.Name)
	}
}

// requestedHarnesses returns the IDs of the harness flags that were set.
func requestedHarnesses(command *cobra.Command) []string {
	var requested []string
	for _, candidate := range harnesses {
		if enabled, _ := command.Flags().GetBool(candidate.ID); enabled {
			requested = append(requested, candidate.ID)
		}
	}

	return requested
}

// newHarnessActionCommand builds a subcommand, such as `mindctl off`, that
// applies action to the flagged harnesses or to those picked from a menu.
func newHarnessActionCommand(use, short string, action harnessAction) *cobra.Command {
	command := &cobra.Command{
		Use:     use,
		Short:   short,
		Example: fmt.Sprintf("  mindctl %[1]s\n  mindctl %[1]s --claude\n  mindctl %[1]s --codex --claude", use),
		Args:    noArgs("unexpected arguments for " + use),
		RunE: func(command *cobra.Command, _ []string) error {
			return runHarnessAction(command, requestedHarnesses(command), action)
		},
	}

	addHarnessFlags(command, action.Name)
	return command
}

// runHarnessAction applies action to the requested harnesses, or asks which
// ones to use when none were requested.
func runHarnessAction(command *cobra.Command, requested []string, action harnessAction) error {
	home, err := userHome()
	if err != nil {
		return err
	}

	selected := requested
	if len(selected) == 0 {
		selected, err = promptHarnesses(command.InOrStdin(), command.OutOrStdout(), home, action.Name)
		if err != nil {
			return err
		}
	}

	output := command.OutOrStdout()
	changed := false
	for _, candidate := range harnesses {
		if !slices.Contains(selected, candidate.ID) {
			continue
		}

		result, err := action.Run(candidate, home)
		if err != nil {
			return fmt.Errorf("%s %s: %w", action.Name, candidate.Name, err)
		}

		marker := "•"
		if result.Changed {
			marker, changed = "✓", true
		}

		if _, err := fmt.Fprintf(output, "%s %s: %s\n", marker, candidate.Name, result.Message); err != nil {
			return err
		}
	}

	if changed && action.NextSteps != nil {
		return action.NextSteps(output, selected)
	}

	return nil
}

// promptHarnesses lists the harnesses and reads one or more selections,
// asking again after invalid input.
func promptHarnesses(input io.Reader, output io.Writer, home, actionName string) ([]string, error) {
	if _, err := fmt.Fprintf(output, "Select the harnesses to %s:\n", actionName); err != nil {
		return nil, err
	}

	for index, candidate := range harnesses {
		status := "unknown"
		if state, err := candidate.State(home); err == nil {
			status = string(state)
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

func userHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}

	return home, nil
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

func (o *orderedObject) delete(key string) {
	if _, exists := o.values[key]; !exists {
		return
	}

	delete(o.values, key)
	o.keys = slices.DeleteFunc(o.keys, func(candidate string) bool { return candidate == key })
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

// managedFileOwned reports whether the file at path carries marker, which
// mindctl writes into every file it manages. A missing file counts as owned so
// setup can create it.
func managedFileOwned(path, marker string) (bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}

	if err != nil {
		return false, err
	}

	return bytes.Contains(body, []byte(marker)), nil
}

// writeManagedFile writes body to path unless a file there lacks marker, in
// which case the user owns it and it is left alone. It reports "added" or
// "refreshed", or "" when it skipped the file. The previous file is mindctl's
// own, so it is replaced without a backup.
func writeManagedFile(path, marker string, body []byte, perm os.FileMode) (string, error) {
	owned, err := managedFileOwned(path, marker)
	if err != nil || !owned {
		return "", err
	}

	_, statErr := os.Stat(path)
	if err := writeSetupFile(path, nil, body, perm); err != nil {
		return "", err
	}

	if statErr == nil {
		return "refreshed", nil
	}

	return "added", nil
}

// removeManagedFile deletes path when it carries marker and reports whether
// it did.
func removeManagedFile(path, marker string) (bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	owned, err := managedFileOwned(path, marker)
	if err != nil || !owned {
		return false, err
	}

	return true, removeIfExists(path)
}
