package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

//go:embed codex-status.sh
var codexStatusScript string

//go:embed codex-directive.sh
var codexDirectiveScript string

const (
	codexStatusMarker    = "<!-- mindctl managed codex status -->"
	codexDirectiveMarker = "<!-- mindctl managed codex directive -->"
	codexHooksBegin      = "# >>> mindctl managed hooks >>>"
	codexHooksEnd        = "# <<< mindctl managed hooks <<<"
	// codexHooksFeatureMarker ends the features.hooks line setup added, so
	// uninstall removes only a line mindctl wrote.
	codexHooksFeatureMarker = "# mindctl: hooks"
)

var (
	codexHookGroupHeader = regexp.MustCompile(`^\s*\[\[\s*hooks\s*\.\s*(\w+)\s*\]\]\s*(#.*)?$`)
	codexHookCommand     = regexp.MustCompile(`^\s*command\s*=\s*(".*"|'.*')\s*(#.*)?$`)
	// codexHooksConflict matches a top-level hooks or features value, which
	// the [[hooks.*]] registrations or a features.hooks key would collide with.
	codexHooksConflict     = regexp.MustCompile(`^\s*(hooks|features)\s*=`)
	codexHooksTable        = regexp.MustCompile(`^\s*\[\s*hooks\s*\]\s*(#.*)?$`)
	codexFeaturesHeader    = regexp.MustCompile(`^\s*\[\s*features\s*\]\s*(#.*)?$`)
	codexFeaturesHooks     = regexp.MustCompile(`^\s*hooks\s*=`)
	codexTopLevelHooksFlag = regexp.MustCompile(`^\s*features\s*\.\s*hooks\s*=`)
)

func codexStatusPath(home string) string {
	return filepath.Join(home, ".mindctl", "codex-status.sh")
}

func codexDirectivePath(home string) string {
	return filepath.Join(home, ".mindctl", "codex-directive.sh")
}

// codexSkill is a Codex skill that runs one mindctl command. The directive
// hook answers the same prompts without a model turn; the skill is the
// fallback when the hook does not fire.
type codexSkill struct {
	Name, Description, Title, Args, Instructions string
}

var codexSkills = []codexSkill{
	{
		Name:         "mindctl-on",
		Description:  "Route Codex through the Mindctl router again (turn it back on).",
		Title:        "Turn Mindctl routing on",
		Args:         "on --codex",
		Instructions: "Then report the result and tell the user the change takes effect on their next\n`codex` launch. Do not alter any other Codex settings.",
	},
	{
		Name:         "mindctl-off",
		Description:  "Route Codex to its default provider again (turn the Mindctl router off).",
		Title:        "Turn Mindctl routing off",
		Args:         "off --codex",
		Instructions: "Then report the result and tell the user the change takes effect on their next\n`codex` launch and can be reversed with `$mindctl-on`. Do not uninstall the\nrouter or alter any other Codex settings.",
	},
	{
		Name:         "mindctl-status",
		Description:  "Show whether Codex is routing through the Mindctl router or using its default provider.",
		Title:        "Mindctl router status",
		Args:         "status codex",
		Instructions: "Then summarize the result in one line. Do not change any configuration.",
	},
}

func codexSkillPath(home, name string) string {
	return filepath.Join(home, ".codex", "skills", name, "SKILL.md")
}

func codexSkillMarker(name string) string {
	return "<!-- mindctl managed " + name + " skill -->"
}

func renderCodexSkill(skill codexSkill, command string) []byte {
	return fmt.Appendf(nil, `---
name: %s
description: %q
---

%s

# %s

When the user invokes `+"`$%s`"+`, run exactly:

`+"```bash\n%s %s\n```"+`

%s
`, skill.Name, skill.Description, codexSkillMarker(skill.Name), skill.Title, skill.Name, command, skill.Args, skill.Instructions)
}

// mindctlCommand returns how the installed hooks and skills invoke mindctl:
// the bare name when it is on PATH, otherwise this executable's path.
func mindctlCommand() string {
	if _, err := exec.LookPath("mindctl"); err == nil {
		return "mindctl"
	}

	if path, err := os.Executable(); err == nil {
		return path
	}

	return "mindctl"
}

