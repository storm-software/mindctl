#!/usr/bin/env bash
# <!-- mindctl managed codex status -->
#
# Codex lifecycle hook for the Mindctl router. Written by `mindctl --codex` and
# registered for SessionStart and Stop in ~/.codex/config.toml.
#
# Codex passes a JSON object on stdin. The hook reflects whether Codex routes
# through Mindctl, and the model Codex requested, in the terminal title, so the
# router stays visible between turns without adding to the conversation. It
# never writes to stdout, which is the hook protocol channel, and always exits
# zero so a broken helper cannot stall a turn.

set -uo pipefail

emit_title() {
  if [ -n "${MINDCTL_CODEX_STATUS_TITLE_FILE:-}" ]; then
    printf '%s\n' "$1" >"$MINDCTL_CODEX_STATUS_TITLE_FILE"
  elif [ -t 2 ] && [ -w /dev/tty ]; then
    printf '\033]0;%s\007' "$1" >/dev/tty
  fi
  return 0
}

safe_display_value() {
  printf '%s' "$1" | sed 's/[^A-Za-z0-9._:\/-]//g' | cut -c1-128
}

# routing_on reports whether Codex's top-level model_provider selects Mindctl.
# `mindctl off --codex` comments that line out and leaves these hooks in place.
routing_on() {
  local config="$HOME/.codex/config.toml"
  [ -f "$config" ] || return 1
  awk '
    /^[[:space:]]*\[/ { exit }
    /^[[:space:]]*model_provider[[:space:]]*=[[:space:]]*"mindctl"[[:space:]]*(#.*)?$/ { found = 1; exit }
    END { exit(found ? 0 : 1) }
  ' "$config"
}

payload="$(cat)"
if ! routing_on; then
  emit_title "Codex · direct"
  exit 0
fi

requested=""
if [ -n "$payload" ] && command -v jq >/dev/null 2>&1; then
  requested="$(safe_display_value "$(jq -r '.model // ""' <<<"$payload" 2>/dev/null)")"
fi

case "$requested" in
  "" | mindctl-auto) emit_title "Mindctl · auto routing" ;;
  *) emit_title "Mindctl · $requested" ;;
esac
exit 0
