package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMain(m *testing.M) {
	cursorProcessRunning = func() bool { return false }
	os.Exit(m.Run())
}

func seedCursorState(t *testing.T, home string) {
	t.Helper()
	path := cursorStatePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", cursorDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	body := `{"composerState":{"x":1},"aiSettings":{"modelOverrideEnabled":["grok-4.5"],"modelConfig":{"composer":{"modelName":"grok-4.7"}}},"useOpenAIKey":false}`
	if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, cursorApplicationUserKey, body); err != nil {
		t.Fatal(err)
	}
}

func TestCursorHarnessSetupOffOnUninstall(t *testing.T) {
	home := setupHome(t)
	seedCursorState(t, home)
	output, err := runSetupCommand(t, "", "--cursor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "OpenAI base URL") || !strings.Contains(output, "Authorization: Bearer") {
		t.Fatalf("setup output:\n%s", output)
	}
	state, err := cursorState(home)
	if err != nil || state != harnessConnected {
		t.Fatalf("state=%s err=%v", state, err)
	}
	blob := readCursorBlob(t, home)
	for _, want := range []string{`"openAIBaseUrl":"http://127.0.0.1:8080/v1"`, `"useOpenAIKey":true`, `"mindctl-auto"`, `"grok-4.5"`, `"modelName":"grok-4.7"`, `"composerState":{"x":1}`} {
		if !strings.Contains(blob, want) {
			t.Fatalf("blob missing %s:\n%s", want, blob)
		}
	}

	if _, err := runSetupCommand(t, "", "off", "--cursor"); err != nil {
		t.Fatal(err)
	}
	state, err = cursorState(home)
	if err != nil || state != harnessOff {
		t.Fatalf("off state=%s err=%v", state, err)
	}
	if _, err := runSetupCommand(t, "", "on", "--cursor"); err != nil {
		t.Fatal(err)
	}
	state, err = cursorState(home)
	if err != nil || state != harnessConnected {
		t.Fatalf("on state=%s err=%v", state, err)
	}
	if _, err := runSetupCommand(t, "", "uninstall", "--cursor"); err != nil {
		t.Fatal(err)
	}
	state, err = cursorState(home)
	if err != nil || state != harnessDisconnected {
		t.Fatalf("uninstalled state=%s err=%v", state, err)
	}
	blob = readCursorBlob(t, home)
	if strings.Contains(blob, "mindctl-auto") || strings.Contains(blob, "openAIBaseUrl") || !strings.Contains(blob, `"grok-4.5"`) {
		t.Fatalf("uninstall blob:\n%s", blob)
	}
}

func TestCursorSetupRefusesWhileCursorIsOpen(t *testing.T) {
	home := setupHome(t)
	seedCursorState(t, home)
	previous := cursorProcessRunning
	cursorProcessRunning = func() bool { return true }
	t.Cleanup(func() { cursorProcessRunning = previous })
	_, err := runSetupCommand(t, "", "--cursor")
	if err == nil || !strings.Contains(err.Error(), "quit Cursor") {
		t.Fatalf("error = %v", err)
	}
}

func readCursorBlob(t *testing.T, home string) string {
	t.Helper()
	db, err := sql.Open("sqlite", cursorDSN(cursorStatePath(home))+"&mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, cursorApplicationUserKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