// shellQuote quotes value as one single-quoted shell word.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// installCodexHelpers writes the status and directive hooks and the toggle
// skills, then registers the hooks in document. The status hook runs on every
// session, so hooks are registered only when it is mindctl's own; the
// directive hook is registered only when it is too.
func installCodexHelpers(home string, document *codexDocument) ([]string, error) {
	command := mindctlCommand()
	var notes []string

	document.removeManagedHooks(home)
	statusPath, directivePath := codexStatusPath(home), codexDirectivePath(home)
	if document.hooksConflict() {
		notes = append(notes, "skipped the Codex status hooks because config.toml already defines hooks in a form mindctl cannot extend")
	} else {
		status, err := writeManagedFile(statusPath, codexStatusMarker, []byte(codexStatusScript), 0700)
		if err != nil {
			return nil, fmt.Errorf("write Codex status hook: %w", err)
		}

		if status == "" {
			notes = append(notes, fmt.Sprintf("left %s alone because mindctl did not write it, and skipped the Codex hooks", statusPath))
		} else {
			// The placeholder sits inside a single-quoted shell assignment.
			script := strings.Replace(codexDirectiveScript, "'__MINDCTL_BIN__'", shellQuote(command), 1)
			directive, err := writeManagedFile(directivePath, codexDirectiveMarker, []byte(script), 0700)
			if err != nil {
				return nil, fmt.Errorf("write Codex directive hook: %w", err)
			}

			registered := []string{statusPath}
			if directive == "" {
				notes = append(notes, fmt.Sprintf("left %s alone because mindctl did not write it", directivePath))
				directivePath = ""
			} else {
				registered = append(registered, directivePath)
			}

			document.enableHooks()
			document.addManagedHooks(statusPath, directivePath)
			notes = append(notes, fmt.Sprintf("registered hooks %s", strings.Join(registered, " and ")))
		}
	}

	skillCommand := command
	if strings.ContainsAny(command, " \t'\"$`\\") {
		skillCommand = shellQuote(command)
	}

	var installed []string
	for _, skill := range codexSkills {
		path := codexSkillPath(home, skill.Name)
		written, err := writeManagedFile(path, codexSkillMarker(skill.Name), renderCodexSkill(skill, skillCommand), 0600)
		if err != nil {
			return nil, fmt.Errorf("write Codex %s skill: %w", skill.Name, err)
		}

		if written == "" {
			notes = append(notes, fmt.Sprintf("left %s alone because mindctl did not write it", path))
			continue
		}
		installed = append(installed, "$"+skill.Name)
	}

	if len(installed) > 0 {
		notes = append(notes, "installed skills "+strings.Join(installed, ", "))
	}

	return notes, nil
}

// uninstallCodexHelpers removes the hook registrations, the hooks feature
// line setup added, and every helper file mindctl wrote. It reports whether
// anything changed.
func uninstallCodexHelpers(home string, document *codexDocument) (bool, error) {
	changed := document.removeManagedHooks(home)
	changed = document.removeHooksFeature() || changed

	paths := map[string]string{
		codexStatusPath(home):    codexStatusMarker,
		codexDirectivePath(home): codexDirectiveMarker,
	}
	for _, skill := range codexSkills {
		paths[codexSkillPath(home, skill.Name)] = codexSkillMarker(skill.Name)
	}

	for path, marker := range paths {
		removed, err := removeManagedFile(path, marker)
		if err != nil {
			return changed, fmt.Errorf("remove %s: %w", path, err)
		}

		if removed && strings.HasSuffix(path, "SKILL.md") {
			// Leave the skill directory when the user added files to it.
			_ = os.Remove(filepath.Dir(path))
		}
		changed = removed || changed
	}

	return changed, nil
}

// hooksConflict reports whether the config defines hooks or features in a
// form that [[hooks.*]] tables or a features.hooks key would collide with.
func (d *codexDocument) hooksConflict() bool {
	return slices.ContainsFunc(d.lines[:d.topLevelEnd()], codexHooksConflict.MatchString) ||
		slices.ContainsFunc(d.lines, codexHooksTable.MatchString)
}

