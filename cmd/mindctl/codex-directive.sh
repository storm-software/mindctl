#!/usr/bin/env bash
# <!-- mindctl managed codex directive -->
#
# Codex UserPromptSubmit hook for Mindctl's local toggles. Written by
# `mindctl --codex`. Codex passes a JSON object on stdin carrying the raw
# prompt text, before `$skill` expansion and before any inference.
#
# $mindctl-on, $mindctl-off, and $mindctl-status rewrite or read this machine's
# Codex config, so the hook runs them locally and answers without a model turn.
# That matters most for $mindctl-off: as a skill it would need a model turn
# served through the router it is meant to bypass. The skills of the same
# names remain the fallback when this hook does not fire.
#
# FAIL OPEN. Every unexpected condition lets the prompt through untouched.

set -uo pipefail

# The mindctl command that wrote this hook; PATH is the fallback.
mindctl_bin='__MINDCTL_BIN__'

# pass_through hands the prompt to Codex unchanged.
pass_through() {
  printf '{"continue":true}\n'
  exit 0
}

# block stops the turn before any inference and shows reason to the user.
# Setting only `decision` is deliberate: `continue:false` would replace the
# reason with a bare "Hook stopped".
block() {
  local payload
  payload="$(jq -cn --arg r "$1" '{decision:"block", reason:$r}' 2>/dev/null)" || pass_through
  printf '%s\n' "$payload"
  exit 0
}

command -v jq >/dev/null 2>&1 || pass_through

payload="$(cat)" || pass_through
[ -n "$payload" ] || pass_through
prompt="$(jq -r '.prompt // ""' <<<"$payload" 2>/dev/null)" || pass_through

# Only a whole prompt that is exactly a directive is ours; prose that mentions
# one is the user's text.
prompt="${prompt%"${prompt##*[![:space:]]}"}"
# shellcheck disable=SC2016  # the directives start with a literal $
case "$prompt" in
  '$mindctl-on') args=(on --codex) ;;
  '$mindctl-off') args=(off --codex) ;;
  '$mindctl-status') args=(status codex) ;;
  *) pass_through ;;
esac

if [ ! -x "$mindctl_bin" ]; then
  mindctl_bin="$(command -v mindctl)" || pass_through
fi

output="$("$mindctl_bin" "${args[@]}" 2>&1)"
status=$?
[ -n "$output" ] || output="mindctl ${args[*]} exited with status $status."

if [ "$status" -eq 0 ] && [ "${args[0]}" != status ]; then
  output="$output"$'\n\n'"Takes effect on your next \`codex\` launch: Codex reads its provider config at startup, so this session keeps routing as it already was."
fi

block "$output"
