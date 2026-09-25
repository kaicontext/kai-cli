#!/usr/bin/env bash
# Replay kai review-commit on the upstream pull requests in
# cmd/kai/testdata/review-regressions/cases.json and grade what it published
# against their golden comments.
#
#   scripts/review-regressions.sh                 # every case
#   scripts/review-regressions.sh calcom-14943    # one or more by id
#
# Each case is a real review: a blobless clone of the benchmark fork, a local
# graph, and `kai review-commit --deep` on the fork's PR range. It spends LLM
# credit through the Kai account in ~/.kai/credentials.json, so it is a manual
# check before and after a reviewer change, not part of `go test`.
#
# A case passes when every `expect` is published, no `forbid` is, and no
# `at_most` group is exceeded. "Published" means a risk claim in the bundle
# that is not a DECISION: what reaches the pull request as a defect.
#
# Environment:
#   KAI_BIN           kai binary to test (default: built from this checkout)
#   KAI_REVIEW_MODEL  reviewer model (default z-ai/glm-5.2, the review pod's)
#   REGRESSION_DIR    clones and results (default ~/.cache/kai-review-regressions)
#   PARALLEL          cases run at once (default 3)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cases="$root/cmd/kai/testdata/review-regressions/cases.json"
work="${REGRESSION_DIR:-$HOME/.cache/kai-review-regressions}"
parallel="${PARALLEL:-3}"
export KAI_REVIEW_MODEL="${KAI_REVIEW_MODEL:-z-ai/glm-5.2}"
export KAI_TELEMETRY=0
mkdir -p "$work"

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

if [ -z "${KAI_BIN:-}" ]; then
  KAI_BIN="$work/kai"
  (cd "$root" && go build -o "$KAI_BIN" ./cmd/kai)
fi
export KAI_BIN

ids=("$@")
if [ $# -eq 0 ]; then
  ids=()
  while IFS= read -r id; do ids+=("$id"); done < <(jq -r '.cases[].id' "$cases")
fi

run_id="$(date -u +%Y%m%dT%H%M%SZ)"
out="$work/runs/$run_id"
mkdir -p "$out"
echo "results: $out"

review_case() {
  local id="$1" c repo base head dir
  c="$(jq -c --arg id "$id" '.cases[] | select(.id == $id)' "$cases")"
  [ -n "$c" ] || { echo "$id: no such case" >&2; return 1; }
  repo="$(jq -r .repo <<<"$c")"; base="$(jq -r .base <<<"$c")"; head="$(jq -r .head <<<"$c")"
  dir="$work/clones/$id"
  if [ ! -d "$dir/.git" ]; then
    git clone --quiet --filter=blob:none --no-checkout "https://github.com/$repo.git" "$dir"
  fi
  (
    cd "$dir"
    git fetch --quiet origin "$base" "$head" 2>/dev/null || git fetch --quiet origin
    git checkout --quiet --force "$head"
    rm -rf .git/kai .kai
    local t0=$SECONDS
    "$KAI_BIN" init --yes --no-remote --no-history >"$out/$id.init.log" 2>&1
    "$KAI_BIN" review-commit "$head" --base "$base" --deep --format json \
      >"$out/$id.json" 2>"$out/$id.log" || echo "review exited $?" >>"$out/$id.log"
    echo "$id: reviewed in $((SECONDS - t0))s"
  )
}
export -f review_case
export cases work out

printf '%s\n' "${ids[@]}" | xargs -P "$parallel" -I{} bash -c 'review_case "$@"' _ {}

# Grade. A claim matches an entry when its statement names the entry's file
# (if one is given) and contains one of its phrases, case-insensitively.
grade='
  def published: [(.claims // [])[] | select((.tag // "risk") == "risk") | .statement
                  | select(startswith("Decision:") | not)];
  def hits($e): [published[] | ascii_downcase as $s
                 | select(($e.file == "" or ($s | contains($e.file | ascii_downcase)))
                          and any($e.any[]; . as $k | $s | contains($k | ascii_downcase)))];
  . as $f
  | $case
  | {id,
     missed:  [(.expect  // [])[] | . as $e | select(($f | hits($e)) | length == 0) | "\(.issue): \(.golden)"],
     leaked:  [(.forbid  // [])[] | . as $e | select(($f | hits($e)) | length > 0)  | "\(.issue): \(.was)"],
     exceeded:[(.at_most // [])[] | . as $e | ($f | hits($e + {file: ""})) as $h
               | select(($h | length) > $e.max) | "\($e.issue): \($h | length) claims (max \($e.max))"],
     published: ($f | published | length)}
  | .pass = ((.missed + .leaked + .exceeded) | length == 0)'

fail=0
for id in "${ids[@]}"; do
  c="$(jq -c --arg id "$id" '.cases[] | select(.id == $id)' "$cases")"
  if ! jq -e '.claims' "$out/$id.json" >/dev/null 2>&1; then
    echo "FAIL $id: no finding (see $out/$id.log)"; fail=1; continue
  fi
  r="$(jq -c --argjson case "$c" "$grade" "$out/$id.json")"
  echo "$r" >>"$out/grades.jsonl"
  if [ "$(jq -r .pass <<<"$r")" = true ]; then
    echo "PASS $id ($(jq -r .published <<<"$r") published)"
  else
    fail=1
    echo "FAIL $id ($(jq -r .published <<<"$r") published)"
    jq -r '(.missed[] | "  missed:   " + .), (.leaked[] | "  leaked:   " + .), (.exceeded[] | "  exceeded: " + .)' <<<"$r"
  fi
done
exit $fail