// enableHooks turns on Codex's hooks feature unless it is already set. It
// uses an existing [features] table rather than adding a duplicate dotted key.
func (d *codexDocument) enableHooks() {
	if slices.ContainsFunc(d.lines[:d.topLevelEnd()], codexTopLevelHooksFlag.MatchString) {
		return
	}

	start := slices.IndexFunc(d.lines, codexFeaturesHeader.MatchString)
	if start < 0 {
		d.lines = slices.Insert(d.lines, 0, "features.hooks = true  "+codexHooksFeatureMarker)
		return
	}

	end := len(d.lines)
	if next := slices.IndexFunc(d.lines[start+1:], codexTableHeader.MatchString); next >= 0 {
		end = start + 1 + next
	}

	if slices.ContainsFunc(d.lines[start+1:end], codexFeaturesHooks.MatchString) {
		return
	}

	d.lines = slices.Insert(d.lines, start+1, "hooks = true  "+codexHooksFeatureMarker)
}

func (d *codexDocument) removeHooksFeature() bool {
	before := len(d.lines)
	d.lines = slices.DeleteFunc(d.lines, func(line string) bool {
		return strings.HasSuffix(strings.TrimSpace(line), codexHooksFeatureMarker)
	})

	return len(d.lines) != before
}

// addManagedHooks appends SessionStart and Stop registrations for the status
// hook and, when directive is set, a UserPromptSubmit registration for it.
func (d *codexDocument) addManagedHooks(status, directive string) {
	block := []string{codexHooksBegin}
	register := func(event, command string) {
		block = append(block,
			"[[hooks."+event+"]]",
			"[[hooks."+event+".hooks]]",
			`type = "command"`,
			"command = "+strconv.Quote(command),
			"",
		)
	}

	register("SessionStart", status)
	register("Stop", status)
	if directive != "" {
		register("UserPromptSubmit", directive)
	}

	block[len(block)-1] = codexHooksEnd
	if len(d.lines) > 0 {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, block...)
}

// removeManagedHooks deletes the managed block markers and every hook group
// whose command is one of mindctl's helpers. Groups are found by command, not
// by the markers, because Codex drops comments when it rewrites config.toml.
func (d *codexDocument) removeManagedHooks(home string) bool {
	helpers := []string{codexStatusPath(home), codexDirectivePath(home)}
	before := len(d.lines)
	d.lines = slices.DeleteFunc(d.lines, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		return trimmed == codexHooksBegin || trimmed == codexHooksEnd
	})
	changed := len(d.lines) != before

	for index := 0; index < len(d.lines); index++ {
		match := codexHookGroupHeader.FindStringSubmatch(d.lines[index])
		if match == nil {
			continue
		}

		// A group runs until the next table header outside hooks.<event>.
		nested := "hooks." + match[1] + "."
		end := len(d.lines)
		for next := index + 1; next < len(d.lines); next++ {
			if !codexTableHeader.MatchString(d.lines[next]) {
				continue
			}

			header := strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(d.lines[next]), "[]")), "")
			if !strings.HasPrefix(header, nested) {
				end = next
				break
			}
		}

		if !slices.ContainsFunc(d.lines[index+1:end], func(line string) bool {
			command := codexHookCommand.FindStringSubmatch(line)
			return command != nil && slices.Contains(helpers, unquoteTOML(command[1]))
		}) {
			index = end - 1
			continue
		}

		// The group took its trailing blank line; drop the blank before it
		// when that would leave two in a row or one at the end.
		d.lines = slices.Delete(d.lines, index, end)
		if index > 0 && strings.TrimSpace(d.lines[index-1]) == "" && (index == len(d.lines) || strings.TrimSpace(d.lines[index]) == "") {
			d.lines = slices.Delete(d.lines, index-1, index)
			index--
		}
		changed = true
		index--
	}

	return changed
}

// unquoteTOML returns the value of a TOML basic or literal string.
func unquoteTOML(value string) string {
	if strings.HasPrefix(value, "'") {
		return strings.Trim(value, "'")
	}

	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}

	return value
}
