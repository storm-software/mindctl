package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
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

// readCodexConfigWithoutHooks returns Codex's config.toml without the hook
// registrations and hooks feature line that setup manages.
func readCodexConfigWithoutHooks(t *testing.T, home string) string {
	t.Helper()
	document, err := readCodexDocument(home)
	if err != nil {
		t.Fatal(err)
	}
	document.removeManagedHooks(home)
	document.removeHooksFeature()
	return strings.Join(document.lines, "\n") + "\n"
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

	statusline := filepath.Join(home, ".mindctl", "cc-statusline.sh")
	want := "{\n  \"model\": \"opus\",\n  \"env\": {\n    \"FOO\": \"bar\",\n    \"ANTHROPIC_BASE_URL\": \"http://127.0.0.1:8080\"\n  },\n  \"hooks\": {},\n  \"statusLine\": {\n    \"type\": \"command\",\n    \"command\": \"" + statusline + "\"\n  }\n}\n"
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

	got := readCodexConfigWithoutHooks(t, home)
	want := "# my settings\nmodel = \"mindctl-auto\"\nmodel_provider = \"mindctl\"\n\n[profiles.fast]\nmodel = \"other\"\n\n" + codexProviderBlock
	if got != want {
		t.Fatalf("config=%q\nwant=%q", got, want)
	}

	// Running setup again leaves the configuration unchanged.
	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if again := readCodexConfigWithoutHooks(t, home); again != want {
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
	if got := readCodexConfigWithoutHooks(t, home); got != want {
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
	got := readCodexConfigWithoutHooks(t, home)
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
	if !strings.Contains(output, "Select the harnesses to turn off:") || !strings.Contains(output, "Claude Code  — patches ~/.claude/settings.json and adds a statusline (connected)") {
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

func TestSetupClaudeInstallsStatusline(t *testing.T) {
	home := setupHome(t)
	configHome := filepath.Join(home, "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeHomeFile(t, home, "config/mindctl/config.yaml", `
savings:
  baseline_model: big
models:
  - id: big
    provider: anthropic
    tier: T5
    available: true
    input_price: 5
    output_price: 25
  - id: small
    provider: openai
    tier: T2
    available: true
    input_price: 1
    cached_input_price_usd_per_million: 0.1
    output_price: 4
`)

	output, err := runSetupCommand(t, "", "--claude")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(output, "added the statusline at") {
		t.Fatalf("setup output:\n%s", output)
	}

	path := filepath.Join(home, ".mindctl", "cc-statusline.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0100 == 0 {
		t.Fatalf("statusline mode = %v; want executable", info.Mode())
	}

	script := readHomeFile(t, home, ".mindctl/cc-statusline.sh")
	if !strings.Contains(script, claudeStatuslineMarker) || strings.Contains(script, claudeStatuslinePricingPlaceholder) {
		t.Fatalf("statusline script missing marker or unrendered prices")
	}
	for _, want := range []string{`"baseline_model":"big"`, `"small":{"input":1,"output":4,"cache_read":0.1,"cache_write":1.25}`} {
		if !strings.Contains(script, want) {
			t.Fatalf("statusline script does not contain %s", want)
		}
	}

	output, err = runSetupCommand(t, "", "--claude")
	if err != nil || !strings.Contains(output, "refreshed the statusline at") {
		t.Fatalf("second setup = %q, %v", output, err)
	}
	if _, err := os.Stat(path + ".mindctl-backup"); !os.IsNotExist(err) {
		t.Fatalf("statusline backup created: %v", err)
	}
}

func TestSetupClaudeKeepsUserStatusline(t *testing.T) {
	home := setupHome(t)
	original := `{"statusLine":{"type":"command","command":"~/my-status.sh"}}`
	writeHomeFile(t, home, ".claude/settings.json", original)

	output, err := runSetupCommand(t, "", "--claude")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(output, "kept the existing Claude Code statusLine") {
		t.Fatalf("setup output:\n%s", output)
	}
	if !strings.Contains(readHomeFile(t, home, ".claude/settings.json"), `"command": "~/my-status.sh"`) {
		t.Fatal("user statusLine was replaced")
	}
	if _, err := os.Stat(filepath.Join(home, ".mindctl", "cc-statusline.sh")); !os.IsNotExist(err) {
		t.Fatalf("statusline script written: %v", err)
	}

	if _, err := runSetupCommand(t, "", "uninstall", "--claude"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !strings.Contains(readHomeFile(t, home, ".claude/settings.json"), `"command": "~/my-status.sh"`) {
		t.Fatal("uninstall removed the user statusLine")
	}
}

func TestSetupClaudeKeepsUserScriptAtStatuslinePath(t *testing.T) {
	home := setupHome(t)
	writeHomeFile(t, home, ".mindctl/cc-statusline.sh", "#!/bin/sh\necho mine\n")

	output, err := runSetupCommand(t, "", "--claude")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(output, "because mindctl did not write it") {
		t.Fatalf("setup output:\n%s", output)
	}
	if got := readHomeFile(t, home, ".mindctl/cc-statusline.sh"); got != "#!/bin/sh\necho mine\n" {
		t.Fatalf("user script replaced: %q", got)
	}
	if strings.Contains(readHomeFile(t, home, ".claude/settings.json"), "statusLine") {
		t.Fatal("statusLine configured for a user-owned script")
	}
}

func TestUninstallClaudeRemovesStatusline(t *testing.T) {
	home := setupHome(t)
	writeHomeFile(t, home, ".claude/settings.json", `{"model":"opus"}`)
	if _, err := runSetupCommand(t, "", "--claude"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := runSetupCommand(t, "", "off", "--claude"); err != nil {
		t.Fatalf("off: %v", err)
	}
	if !strings.Contains(readHomeFile(t, home, ".claude/settings.json"), "statusLine") {
		t.Fatal("off removed the statusline")
	}

	if _, err := runSetupCommand(t, "", "uninstall", "--claude"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := readHomeFile(t, home, ".claude/settings.json"); got != "{\n  \"model\": \"opus\"\n}\n" {
		t.Fatalf("settings after uninstall = %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".mindctl", "cc-statusline.sh")); !os.IsNotExist(err) {
		t.Fatalf("statusline script remains: %v", err)
	}
}

func TestClaudeStatuslineRendersSavings(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	dir := t.TempDir()
	script, err := renderClaudeStatusline(statuslinePricing{
		BaselineModel: "big",
		Models: map[string]statuslineModelPrice{
			"big":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
			"small": {Input: 1, Output: 4, CacheRead: 0.1, CacheWrite: 1.25},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "cc-statusline.sh")
	if err := os.WriteFile(scriptPath, script, 0755); err != nil {
		t.Fatal(err)
	}

	// Two content blocks of one turn share usage and must be counted once.
	turn := `{"type":"assistant","message":{"id":"msg_1","model":"small","usage":{"input_tokens":100000,"output_tokens":10000}}}`
	transcript := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(turn+"\n"+turn+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("bash", scriptPath)
	command.Stdin = strings.NewReader(`{"transcript_path":"` + transcript + `","model":{"id":"big[1m]"}}`)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("statusline: %v", err)
	}

	// big: 0.5 + 0.25 = 0.75; small: 0.1 + 0.04 = 0.14; saved 0.61.
	want := "\033[38;2;99;102;241mMINDCTL\033[0m — small ← big[1m] · saved $0.61 · 100.0k in / 10.0k out"
	if string(output) != want {
		t.Fatalf("statusline = %q\nwant %q", output, want)
	}
}

func TestSetupCodexInstallsHooksAndSkills(t *testing.T) {
	home := setupHome(t)
	original := "[features]\nweb_search = true\n\n[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"~/mine.sh\"\n"
	writeHomeFile(t, home, ".codex/config.toml", original)

	output, err := runSetupCommand(t, "", "--codex")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, want := range []string{"registered hooks", "installed skills $mindctl-on, $mindctl-off, $mindctl-status"} {
		if !strings.Contains(output, want) {
			t.Fatalf("setup output missing %q:\n%s", want, output)
		}
	}

	statusPath := filepath.Join(home, ".mindctl", "codex-status.sh")
	directivePath := filepath.Join(home, ".mindctl", "codex-directive.sh")
	for _, path := range []string{statusPath, directivePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("%s mode = %v; want 0700", path, info.Mode())
		}
	}
	if strings.Contains(readHomeFile(t, home, ".mindctl/codex-directive.sh"), "__MINDCTL_BIN__") {
		t.Fatal("directive hook has an unrendered mindctl command")
	}
	for _, skill := range codexSkills {
		body := readHomeFile(t, home, filepath.Join(".codex/skills", skill.Name, "SKILL.md"))
		if !strings.Contains(body, "name: "+skill.Name) || !strings.Contains(body, " "+skill.Args+"\n") {
			t.Fatalf("%s skill:\n%s", skill.Name, body)
		}
	}

	config := readHomeFile(t, home, ".codex/config.toml")
	settings, err := parseCodexConfig([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	if !settings.GetBool("features.hooks") || !settings.GetBool("features.web_search") {
		t.Fatalf("features not enabled:\n%s", config)
	}
	for _, want := range []string{
		"[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = \"" + statusPath + "\"",
		"[[hooks.UserPromptSubmit.hooks]]\ntype = \"command\"\ncommand = \"" + directivePath + "\"",
		"command = \"~/mine.sh\"",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("config missing %q:\n%s", want, config)
		}
	}

	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if again := readHomeFile(t, home, ".codex/config.toml"); again != config {
		t.Fatalf("second setup changed config:\n%s", again)
	}

	if _, err := runSetupCommand(t, "", "uninstall", "--codex"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := readHomeFile(t, home, ".codex/config.toml"); got != original {
		t.Fatalf("config after uninstall = %q", got)
	}
	for _, path := range []string{statusPath, directivePath, filepath.Join(home, ".codex", "skills", "mindctl-on")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s remains after uninstall: %v", path, err)
		}
	}
}

func TestUninstallCodexFindsHooksWithoutMarkers(t *testing.T) {
	home := setupHome(t)
	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Codex drops comments when it rewrites config.toml.
	config := readHomeFile(t, home, ".codex/config.toml")
	var kept []string
	for _, line := range strings.Split(config, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, strings.TrimSuffix(line, "  "+codexHooksFeatureMarker))
		}
	}
	writeHomeFile(t, home, ".codex/config.toml", strings.Join(kept, "\n"))

	if _, err := runSetupCommand(t, "", "uninstall", "--codex"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := readHomeFile(t, home, ".codex/config.toml"); strings.Contains(got, "hooks.") || strings.Contains(got, "mindctl") {
		t.Fatalf("config after uninstall:\n%s", got)
	}
}

func TestSetupCodexSkipsHooksItCannotExtend(t *testing.T) {
	for name, test := range map[string]struct{ config, file string }{
		"hooks table":      {config: "[hooks]\nfoo = 1\n"},
		"user status hook": {file: ".mindctl/codex-status.sh"},
		"inline features":  {config: "features = { web_search = true }\n"},
	} {
		t.Run(name, func(t *testing.T) {
			home := setupHome(t)
			if test.config != "" {
				writeHomeFile(t, home, ".codex/config.toml", test.config)
			}
			if test.file != "" {
				writeHomeFile(t, home, test.file, "#!/bin/sh\n")
			}

			output, err := runSetupCommand(t, "", "--codex")
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			if strings.Contains(output, "registered hooks") || strings.Contains(readHomeFile(t, home, ".codex/config.toml"), "[[hooks.") {
				t.Fatalf("hooks registered:\n%s", output)
			}
			if codex, _ := codexState(home); codex != harnessConnected {
				t.Fatalf("codex = %s; want connected", codex)
			}
		})
	}
}

func TestCodexStatusHookSetsTitle(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	home := setupHome(t)
	if _, err := runSetupCommand(t, "", "--codex"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	title := filepath.Join(home, "title")
	runHook := func(payload string) string {
		t.Helper()
		command := exec.Command("bash", filepath.Join(home, ".mindctl", "codex-status.sh"))
		command.Env = append(os.Environ(), "HOME="+home, "MINDCTL_CODEX_STATUS_TITLE_FILE="+title)
		command.Stdin = strings.NewReader(payload)
		if output, err := command.Output(); err != nil || len(output) != 0 {
			t.Fatalf("status hook stdout=%q err=%v", output, err)
		}
		return readHomeFile(t, home, "title")
	}

	if got := runHook(`{"hook_event_name":"SessionStart","model":"mindctl-auto"}`); got != "Mindctl · auto routing\n" {
		t.Fatalf("auto title = %q", got)
	}
	if got := runHook(`{"hook_event_name":"Stop","model":"gpt-6-sol"}`); got != "Mindctl · gpt-6-sol\n" {
		t.Fatalf("explicit title = %q", got)
	}

	if _, err := runSetupCommand(t, "", "off", "--codex"); err != nil {
		t.Fatalf("off: %v", err)
	}
	if got := runHook(`{"hook_event_name":"SessionStart"}`); got != "Codex · direct\n" {
		t.Fatalf("off title = %q", got)
	}
}

func TestCodexDirectiveHookRunsToggles(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "fake mindctl")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"ran $*\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "codex-directive.sh")
	body := strings.Replace(codexDirectiveScript, "'__MINDCTL_BIN__'", shellQuote(fake), 1)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}

	runHook := func(prompt string) map[string]any {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "prompt": prompt})
		command := exec.Command("bash", script)
		command.Stdin = bytes.NewReader(payload)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("directive hook: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("directive output %q: %v", output, err)
		}
		return result
	}

	for prompt, want := range map[string]string{
		"$mindctl-off\n":  "ran off --codex",
		"$mindctl-on":     "ran on --codex",
		"$mindctl-status": "ran status codex",
	} {
		result := runHook(prompt)
		reason, _ := result["reason"].(string)
		if result["decision"] != "block" || !strings.HasPrefix(reason, want) {
			t.Fatalf("%q => %v", prompt, result)
		}
	}

	for _, prompt := range []string{"please run $mindctl-off", "$mindctl-off now", "$router-off"} {
		if result := runHook(prompt); result["continue"] != true {
			t.Fatalf("%q => %v; want pass-through", prompt, result)
		}
	}
}
