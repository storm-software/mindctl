package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func claudeSettingsPath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

// claudeParkedPath holds the router base URL while Claude Code is turned off.
func claudeParkedPath(home string) string {
	return filepath.Join(home, ".claude", "mindctl-off.json")
}

func claudeState(home string) (harnessState, error) {
	body, err := os.ReadFile(claudeSettingsPath(home))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read Claude settings: %w", err)
	}

	if err == nil {
		if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
			return "", errors.New("read Claude settings: expected a JSON object")
		}

		var settings struct {
			Env struct {
				AnthropicBaseURL string `json:"ANTHROPIC_BASE_URL"`
			} `json:"env"`
		}

		if err := json.Unmarshal(body, &settings); err != nil {
			return "", fmt.Errorf("read Claude settings: %w", err)
		}

		if strings.TrimSuffix(settings.Env.AnthropicBaseURL, "/") == routerOrigin {
			return harnessConnected, nil
		}
	}

	if _, err := os.Stat(claudeParkedPath(home)); err == nil {
		return harnessOff, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read parked Claude settings: %w", err)
	}

	return harnessDisconnected, nil
}

// editClaudeSettings applies edit to Claude Code's global settings, keeping
// every other setting and its order. The file is written only when edit
// reports a change.
func editClaudeSettings(home string, edit func(settings *orderedObject) (bool, error)) error {
	path := claudeSettingsPath(home)
	body, perm, err := readSetupFile(path)
	if err != nil {
		return fmt.Errorf("read Claude settings: %w", err)
	}

	source := body
	if len(bytes.TrimSpace(source)) == 0 {
		source = []byte("{}")
	}

	settings, err := decodeOrderedObject(source)
	if err != nil {
		return fmt.Errorf("read Claude settings: %w", err)
	}

	if changed, err := edit(&settings); err != nil || !changed {
		return err
	}

	encoded, err := settings.marshal()
	if err != nil {
		return err
	}

	var formatted bytes.Buffer
	if err := json.Indent(&formatted, encoded, "", "  "); err != nil {
		return err
	}

	formatted.WriteByte('\n')
	if err := writeSetupFile(path, body, formatted.Bytes(), perm); err != nil {
		return fmt.Errorf("write Claude settings: %w", err)
	}

	return nil
}

// editClaudeEnvObject applies edit to the env object of settings; an env
// object left empty is removed.
func editClaudeEnvObject(settings *orderedObject, edit func(env *orderedObject) bool) (bool, error) {
	env := orderedObject{}
	if raw, exists := settings.get("env"); exists && string(raw) != "null" {
		var err error
		env, err = decodeOrderedObject(raw)
		if err != nil {
			return false, fmt.Errorf("read Claude settings: env: %w", err)
		}
	}

	if !edit(&env) {
		return false, nil
	}

	if len(env.keys) == 0 {
		settings.delete("env")
		return true, nil
	}

	encodedEnv, err := env.marshal()
	if err != nil {
		return false, err
	}

	settings.set("env", encodedEnv)
	return true, nil
}

// editClaudeEnv applies edit to the env object of Claude Code's global
// settings. The file is written only when edit reports a change.
func editClaudeEnv(home string, edit func(env *orderedObject) bool) error {
	return editClaudeSettings(home, func(settings *orderedObject) (bool, error) {
		return editClaudeEnvObject(settings, edit)
	})
}

func claudeBaseURL(env *orderedObject) string {
	var value string
	if raw, exists := env.get("ANTHROPIC_BASE_URL"); exists {
		_ = json.Unmarshal(raw, &value)
	}

	return value
}

func setClaudeRouterURL(env *orderedObject) {
	origin, _ := json.Marshal(routerOrigin)
	env.set("ANTHROPIC_BASE_URL", origin)
}

