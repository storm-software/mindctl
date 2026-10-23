package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	cursorApplicationUserKey = "src.vs.platform.reactivestorage.browser.reactiveStorageServiceImpl.persistentStorage.applicationUser"
	cursorRouterModel        = "mindctl-auto"
)

// cursorBaseURL is the OpenAI-compatible origin Cursor appends /chat/completions to.
var cursorBaseURL = routerOrigin + "/v1"

// cursorProcessRunning reports whether the Cursor app is open. Setup refuses
// to write while it is, because Cursor keeps state.vscdb in memory and
// overwrites the file on the way out.
var cursorProcessRunning = defaultCursorProcessRunning

func cursorUserDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User")
	default:
		configHome := filepath.Join(home, ".config")
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			configHome = xdg
		}
		return filepath.Join(configHome, "Cursor", "User")
	}
}

func cursorStatePath(home string) string {
	return filepath.Join(cursorUserDir(home), "globalStorage", "state.vscdb")
}

func cursorParkedPath(home string) string {
	return filepath.Join(cursorUserDir(home), "mindctl-off.json")
}

func defaultCursorProcessRunning() bool {
	switch runtime.GOOS {
	case "linux":
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return false
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "" || name[0] < '0' || name[0] > '9' {
				continue
			}
			comm, err := os.ReadFile(filepath.Join("/proc", name, "comm"))
			if err != nil {
				continue
			}
			switch strings.TrimSpace(string(comm)) {
			case "cursor", "Cursor":
				return true
			}
		}
		return false
	case "darwin":
		return exec.Command("pgrep", "-x", "Cursor").Run() == nil
	default:
		return false
	}
}

func ensureCursorClosed() error {
	if cursorProcessRunning() {
		return errors.New("quit Cursor before changing its router settings; it rewrites state.vscdb while open")
	}
	return nil
}

func cursorDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "_pragma=busy_timeout(2000)&_txlock=immediate"}).String()
}

type cursorSettings struct {
	baseURL    string
	useKey     bool
	hasBase    bool
	models     []string
	enabled    []string
	hasModels  bool
	hasEnabled bool
}

func readCursorSettings(home string) (cursorSettings, bool, error) {
	path := cursorStatePath(home)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cursorSettings{}, false, nil
	} else if err != nil {
		return cursorSettings{}, false, fmt.Errorf("read Cursor state: %w", err)
	}
	blob, err := loadCursorBlob(path)
	if err != nil {
		return cursorSettings{}, false, err
	}
	settings, err := cursorSettingsFrom(&blob)
	return settings, true, err
}

func loadCursorBlob(path string) (orderedObject, error) {
	db, err := sql.Open("sqlite", cursorDSN(path))
	if err != nil {
		return orderedObject{}, fmt.Errorf("read Cursor state: %w", err)
	}
	defer db.Close()
	var raw []byte
	err = db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, cursorApplicationUserKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || len(bytesTrim(raw)) == 0 {
		return decodeOrderedObject([]byte("{}"))
	}
	if err != nil {
		return orderedObject{}, fmt.Errorf("read Cursor state: %w", err)
	}
	blob, err := decodeOrderedObject(raw)
	if err != nil {
		return orderedObject{}, fmt.Errorf("read Cursor state: %w", err)
	}
	return blob, nil
}

func bytesTrim(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func cursorSettingsFrom(blob *orderedObject) (cursorSettings, error) {
	settings := cursorSettings{}
	if raw, ok := blob.get("openAIBaseUrl"); ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &settings.baseURL); err != nil {
			return cursorSettings{}, fmt.Errorf("read Cursor state: openAIBaseUrl: %w", err)
		}
		settings.hasBase = true
	}
	if raw, ok := blob.get("useOpenAIKey"); ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &settings.useKey); err != nil {
			return cursorSettings{}, fmt.Errorf("read Cursor state: useOpenAIKey: %w", err)
		}
	}
	ai, err := cursorAISettings(blob)
	if err != nil {
		return cursorSettings{}, err
	}
	settings.models, settings.hasModels, err = cursorStringList(&ai, "userAddedModels")
	if err != nil {
		return cursorSettings{}, err
	}
	settings.enabled, settings.hasEnabled, err = cursorStringList(&ai, "modelOverrideEnabled")
	if err != nil {
		return cursorSettings{}, err
	}
	return settings, nil
}

func cursorAISettings(blob *orderedObject) (orderedObject, error) {
	raw, ok := blob.get("aiSettings")
	if !ok || string(raw) == "null" {
		return orderedObject{}, nil
	}
	ai, err := decodeOrderedObject(raw)
	if err != nil {
		return orderedObject{}, fmt.Errorf("read Cursor state: aiSettings: %w", err)
	}
	return ai, nil
}

