#!/usr/bin/env bash
#
# Claude Code statusline for the Mindctl router. Written by `mindctl --claude`;
# rerun it to refresh the model prices below after changing the gateway config.
#
# Claude Code pipes a JSON blob on stdin whose `transcript_path` points at the
# JSONL log of the current session and whose `model.id` is the user's
# Claude-side model selection. The gateway answers with the served model's
# configured ID in `message.model`, which Claude Code stores in the transcript
# verbatim. Per-turn savings compare each turn's served cost against what the
# selection (or the gateway's savings.baseline_model when the selection is not
# a configured model) would have cost on the same tokens.
#
# Renders:
#   MINDCTL — gpt-6-sol ← claude-opus-5-5 · saved $1.23 · 12.4k in / 3.1k out / 45.2k cache read

set -euo pipefail

brand=$'\033[38;2;99;102;241mMINDCTL\033[0m'

input="$(cat)"
if ! command -v jq >/dev/null 2>&1; then
  printf '%s' "$brand"
  exit 0
fi

transcript_path="$(printf '%s' "$input" | jq -r '.transcript_path // empty')"
# Prefer model.id over display_name: pricing keys and the served model in the
# transcript are model IDs, while display_name is a human label.
selected="$(printf '%s' "$input" | jq -r '.model.id // .model.display_name // "?"')"

# Strip Claude Code's variant tag and date suffix so the selection matches a
# pricing key: claude-opus-5-5[1m] and claude-opus-5-5-20260101 both become
# claude-opus-5-5.
normalize_model() {
  printf '%s' "$1" | sed -E 's/\[[^]]*\]$//; s/-[0-9]{8}$//'
}

# Prices in USD per million tokens, keyed by configured model ID, and the
# savings baseline, both read from the gateway config when mindctl set this up.
pricing='__MINDCTL_PRICING__'

served=""
session_savings=""
tot_in=0
tot_out=0
tot_cache_read=0
tot_cache_write=0

selected_norm="$(normalize_model "$selected")"
baseline="$(jq -rn --argjson p "$pricing" --arg s "$selected_norm" \
  'if $p.models[$s] then $s else ($p.baseline_model // "") end' 2>/dev/null || true)"

if [[ -n "$transcript_path" && -f "$transcript_path" ]]; then
  # macOS ships `tail -r`, GNU coreutils ships `tac`.
  if command -v tac >/dev/null 2>&1; then reverse=(tac); else reverse=(tail -r); fi

  # Claude Code stamps message.model = "<synthetic>" on turns it generated
  # locally (errors, cancellations); show those as "failure".
  served="$("${reverse[@]}" "$transcript_path" 2>/dev/null \
    | jq -r 'select(.type=="assistant") | .message.model // empty' \
    | head -n 1 || true)"
  if [[ "$served" == "<synthetic>" ]]; then
    served="failure"
  else
    served="$(normalize_model "$served")"
  fi

  # Claude Code writes one JSONL entry per content block of an assistant turn,
  # each carrying the same usage, so dedupe on (message.id, usage) before
  # summing. Turns served by the baseline, or with either model unpriced,
  # save nothing; token totals count every turn.
  read -r session_savings tot_in tot_out tot_cache_read tot_cache_write < <(
    jq -rs --argjson p "$pricing" --arg baseline "$baseline" '
      [.[] | select(.type=="assistant")] |
      unique_by([.message.id, .message.usage]) |
      .[] |
      .message as $m |
      ($m.model // "" | sub("\\[[^]]*\\]$"; "") | sub("-[0-9]{8}$"; "")) as $sm |
      {
        in:   ($m.usage.input_tokens // 0),
        out:  ($m.usage.output_tokens // 0),
        cwrt: ($m.usage.cache_creation_input_tokens // 0),
        crd:  ($m.usage.cache_read_input_tokens // 0)
      } as $t |
      def cost($price): ($t.in * $price.input + $t.cwrt * $price.cache_write
        + $t.crd * $price.cache_read + $t.out * $price.output) / 1000000;
      (if $baseline == "" or $baseline == $sm then 0
       else
         ($p.models[$sm] // null) as $sp | ($p.models[$baseline] // null) as $bp |
         if $sp == null or $bp == null then 0 else cost($bp) - cost($sp) end
       end) as $savings |
      "\($savings) \($t.in) \($t.out) \($t.crd) \($t.cwrt)"
    ' "$transcript_path" 2>/dev/null \
    | awk 'BEGIN{s=0; i=0; o=0; r=0; w=0}
           {s+=$1; i+=$2; o+=$3; r+=$4; w+=$5}
           END{printf "%.4f %d %d %d %d\n", s, i, o, r, w}'
  ) || true
fi

fmt_money() {
  awk -v v="$1" 'BEGIN{
    if (v == "" || v+0 <= 0) { printf "$0.00"; exit }
    if (v+0 < 0.005)         { printf "<$0.01"; exit }
    printf "$%.2f", v
  }'
}

fmt_tok() {
  awk -v v="$1" 'BEGIN{
    v = v+0
    if (v >= 1000000) { printf "%.1fM", v/1000000; exit }
    if (v >= 1000)    { printf "%.1fk", v/1000;    exit }
    printf "%d", v
  }'
}

# Cache reads and writes are priced very differently, so each is shown
# separately and only when nonzero.
tokens_clause=""
if [[ "$tot_in" -gt 0 || "$tot_out" -gt 0 || "$tot_cache_read" -gt 0 || "$tot_cache_write" -gt 0 ]]; then
  tokens_clause=" · $(fmt_tok "$tot_in") in / $(fmt_tok "$tot_out") out"
  if [[ "$tot_cache_read" -gt 0 ]]; then
    tokens_clause+=" / $(fmt_tok "$tot_cache_read") cache read"
  fi
  if [[ "$tot_cache_write" -gt 0 ]]; then
    tokens_clause+=" / $(fmt_tok "$tot_cache_write") cache write"
  fi
fi

if [[ "$served" == "failure" || -z "$served" ]]; then
  printf '%s — %s%s' "$brand" "${served:-$selected}" "$tokens_clause"
elif [[ -n "$selected" && "$selected" != "?" ]]; then
  # A negative total means routing picked pricier models than the baseline;
  # fmt_money floors it at $0.00 rather than claiming a negative saving.
  savings_clause=""
  if [[ -n "$baseline" ]]; then
    savings_clause=" · saved $(fmt_money "$session_savings")"
  fi
  printf '%s — %s ← %s%s%s' "$brand" "$served" "$selected" "$savings_clause" "$tokens_clause"
else
  printf '%s — %s%s' "$brand" "$served" "$tokens_clause"
fi