// setupClaude points Claude Code's global settings at the router and adds
// the Mindctl statusline unless another statusline is configured.
func setupClaude(home string) (actionResult, error) {
	previous, statusline := "", ""
	err := editClaudeSettings(home, func(settings *orderedObject) (bool, error) {
		if _, err := editClaudeEnvObject(settings, func(env *orderedObject) bool {
			previous = claudeBaseURL(env)
			setClaudeRouterURL(env)
			return true
		}); err != nil {
			return false, err
		}

		var err error
		statusline, err = installClaudeStatusline(home, settings)
		return true, err
	})
	if err != nil {
		return actionResult{}, err
	}

	if err := removeIfExists(claudeParkedPath(home)); err != nil {
		return actionResult{}, err
	}

	message := fmt.Sprintf("set env.ANTHROPIC_BASE_URL to %s in %s", routerOrigin, claudeSettingsPath(home))
	if previous != "" && strings.TrimSuffix(previous, "/") != routerOrigin {
		message += fmt.Sprintf(" (was %s)", previous)
	}

	return actionResult{Changed: true, Message: message + "; " + statusline}, nil
}

// offClaude removes the router base URL so Claude Code talks to Anthropic
// directly, and records that it was parked so onClaude can restore it.
func offClaude(home string) (actionResult, error) {
	state, err := claudeState(home)
	if err != nil {
		return actionResult{}, err
	}

	switch state {
	case harnessOff:
		return actionResult{Message: "already off; nothing to do"}, nil
	case harnessDisconnected:
		return actionResult{Message: "not set up for the router; run `mindctl --claude` first"}, nil
	}

	parked, _ := json.Marshal(map[string]map[string]string{"env": {"ANTHROPIC_BASE_URL": routerOrigin}})
	if err := writeSetupFile(claudeParkedPath(home), nil, append(parked, '\n'), 0600); err != nil {
		return actionResult{}, fmt.Errorf("park Claude settings: %w", err)
	}

	err = editClaudeEnv(home, func(env *orderedObject) bool {
		env.delete("ANTHROPIC_BASE_URL")
		return true
	})
	if err != nil {
		return actionResult{}, err
	}

	return actionResult{Changed: true, Message: "off; Claude Code uses Anthropic directly from its next launch"}, nil
}

// onClaude restores the router base URL parked by offClaude.
func onClaude(home string) (actionResult, error) {
	state, err := claudeState(home)
	if err != nil {
		return actionResult{}, err
	}

	switch state {
	case harnessConnected:
		return actionResult{Message: "already on; nothing to do"}, nil
	case harnessDisconnected:
		return actionResult{Message: "not set up for the router; run `mindctl --claude` first"}, nil
	}

	err = editClaudeEnv(home, func(env *orderedObject) bool {
		setClaudeRouterURL(env)
		return true
	})
	if err != nil {
		return actionResult{}, err
	}

	if err := removeIfExists(claudeParkedPath(home)); err != nil {
		return actionResult{}, err
	}

	return actionResult{Changed: true, Message: "on; Claude Code routes through Mindctl from its next launch"}, nil
}

// uninstallClaude removes the router base URL and any parked settings.
func uninstallClaude(home string) (actionResult, error) {
	state, err := claudeState(home)
	if err != nil {
		return actionResult{}, err
	}

	if state == harnessDisconnected {
		return actionResult{Message: "not set up for the router; nothing to remove"}, nil
	}

	err = editClaudeSettings(home, func(settings *orderedObject) (bool, error) {
		envChanged, err := editClaudeEnvObject(settings, func(env *orderedObject) bool {
			if strings.TrimSuffix(claudeBaseURL(env), "/") != routerOrigin {
				return false
			}

			env.delete("ANTHROPIC_BASE_URL")
			return true
		})
		if err != nil {
			return false, err
		}

		statuslineChanged, err := uninstallClaudeStatusline(home, settings)
		return envChanged || statuslineChanged, err
	})
	if err != nil {
		return actionResult{}, err
	}

	if err := removeIfExists(claudeParkedPath(home)); err != nil {
		return actionResult{}, err
	}

	return actionResult{Changed: true, Message: "removed the Mindctl router from " + claudeSettingsPath(home)}, nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}
