package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runSetupCommand(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	command := newRootCommand(context.Background(), &stdout, io.Discard)
	command.SetIn(strings.NewReader(stdin))
	command.SetArgs(args)
	err := command.ExecuteContext(context.Background())
	return stdout.String(), err
}

func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "missing-gateway-config"))
	return home
}

func writeHomeFile(t *testing.T, home, name, body string) {
	t.Helper()
	path := filepath.Join(home, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func readHomeFile(t *testing.T, home, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func harnessStates(t *testing.T, home string) (claude, codex harnessState) {
	t.Helper()
	claude, err := claudeState(home)
	if err != nil {
		t.Fatal(err)
	}
	codex, err = codexState(home)
	if err != nil {
		t.Fatal(err)
	}
	return claude, codex
}

func connectedHarnesses(t *testing.T, home string) (claude, codex bool) {
	t.Helper()
	claudeState, codexState := harnessStates(t, home)
	return claudeState == harnessConnected, codexState == harnessConnected
}

func TestSetupFlagsConfigureOnlyRequestedHarnesses(t *testing.T) {
	for _, test := range []struct {
		name       string
		args       []string
		wantClaude bool
		wantCodex  bool
	}{
		{name: "claude", args: []string{"--claude"}, wantClaude: true},
		{name: "codex", args: []string{"--codex"}, wantCodex: true},
		{name: "both", args: []string{"--codex", "--claude"}, wantClaude: true, wantCodex: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := setupHome(t)
			output, err := runSetupCommand(t, "", test.args...)
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			if strings.Contains(output, "Select the harnesses") {
				t.Fatalf("harness flags still showed the menu:\n%s", output)
			}

			claude, codex := connectedHarnesses(t, home)
			if claude != test.wantClaude || codex != test.wantCodex {
				t.Fatalf("claude=%v codex=%v; want claude=%v codex=%v", claude, codex, test.wantClaude, test.wantCodex)
			}
		})
	}
}

func TestSetupMenuSelectsHarnesses(t *testing.T) {
	for _, test := range []struct {
		name       string
		stdin      string
		wantClaude bool
		wantCodex  bool
		wantErr    string
	}{
		{name: "single number", stdin: "1\n", wantClaude: true},
		{name: "several numbers", stdin: "2, 1\n", wantClaude: true, wantCodex: true},
		{name: "names", stdin: "codex\n", wantCodex: true},
		{name: "all", stdin: "all\n", wantClaude: true, wantCodex: true},
		{name: "retry after invalid", stdin: "9\n2\n", wantCodex: true},
		{name: "no newline", stdin: "1", wantClaude: true},
		{name: "no selection", stdin: "", wantErr: "no harness selected"},
		{name: "only invalid", stdin: "nope\n", wantErr: "no harness selected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := setupHome(t)
			output, err := runSetupCommand(t, test.stdin)
			if !strings.Contains(output, "1) Claude Code") || !strings.Contains(output, "2) Codex") {
				t.Fatalf("menu missing harnesses:\n%s", output)
			}
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("setup error = %v; want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("setup: %v", err)
			}

			claude, codex := connectedHarnesses(t, home)
			if claude != test.wantClaude || codex != test.wantCodex {
				t.Fatalf("claude=%v codex=%v; want claude=%v codex=%v", claude, codex, test.wantClaude, test.wantCodex)
			}
		})
	}
}

func TestSetupClaudePreservesSettings(t *testing.T) {
	home := setupHome(t)
	original := `{"model":"opus","env":{"FOO":"bar","ANTHROPIC_BASE_URL":"https://other.example"},"hooks":{}}`
	writeHomeFile(t, home, ".claude/settings.json", original)

	if _, err := runSetupCommand(t, "", "--claude"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	want := "{\n  \"model\": \"opus\",\n  \"env\": {\n    \"FOO\": \"bar\",\n    \"ANTHROPIC_BASE_URL\": \"http://127.0.0.1:8080\"\n  },\n  \"hooks\": {}\n}\n"
	if got := readHomeFile(t, home, ".claude/settings.json"); got != want {
		t.Fatalf("settings=%q\nwant=%q", got, want)
	}
	if got := readHomeFile(t, home, ".claude/settings.json.mindctl-backup"); got != original {
		t.Fatalf("backup=%q want=%q", got, original)
	}
}

func TestSetupClaudeRejectsMalformedSettings(t *testing.T) {
	home := setupHome(t)
	writeHomeFile(t, home, ".claude/settings.json", `[]`)
	if _, err := runSetupCommand(t, "", "--claude"); err == nil || !strings.Contains(err.Error(), "read Claude settings") {
		t.Fatalf("setup error = %v", err)
	}
	if got := readHomeFile(t, home, ".claude/settings.json"); got != `[]` {
		t.Fatalf("malformed settings were rewritten: %q", got)
	}
}

func TestSetupCodexEditsConfigInPlace(t *testing.T) {
	home := setupHome(t)
	original := "# my settings\nmodel = \"gpt-5\"\nmodel_provider = \"openai\"\n\n[profiles.fast]\nmodel = \"other\"\n"
	writeHomeFile(t, home, ".codex/config.toml", original)

	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := readHomeFile(t, home, ".codex/config.toml")
	want := "# my settings\nmodel = \"mindctl-auto\"\nmodel_provider = \"mindctl\"\n\n[profiles.fast]\nmodel = \"other\"\n\n" + codexProviderBlock
	if got != want {
		t.Fatalf("config=%q\nwant=%q", got, want)
	}

	// Running setup again leaves the configuration unchanged.
	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if again := readHomeFile(t, home, ".codex/config.toml"); again != want {
		t.Fatalf("second setup changed config: %q", again)
	}
}

func TestSetupCodexKeepsCustomProvider(t *testing.T) {
	home := setupHome(t)
	custom := "[model_providers.mindctl]\nname = \"Mindctl\"\nbase_url = \"http://127.0.0.1:8080/v1\"\nwire_api = \"responses\"\n"
	writeHomeFile(t, home, ".codex/config.toml", custom)

	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	want := "model_provider = \"mindctl\"\nmodel = \"mindctl-auto\"\n" + custom
	if got := readHomeFile(t, home, ".codex/config.toml"); got != want {
		t.Fatalf("config=%q\nwant=%q", got, want)
	}
}

func TestSetupCodexRejectsInvalidConfig(t *testing.T) {
	home := setupHome(t)
	writeHomeFile(t, home, ".codex/config.toml", "model = \"broken\n")
	if _, err := runSetupCommand(t, "", "--codex"); err == nil || !strings.Contains(err.Error(), "read Codex config") {
		t.Fatalf("setup error = %v", err)
	}
}

func TestOffOnUninstallRoundTrip(t *testing.T) {
	for _, form := range []struct {
		name string
		verb func(verb string) []string
	}{
		{name: "subcommand", verb: func(verb string) []string { return []string{verb} }},
		{name: "flag", verb: func(verb string) []string { return []string{"--" + verb} }},
	} {
		t.Run(form.name, func(t *testing.T) {
			home := setupHome(t)
			claudeOriginal := `{"model":"opus","env":{"FOO":"bar"}}`
			codexOriginal := "# my settings\nmodel = \"gpt-5\"\n\n[profiles.fast]\nmodel = \"other\"\n"
			writeHomeFile(t, home, ".claude/settings.json", claudeOriginal)
			writeHomeFile(t, home, ".codex/config.toml", codexOriginal)
			both := func(verb string) []string { return append(form.verb(verb), "--claude", "--codex") }

			if _, err := runSetupCommand(t, "", "--claude", "--codex"); err != nil {
				t.Fatalf("setup: %v", err)
			}
			routedClaude := readHomeFile(t, home, ".claude/settings.json")
			routedCodex := readHomeFile(t, home, ".codex/config.toml")

			if _, err := runSetupCommand(t, "", both("off")...); err != nil {
				t.Fatalf("off: %v", err)
			}
			if claude, codex := harnessStates(t, home); claude != harnessOff || codex != harnessOff {
				t.Fatalf("after off: claude=%s codex=%s", claude, codex)
			}
			if got := readHomeFile(t, home, ".claude/settings.json"); strings.Contains(got, "ANTHROPIC_BASE_URL") || !strings.Contains(got, `"FOO": "bar"`) {
				t.Fatalf("Claude settings after off: %s", got)
			}
			if got := readHomeFile(t, home, ".codex/config.toml"); !strings.Contains(got, "[model_providers.mindctl]") {
				t.Fatalf("off removed the Codex provider: %s", got)
			}

			output, err := runSetupCommand(t, "", both("off")...)
			if err != nil || strings.Count(output, "already off") != 2 {
				t.Fatalf("second off: err=%v output=%s", err, output)
			}

			if _, err := runSetupCommand(t, "", both("on")...); err != nil {
				t.Fatalf("on: %v", err)
			}
			if got := readHomeFile(t, home, ".claude/settings.json"); got != routedClaude {
				t.Fatalf("Claude settings after on=%q\nwant=%q", got, routedClaude)
			}
			if got := readHomeFile(t, home, ".codex/config.toml"); got != routedCodex {
				t.Fatalf("Codex config after on=%q\nwant=%q", got, routedCodex)
			}

			if _, err := runSetupCommand(t, "", both("uninstall")...); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			if claude, codex := harnessStates(t, home); claude != harnessDisconnected || codex != harnessDisconnected {
				t.Fatalf("after uninstall: claude=%s codex=%s", claude, codex)
			}
			if got, want := readHomeFile(t, home, ".claude/settings.json"), "{\n  \"model\": \"opus\",\n  \"env\": {\n    \"FOO\": \"bar\"\n  }\n}\n"; got != want {
				t.Fatalf("Claude settings after uninstall=%q\nwant=%q", got, want)
			}
			if got, want := readHomeFile(t, home, ".codex/config.toml"), "# my settings\n\n[profiles.fast]\nmodel = \"other\"\n"; got != want {
				t.Fatalf("Codex config after uninstall=%q\nwant=%q", got, want)
			}
		})
	}
}

func TestUninstallRemovesTurnedOffSettings(t *testing.T) {
	home := setupHome(t)
	if _, err := runSetupCommand(t, "", "--claude", "--codex"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := runSetupCommand(t, "", "off", "--claude", "--codex"); err != nil {
		t.Fatalf("off: %v", err)
	}
	if _, err := runSetupCommand(t, "", "uninstall", "--claude", "--codex"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if claude, codex := harnessStates(t, home); claude != harnessDisconnected || codex != harnessDisconnected {
		t.Fatalf("after uninstall: claude=%s codex=%s", claude, codex)
	}
	if got := readHomeFile(t, home, ".codex/config.toml"); got != "" {
		t.Fatalf("Codex config after uninstall=%q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "mindctl-off.json")); !os.IsNotExist(err) {
		t.Fatalf("parked Claude settings remain: %v", err)
	}
}

func TestOnKeepsCodexModelChosenWhileOff(t *testing.T) {
	home := setupHome(t)
	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := runSetupCommand(t, "", "off", "--codex"); err != nil {
		t.Fatalf("off: %v", err)
	}
	config := readHomeFile(t, home, ".codex/config.toml")
	writeHomeFile(t, home, ".codex/config.toml", "model = \"gpt-6-sol\"\n"+config)
	if _, err := runSetupCommand(t, "", "on", "--codex"); err != nil {
		t.Fatalf("on: %v", err)
	}
	got := readHomeFile(t, home, ".codex/config.toml")
	if !strings.HasPrefix(got, "model = \"gpt-6-sol\"\nmodel_provider = \"mindctl\"\n") || strings.Contains(got, "mindctl-auto") {
		t.Fatalf("Codex config after on=%q", got)
	}
}

func TestActionsReportUnconfiguredHarnesses(t *testing.T) {
	for _, verb := range []string{"off", "on", "uninstall"} {
		t.Run(verb, func(t *testing.T) {
			home := setupHome(t)
			output, err := runSetupCommand(t, "", verb, "--claude", "--codex")
			if err != nil {
				t.Fatalf("%s: %v", verb, err)
			}
			if strings.Count(output, "not set up for the router") != 2 || strings.Contains(output, "✓") {
				t.Fatalf("%s output:\n%s", verb, output)
			}
			for _, name := range []string{".claude", ".codex"} {
				if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
					t.Fatalf("%s created %s: %v", verb, name, err)
				}
			}
		})
	}
}

func TestActionMenuAndConflictingFlags(t *testing.T) {
	home := setupHome(t)
	if _, err := runSetupCommand(t, "", "--claude"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	output, err := runSetupCommand(t, "1\n", "off")
	if err != nil {
		t.Fatalf("off menu: %v", err)
	}
	if !strings.Contains(output, "Select the harnesses to turn off:") || !strings.Contains(output, "Claude Code  — patches ~/.claude/settings.json (connected)") {
		t.Fatalf("off menu output:\n%s", output)
	}
	if claude, _ := harnessStates(t, home); claude != harnessOff {
		t.Fatalf("claude=%s after off menu", claude)
	}

	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"status", "claude"}, &stdout, io.Discard); err != nil || stdout.String() != "off\n" {
		t.Fatalf("status claude = %q, %v", stdout.String(), err)
	}

	if _, err := runSetupCommand(t, "", "--off", "--on", "--claude"); err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Fatalf("conflicting flags error = %v", err)
	}
}
