#!/usr/bin/env bash
#
# Tests for pr-label.sh. Run it from anywhere:
#
#   bash .github/scripts/pr-label_test.sh
#
# The tests replace gh with a stub, so they need no network and no token.
# They need bash, jq and the usual POSIX tools.

set -euo pipefail

dir="$(cd "$(dirname "$0")" && pwd)"
script="$dir/pr-label.sh"
release_config="$dir/../release.yaml"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
failures=0

# The gh stub answers the three calls of pr-label.sh and logs every call.
# It runs the --jq filter of the call with the real jq.
#   STUB_LABELS  JSON of GET repos/<repo>/issues/<n>/labels
#   STUB_EVENTS  JSON of GET repos/<repo>/issues/<n>/events
#   STUB_FAIL    GET or POST: this kind of call fails like gh does
mkdir -p "$work/bin"
cat > "$work/bin/gh" << 'EOF'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_DIR/gh.log"
method="GET" path="" filter="."
while [ "$#" -gt 0 ]; do
  case "$1" in
    api | --paginate) ;;
    --method) method="$2"; shift ;;
    --jq) filter="$2"; shift ;;
    -f) shift ;;
    *) path="$1" ;;
  esac
  shift
done
if [ "${STUB_FAIL:-}" = "$method" ]; then
  echo "gh: Server Error (HTTP 502)" >&2
  exit 1
fi
case "$method $path" in
  "GET "*/labels) jq -r "$filter" <<< "${STUB_LABELS:-[]}" ;;
  "GET "*/events) jq -r "$filter" <<< "${STUB_EVENTS:-[]}" ;;
  "POST "*/labels) echo '[]' ;;
  *) echo "gh stub: unexpected call $method $path" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/gh"

# run [VAR=value ...] runs pr-label.sh with only these variables. It sets
# $status, $out (all messages) and $calls (the gh calls, one per line).
run() {
  rm -f -- "$work/gh.log"
  status=0
  out="$(env -i PATH="$work/bin:$PATH" STUB_DIR="$work" "$@" bash "$script" 2>&1)" || status=$?
  calls="$(cat "$work/gh.log" 2> /dev/null || true)"
}

report() {
  if [ -z "$2" ]; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    echo "      $2"
    echo "      output: $(tr '\n' '|' <<< "$out")"
    echo "      gh calls: $(tr '\n' '|' <<< "$calls")"
    failures=$((failures + 1))
  fi
}

post_call() {
  echo "gh api --method POST repos/sebingel/gitlab-migrator/issues/7/labels -f labels[]=$1"
}

# expect_label NAME LABEL [VAR=value ...]
# The script must succeed and add exactly LABEL with POST.
expect_label() {
  local name="$1" label="$2" problems="" posts
  shift 2
  run "$@"
  if [ "$status" -ne 0 ]; then
    problems="exit status $status;"
  fi
  posts="$(grep -F -- "--method POST" <<< "$calls" || true)"
  if [ "$posts" != "$(post_call "$label")" ]; then
    problems="$problems want one POST of $label, got '$posts';"
  fi
  report "$name" "$problems"
}

# expect_no_label NAME TEXT [VAR=value ...]
# The script must succeed, add no label and print TEXT.
expect_no_label() {
  local name="$1" text="$2" problems=""
  shift 2
  run "$@"
  if [ "$status" -ne 0 ]; then
    problems="exit status $status;"
  fi
  if grep -qF -- "--method POST" <<< "$calls"; then
    problems="$problems it added a label;"
  fi
  if ! grep -qF -- "$text" <<< "$out"; then
    problems="$problems output does not contain '$text';"
  fi
  report "$name" "$problems"
}

# expect_error NAME TEXT [VAR=value ...]
# The script must fail, and its output must contain TEXT.
expect_error() {
  local name="$1" text="$2" problems=""
  shift 2
  run "$@"
  if [ "$status" -eq 0 ]; then
    problems="expected an error, got exit status 0;"
  fi
  if ! grep -qF -- "$text" <<< "$out"; then
    problems="$problems output does not contain '$text';"
  fi
  report "$name" "$problems"
}

expect_api_calls() {
  local count=0
  if [ -n "$calls" ]; then
    count="$(wc -l <<< "$calls" | tr -d ' ')"
  fi
  if [ "$count" = "$2" ]; then
    report "$1" ""
  else
    report "$1" "want $2 gh call(s), got $count"
  fi
}

# labels NAME... prints the JSON of the labels endpoint.
labels() {
  jq -cn '$ARGS.positional | map({name: .})' --args "$@"
}

# events EVENT=LABEL... prints the JSON of the events endpoint.
events() {
  jq -cn '$ARGS.positional | map(split("=") | {event: .[0], label: {name: .[1]}})' --args "$@"
}

pr=(REPO=sebingel/gitlab-migrator NUMBER=7)

echo "--- branch prefixes"