func cursorStringList(ai *orderedObject, key string) ([]string, bool, error) {
	raw, ok := ai.get(key)
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, fmt.Errorf("read Cursor state: aiSettings.%s: %w", key, err)
	}
	return values, true, nil
}

func cursorRoutesHere(settings cursorSettings) bool {
	return settings.useKey && strings.TrimSuffix(settings.baseURL, "/") == cursorBaseURL
}

func cursorState(home string) (harnessState, error) {
	settings, exists, err := readCursorSettings(home)
	if err != nil {
		return "", err
	}
	if exists && cursorRoutesHere(settings) {
		return harnessConnected, nil
	}
	if _, err := os.Stat(cursorParkedPath(home)); err == nil {
		return harnessOff, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read parked Cursor settings: %w", err)
	}
	return harnessDisconnected, nil
}

func editCursorBlob(home string, edit func(*orderedObject) (bool, error)) (bool, error) {
	if err := ensureCursorClosed(); err != nil {
		return false, err
	}
	path := cursorStatePath(home)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("Cursor state database not found at %s; open Cursor once, quit it, and run this again", path)
	} else if err != nil {
		return false, fmt.Errorf("read Cursor state: %w", err)
	}
	db, err := sql.Open("sqlite", cursorDSN(path))
	if err != nil {
		return false, fmt.Errorf("read Cursor state: %w", err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return false, fmt.Errorf("read Cursor state: %w", err)
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, cursorApplicationUserKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || len(bytesTrim(raw)) == 0 {
		raw = []byte("{}")
		err = nil
	}
	if err != nil {
		return false, fmt.Errorf("read Cursor state: %w", err)
	}
	blob, err := decodeOrderedObject(raw)
	if err != nil {
		return false, fmt.Errorf("read Cursor state: %w", err)
	}
	changed, err := edit(&blob)
	if err != nil || !changed {
		return false, err
	}
	encoded, err := blob.marshal()
	if err != nil {
		return false, err
	}
	result, err := tx.Exec(`UPDATE ItemTable SET value = ? WHERE key = ?`, encoded, cursorApplicationUserKey)
	if err != nil {
		return false, fmt.Errorf("write Cursor state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("write Cursor state: %w", err)
	}
	if rows == 0 {
		if _, err := tx.Exec(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, cursorApplicationUserKey, encoded); err != nil {
			return false, fmt.Errorf("write Cursor state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("write Cursor state: %w", err)
	}
	return true, nil
}

func setCursorRouter(blob *orderedObject) (bool, string, error) {
	settings, err := cursorSettingsFrom(blob)
	if err != nil {
		return false, "", err
	}
	previous := settings.baseURL
	changed := settings.baseURL != cursorBaseURL || !settings.useKey
	base, err := json.Marshal(cursorBaseURL)
	if err != nil {
		return false, "", err
	}
	blob.set("openAIBaseUrl", base)
	blob.set("useOpenAIKey", []byte("true"))
	modelChanged, err := ensureCursorModel(blob)
	if err != nil {
		return false, "", err
	}
	return changed || modelChanged, previous, nil
}

func ensureCursorModel(blob *orderedObject) (bool, error) {
	ai, err := cursorAISettings(blob)
	if err != nil {
		return false, err
	}
	modelsChanged, err := appendCursorModel(&ai, "userAddedModels")
	if err != nil {
		return false, err
	}
	enabledChanged, err := appendCursorModel(&ai, "modelOverrideEnabled")
	if err != nil {
		return false, err
	}
	if !modelsChanged && !enabledChanged {
		return false, nil
	}
	encoded, err := ai.marshal()
	if err != nil {
		return false, err
	}
	blob.set("aiSettings", encoded)
	return true, nil
}

func appendCursorModel(ai *orderedObject, key string) (bool, error) {
	values, _, err := cursorStringList(ai, key)
	if err != nil {
		return false, err
	}
	if slices.Contains(values, cursorRouterModel) {
		return false, nil
	}
	values = append(values, cursorRouterModel)
	encoded, err := json.Marshal(values)
	if err != nil {
		return false, err
	}
	ai.set(key, encoded)
	return true, nil
}

func clearCursorRouter(blob *orderedObject) (bool, error) {
	settings, err := cursorSettingsFrom(blob)
	if err != nil {
		return false, err
	}
	changed := false
	if settings.hasBase && strings.TrimSuffix(settings.baseURL, "/") == cursorBaseURL {
		blob.delete("openAIBaseUrl")
		changed = true
	}
	if settings.useKey {
		blob.set("useOpenAIKey", []byte("false"))
		changed = true
	}
	return changed, nil
}

func removeCursorModel(blob *orderedObject) (bool, error) {
	ai, err := cursorAISettings(blob)
	if err != nil {
		return false, err
	}
	removedModels, err := dropCursorModel(&ai, "userAddedModels")
	if err != nil {
		return false, err
	}
	removedEnabled, err := dropCursorModel(&ai, "modelOverrideEnabled")
	if err != nil {
		return false, err
	}
	if !removedModels && !removedEnabled {
		return false, nil
	}
	encoded, err := ai.marshal()
	if err != nil {
		return false, err
	}
	blob.set("aiSettings", encoded)
	return true, nil
}

func dropCursorModel(ai *orderedObject, key string) (bool, error) {
	values, exists, err := cursorStringList(ai, key)
	if err != nil || !exists || !slices.Contains(values, cursorRouterModel) {
		return false, err
	}
	values = slices.DeleteFunc(values, func(value string) bool { return value == cursorRouterModel })
	if len(values) == 0 {
		ai.delete(key)
		return true, nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return false, err
	}
	ai.set(key, encoded)
	return true, nil
}

func setupCursor(home string) (actionResult, error) {
	var previous string
	changed, err := editCursorBlob(home, func(blob *orderedObject) (bool, error) {
		var edited bool
		var editErr error
		edited, previous, editErr = setCursorRouter(blob)
		return edited, editErr
	})
	if err != nil {
		return actionResult{}, err
	}
	if err := removeIfExists(cursorParkedPath(home)); err != nil {
		return actionResult{}, err
	}
	if !changed {
		return actionResult{
			Message: "already routes through Mindctl; paste the gateway token into Cursor's OpenAI API key if requests fail",
		}, nil
	}
	message := fmt.Sprintf("set the OpenAI base URL to %s in %s", cursorBaseURL, cursorStatePath(home))
	if previous != "" && strings.TrimSuffix(previous, "/") != cursorBaseURL {
		message += fmt.Sprintf(" (was %s)", previous)
	}
	return actionResult{
		Changed: true,
		Message: message + "; added the mindctl-auto model. Paste the gateway token into Cursor's OpenAI API key, then restart Cursor",
	}, nil
}

func offCursor(home string) (actionResult, error) {
	state, err := cursorState(home)
	if err != nil {
		return actionResult{}, err
	}
	switch state {
	case harnessOff:
		return actionResult{Message: "already off; nothing to do"}, nil
	case harnessDisconnected:
		return actionResult{Message: "not set up for the router; run `mindctl --cursor` first"}, nil
	}
	if err := ensureCursorClosed(); err != nil {
		return actionResult{}, err
	}
	parked, err := json.Marshal(map[string]any{"openAIBaseUrl": cursorBaseURL, "useOpenAIKey": true})
	if err != nil {
		return actionResult{}, err
	}
	if err := writeSetupFile(cursorParkedPath(home), nil, append(parked, '\n'), 0600); err != nil {
		return actionResult{}, fmt.Errorf("park Cursor settings: %w", err)
	}
	if _, err := editCursorBlob(home, clearCursorRouter); err != nil {
		return actionResult{}, err
	}
	return actionResult{Changed: true, Message: "off; Cursor uses its own models from its next launch"}, nil
}

func onCursor(home string) (actionResult, error) {
	state, err := cursorState(home)
	if err != nil {
		return actionResult{}, err
	}
	switch state {
	case harnessConnected:
		return actionResult{Message: "already on; nothing to do"}, nil
	case harnessDisconnected:
		return actionResult{Message: "not set up for the router; run `mindctl --cursor` first"}, nil
	}
	if _, err := editCursorBlob(home, func(blob *orderedObject) (bool, error) {
		changed, _, err := setCursorRouter(blob)
		return changed, err
	}); err != nil {
		return actionResult{}, err
	}
	if err := removeIfExists(cursorParkedPath(home)); err != nil {
		return actionResult{}, err
	}
	return actionResult{Changed: true, Message: "on; Cursor routes through Mindctl from its next launch"}, nil
}

func uninstallCursor(home string) (actionResult, error) {
	state, err := cursorState(home)
	if err != nil {
		return actionResult{}, err
	}
	if state == harnessDisconnected {
		return actionResult{Message: "not set up for the router; nothing to remove"}, nil
	}
	if _, err := editCursorBlob(home, func(blob *orderedObject) (bool, error) {
		cleared, err := clearCursorRouter(blob)
		if err != nil {
			return false, err
		}
		removed, err := removeCursorModel(blob)
		return cleared || removed, err
	}); err != nil {
		return actionResult{}, err
	}
	if err := removeIfExists(cursorParkedPath(home)); err != nil {
		return actionResult{}, err
	}
	return actionResult{Changed: true, Message: "removed the OpenAI base URL and the mindctl-auto model"}, nil
}