expect_label "feat/ gets enhancement" enhancement "${pr[@]}" BRANCH=feat/issue-47-automated-changelog
expect_api_calls "reads labels and events, then adds the label" 3
expect_label "feature/ gets enhancement" enhancement "${pr[@]}" BRANCH=feature/fix-skip-invalid-merge-requests
expect_label "fix/ gets bug" bug "${pr[@]}" BRANCH=fix/mr-paging-state-and-hints
expect_label "bugfix/ gets bug" bug "${pr[@]}" BRANCH=bugfix/detect-branches-with-slashes
expect_label "hotfix/ gets bug" bug "${pr[@]}" BRANCH=hotfix/x
expect_label "docs/ gets documentation" documentation "${pr[@]}" BRANCH=docs/readme-and-scripts-flags
expect_label "upper case prefix counts" bug "${pr[@]}" BRANCH=Fix/Retry-404
# shellcheck disable=SC2016 # the branch name must reach the script as text
expect_label "a branch name is never run as code" enhancement "${pr[@]}" BRANCH='feat/$(touch pwned)'
if [ -e pwned ] || [ -e "$work/pwned" ]; then
  report "and creates no file" "a file 'pwned' exists"
fi

for branch in chore/issue-44-remove-vim-modelines ci/issue-43-claude-review-comments \
  refactor/simplify-main dependabot/go_modules/golang.org/x/net-0.56.0 copilot/add-go feat \
  feat-unarchive-archived-repos feature-flag-cleanup fixes fixtures/x docsx/y main; do
  expect_no_label "$branch gets no label" "has no prefix with a label" "${pr[@]}" BRANCH="$branch"
  expect_api_calls "and makes no API call" 0
done

echo "--- labels that are there already"

for label in enhancement bug documentation dependencies Documentation; do
  expect_no_label "a pull request with $label keeps its section" "has the section label" \
    "${pr[@]}" BRANCH=feat/x STUB_LABELS="$(labels release:skip "$label")"
done

expect_label "release:minor does not choose the kind, so a fix still gets bug" bug \
  "${pr[@]}" BRANCH=fix/x STUB_LABELS="$(labels release:minor)"
expect_label "release:major and release:skip do not stop it either" enhancement \
  "${pr[@]}" BRANCH=feat/x STUB_LABELS="$(labels release:major release:skip)"
expect_label "a label that only looks like a section label does not count" enhancement \
  "${pr[@]}" BRANCH=feat/x STUB_LABELS="$(labels enhancements bug-report)"

echo "--- labels that were removed"

expect_no_label "a label that someone removed does not come back" "It is not added again" \
  "${pr[@]}" BRANCH=feat/x STUB_EVENTS="$(events labeled=enhancement unlabeled=enhancement)"
expect_label "other removed labels do not matter" enhancement \
  "${pr[@]}" BRANCH=feat/x STUB_EVENTS="$(events labeled=bug unlabeled=bug labeled=release:skip)"
expect_label "an unlabeled event alone does not count" enhancement \
  "${pr[@]}" BRANCH=feat/x STUB_EVENTS="$(events unlabeled=enhancement)"

echo "--- errors"

expect_error "a failed read stops the script" "HTTP 502" "${pr[@]}" BRANCH=feat/x STUB_FAIL=GET
if grep -qF -- "--method POST" <<< "$calls"; then
  report "and adds no label" "it added a label"
fi
expect_error "a failed POST fails the script" "HTTP 502" "${pr[@]}" BRANCH=feat/x STUB_FAIL=POST
expect_error "missing branch" "BRANCH is required" REPO=sebingel/gitlab-migrator NUMBER=7
expect_error "missing number" "NUMBER is required" REPO=sebingel/gitlab-migrator BRANCH=feat/x
expect_error "missing repository" "REPO is required" NUMBER=7 BRANCH=feat/x

echo "--- release.yaml"

# The labels of the categories in release.yaml: list items under a
# "labels:" key with six spaces of indent. Labels under "exclude:" have a
# deeper indent and do not count.
config_labels="$(tr -d '\r' < "$release_config" | awk '
  /^      labels:/ { on = 1; next }
  on && /^        - / { sub(/^        - /, ""); gsub(/"/, ""); print; next }
  { on = 0 }' | grep -vx -e '\*' -e 'release:.*' | sort)"
script_labels="$(tr -d '\r' < "$script" | sed -n 's/^section_labels=(\(.*\))$/\1/p' | tr ' ' '\n' | sort)"
if [ -n "$config_labels" ] && [ "$config_labels" = "$script_labels" ]; then
  report "section_labels matches the categories of release.yaml" ""
else
  report "section_labels matches the categories of release.yaml" \
    "release.yaml: $(paste -sd ' ' - <<< "$config_labels"), pr-label.sh: $(paste -sd ' ' - <<< "$script_labels")"
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures test(s) failed"
  exit 1
fi
echo "all tests passed"
