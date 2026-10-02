#!/usr/bin/env bash
#
# Tests for release-plan.sh. Run it from anywhere:
#
#   bash .github/scripts/release-plan_test.sh
#
# The tests replace gh with a stub, so they need no network and no token.
# They need bash, jq and the usual POSIX tools.

set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release-plan.sh"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
failures=0

# The gh stub answers "gh api repos/<repo>/pulls/<n>" and the list
# "gh api repos/<repo>/pulls?state=closed&head=<owner>:<branch>", and logs
# every call.
#   STUB_PULL      JSON that it prints for pulls/<n>
#   STUB_PULLS     JSON array of all pull requests. Without STUB_PULL,
#                  pulls/<n> prints the one with this number (HTTP 404 if
#                  there is none). The list prints the ones whose head
#                  label matches, without "merged" (the list API has only
#                  merged_at), and only open ones without state=closed.
#   STUB_LIST      JSON that it prints for the list instead
#   STUB_LIST_STATUS
#                  502 makes every list call fail with HTTP 502, 404 makes
#                  it fail like gh does for HTTP 404
#   STUB_STATUS    404 makes it fail like gh does for HTTP 404
#   STUB_FAILURES  number of calls that fail with HTTP 502 first
mkdir -p "$work/bin"
cat > "$work/bin/gh" << 'EOF'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_DIR/gh.log"
count=$(($(cat "$STUB_DIR/gh.count" 2> /dev/null || echo 0) + 1))
echo "$count" > "$STUB_DIR/gh.count"
not_found() {
  echo '{"message":"Not Found","status":"404"}'
  echo "gh: Not Found (HTTP 404)" >&2
  exit 1
}
if [ "$count" -le "${STUB_FAILURES:-0}" ]; then
  echo "gh: Server Error (HTTP 502)" >&2
  exit 1
fi
if [ "${STUB_STATUS:-200}" = "404" ]; then
  not_found
fi
case "$2" in
  */pulls\?*)
    if [ "${STUB_LIST_STATUS:-200}" = "404" ]; then
      not_found
    fi
    if [ "${STUB_LIST_STATUS:-200}" = "502" ]; then
      echo "gh: Server Error (HTTP 502)" >&2
      exit 1
    fi
    if [ -n "${STUB_LIST+x}" ]; then
      printf '%s\n' "$STUB_LIST"
      exit 0
    fi
    head="${2#*head=}"
    head="${head%%&*}"
    state=open
    if [[ "$2" == *state=closed* ]]; then
      state=closed
    fi
    jq -c --arg head "$head" --arg state "$state" \
      'map(select((.head.label | @uri) == $head and .state == $state) | del(.merged))' \
      <<< "${STUB_PULLS:-[]}"
    ;;
  */pulls/*)
    if [ -n "${STUB_PULL+x}" ]; then
      printf '%s\n' "$STUB_PULL"
      exit 0
    fi
    jq -ce --argjson n "${2##*/}" '.[] | select(.number == $n)' <<< "${STUB_PULLS:-[]}" || not_found
    ;;
  *)
    echo "gh stub: unexpected call: gh $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$work/bin/gh"

# run MODE [VAR=value ...] runs release-plan.sh with only these variables.
# It sets $status, $out (all messages) and $result (the key=value lines).
run() {
  local mode="$1"
  shift
  rm -f -- "$work/gh.log" "$work/gh.count" "$work/result" "$work/summary"
  status=0
  out="$(env -i PATH="$work/bin:$PATH" STUB_DIR="$work" API_RETRY_DELAY=0 \
    RESULT_FILE="$work/result" "$@" bash "$script" "$mode" 2>&1)" || status=$?
  result="$(cat "$work/result" 2> /dev/null || true)"
}

get() {
  sed -n "s/^$1=//p" <<< "$result"
}

api_calls() {
  if [ -f "$work/gh.log" ]; then
    wc -l < "$work/gh.log" | tr -d ' '
  else
    echo 0
  fi
}

report() {
  if [ -z "$2" ]; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    echo "      $2"
    echo "      output: $(tr '\n' '|' <<< "$out")"
    failures=$((failures + 1))
  fi
}

# expect NAME "key=value ..." [VAR=value ...]
# The plan must succeed and every listed key must have the listed value.
expect() {
  local name="$1" pair key got problems=""
  local -a pairs
  read -ra pairs <<< "$2"
  shift 2
  run plan "$@"
  if [ "$status" -ne 0 ]; then
    problems="exit status $status"
  fi
  for pair in "${pairs[@]}"; do
    key="${pair%%=*}"
    got="$(get "$key")"
    if [ "$key=$got" != "$pair" ]; then
      problems="$problems want $pair, got $key=$got;"
    fi
  done
  report "$name" "$problems"
}

# expect_error NAME TEXT MODE [VAR=value ...]
# The script must fail, and its output must contain TEXT.
expect_error() {
  local name="$1" text="$2" mode="$3" problems=""
  shift 3
  run "$mode" "$@"
  if [ "$status" -eq 0 ]; then
    problems="expected an error, got exit status 0;"
  fi
  if ! grep -qF -- "$text" <<< "$out"; then
    problems="$problems output does not contain '$text';"
  fi
  report "$name" "$problems"
}

# The next two check the last run.
expect_message() {
  if grep -qF -- "$2" <<< "$out"; then
    report "$1" ""
  else
    report "$1" "output does not contain '$2'"
  fi
}

expect_api_calls() {
  local calls
  calls="$(api_calls)"
  if [ "$calls" = "$2" ]; then
    report "$1" ""
  else
    report "$1" "want $2 API call(s), got $calls"
  fi
}

# pull NUMBER MERGE_COMMIT_SHA LABELS [BASE_REF] [MERGED] [BASE_REPO] [HEAD_REF]
# prints a closed pull request as the API returns it. LABELS is a JSON array
# of label names. An empty argument takes the default.
pull() {
  jq -cn --argjson n "$1" --arg sha "$2" --argjson labels "$3" --arg base "${4:-main}" \
    --argjson merged "${5:-true}" --arg repo "${6:-sebingel/gitlab-migrator}" \
    --arg head "${7:-feat/pr-$1}" \
    '{number: $n, state: "closed", merged: $merged,
      merged_at: (if $merged then "2026-10-01T16:38:50Z" else null end),
      merge_commit_sha: $sha,
      base: {ref: $base, repo: {full_name: $repo}},
      head: {ref: $head, label: "\($repo | split("/")[0]):\($head)"},
      labels: ($labels | map({name: .}))}'
}

# stack_pulls BOTTOM MIDDLE TOP prints the pull requests of a stack that
# was merged in one step with the merge commit $sha, like #52, #59 and #56
# of issue 99: #52 (head chore/a) into main, #59 (head ci/b) into chore/a,
# and #56 (head feat/c) into ci/b. Each argument is a JSON array of labels.
# #30 is an older pull request from chore/a with another merge commit.
stack_pulls() {
  jq -sc . << PULLS
$(pull 30 "$other_sha" '["release:major"]' main true "" chore/a)
$(pull 52 "$sha" "$1" main true "" chore/a)
$(pull 59 "$sha" "$2" chore/a true "" ci/b)
$(pull 56 "$sha" "$3" ci/b true "" feat/c)
PULLS
}

repo="sebingel/gitlab-migrator"
main="refs/heads/main"
sha="1111111111111111111111111111111111111111"
other_sha="9999999999999999999999999999999999999999"
head_sha="2222222222222222222222222222222222222222"
tags=$'v0.9.0\nv0.15.1\nv0.16.0\nv0.10.0'
merge_msg=$'Merge pull request #41 from sebingel/feat/x\n\nadds x'

# Everything that a push of the merge commit of pull request #41 needs.
push_env=(EVENT=push REF="$main" SHA="$sha" REPO="$repo" TAGS="$tags" MERGED_TAGS="$tags"
  COMMIT_MESSAGE="$merge_msg")
dispatch_env=(EVENT=workflow_dispatch REF="$main" SHA="$sha" REPO="$repo" TAGS="$tags"
  MERGED_TAGS="$tags")
pr_env=(EVENT=pull_request REF=refs/pull/41/merge SHA="$other_sha" PR_HEAD_SHA="$head_sha"
  TAGS="$tags")

echo "--- version rules (pull request dry run)"

expect "no tag starts at v0.0.1" \
  "previous= bump=patch version=v0.0.1 release=true publish=false build_version=v0.0.1-dryrun.2222222" \
  "${pr_env[@]}" TAGS= PR_LABELS='[]'

expect "patch is the default" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=true publish=false" \
  "${pr_env[@]}" PR_LABELS='[]'

expect "release:minor" \
  "bump=minor version=v0.17.0 release=true build_version=v0.17.0-dryrun.2222222" \
  "${pr_env[@]}" PR_LABELS='["dependencies","release:minor"]'

expect "release:major wins over release:minor" \
  "bump=major version=v1.0.0 release=true" \
  "${pr_env[@]}" PR_LABELS='["release:minor","release:major"]'

expect "release:skip" \
  "version=v0.16.1 release=false publish=false build_version=v0.16.1-dryrun.2222222" \
  "${pr_env[@]}" PR_LABELS='["release:skip"]'

expect "release:skip wins over release:major" \
  "bump=major release=false" \
  "${pr_env[@]}" PR_LABELS='["release:major","release:skip"]'

expect "labels must match exactly" \
  "bump=patch release=true" \
  "${pr_env[@]}" PR_LABELS='["Release:Major","release:minor-ish","release:skip "]'

expect "versions sort as numbers" \
  "previous=v0.10.0 version=v0.10.1" \
  "${pr_env[@]}" TAGS=$'v0.9.0\nv0.10.0\nv0.2.0' PR_LABELS='[]'

expect "tags that are not vX.Y.Z are ignored" \
  "previous=v0.16.0 version=v0.16.1" \
  "${pr_env[@]}" TAGS=$'v0.16.0\nv0.17.0-rc.1\nv1.0\nv01.0.0\nlatest\n1.2.3' PR_LABELS='[]'

expect "dry run version uses the head commit, not the merge ref (finding 10)" \
  "build_version=v0.16.1-dryrun.2222222" \
  "${pr_env[@]}" PR_LABELS='[]'

expect_api_calls "dry run makes no API call" 0

expect "a GitHub skip marker in the title does not change the plan" \
  "release=true publish=false" \
  "${pr_env[@]}" PR_LABELS='[]' PR_TITLE='fixes the docs [Skip CI]'
expect_message "but the dry run warns that the merge will not release" "[skip ci]"

run plan "${pr_env[@]}" PR_LABELS='[]' PR_TITLE='fixes the ci skip logic' \
  PR_COMMIT_MESSAGES=$'@@@ aaaaaaa\nadds x\n\nno markers here\n@@@ bbbbbbb\nfixes y'
if [ "$status" -eq 0 ] && ! grep -qF "::warning::" <<< "$out"; then
  report "no warning for a normal title and normal commits" ""
else
  report "no warning for a normal title and normal commits" "exit status $status, output: $out"
fi

expect "a GitHub skip marker in a commit message does not change the plan" \
  "release=true publish=false" \
  "${pr_env[@]}" PR_LABELS='[]' PR_TITLE='adds x' \
  PR_COMMIT_MESSAGES=$'@@@ aaaaaaa\nadds x\n@@@ bbbbbbb\nfixes y\n\nthe text [No CI] in the body'
expect_message "but the dry run warns about that commit" "Commit bbbbbbb"
expect_message "and names the marker" "[no ci]"
if grep -qF "Commit aaaaaaa" <<< "$out"; then
  report "and does not blame the clean commit" "output: $out"
else
  report "and does not blame the clean commit" ""
fi

expect "a skip-checks trailer in a commit message does not change the plan" \
  "release=true publish=false" \
  "${pr_env[@]}" PR_LABELS='[]' PR_TITLE='adds x' \
  PR_COMMIT_MESSAGES=$'@@@ aaaaaaa\nadds x\n@@@ bbbbbbb\nfixes y\n\n\nskip-checks: true'
expect_message "but the dry run warns about that commit" "Commit bbbbbbb"
expect_message "and names the trailer" "skip-checks"

run plan "${pr_env[@]}" PR_LABELS='[]' PR_TITLE='adds x' \
  PR_COMMIT_MESSAGES=$'@@@ aaaaaaa\nadds x\n\n\nSkip-Checks:true\n@@@ bbbbbbb\nfixes y\n\n\nskip-checks: true\r'
expect_message "the trailer without a space and in mixed case warns" "Commit aaaaaaa"
expect_message "the trailer with a CRLF line end warns" "Commit bbbbbbb"

run plan "${pr_env[@]}" PR_LABELS='[]' PR_TITLE='adds x' \
  PR_COMMIT_MESSAGES=$'@@@ aaaaaaa\nadds x\n\n\nskip-checks: false\n@@@ bbbbbbb\nfixes y\n\nskip-checks: truely\nwe removed skip-checks: true handling\nwe removed skip-checks: true'
if [ "$status" -eq 0 ] && ! grep -qF "::warning::" <<< "$out"; then
  report "no warning for skip-checks: false, other words or the text in a sentence" ""
else
  report "no warning for skip-checks: false, other words or the text in a sentence" "exit status $status, output: $out"
fi

run plan "${pr_env[@]}" PR_LABELS='[]' PR_TITLE='adds x' \
  PR_COMMIT_MESSAGES=$'@@@ aaaaaaa\nadds x\n\n\nskip-checks: true\nskip-checks:true'
if [ "$(grep -c "has a 'skip-checks: true' trailer" <<< "$out")" = "1" ]; then
  report "a trailer twice in one commit warns once" ""
else
  report "a trailer twice in one commit warns once" "output: $out"
fi

echo "--- push to main"

expect "merge of a pull request without labels" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=true publish=true build_version=v0.16.1 pull_request=41 pull_requests=41" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$sha" '[]')"
expect_api_calls "reads only pull request #41 (finding 5)" 1
if grep -qxF "gh api repos/sebingel/gitlab-migrator/pulls/41" "$work/gh.log"; then
  report "API path is repos/<repo>/pulls/41" ""
else
  report "API path is repos/<repo>/pulls/41" "gh.log: $(cat "$work/gh.log")"
fi
run plan "${push_env[@]}" SUMMARY_FILE="$work/summary" STUB_PULL="$(pull 41 "$sha" '[]')"
if grep -qxF "| Pull request | #41 |" "$work/summary"; then
  report "the job summary names the pull request" ""
else
  report "the job summary names the pull request" "summary: $(cat "$work/summary")"
fi

expect "merge of a pull request with release:minor" \
  "bump=minor version=v0.17.0 release=true publish=true" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$sha" '["release:minor"]')"

expect "merge of a pull request with release:skip" \
  "release=false publish=false" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$sha" '["release:skip"]')"

expect "[skip release] in the message does not skip any more (finding 1)" \
  "release=true publish=true" \
  "${push_env[@]}" COMMIT_MESSAGE=$'Merge pull request #41 from sebingel/feat/x\n\nfixes [skip release] handling' \
  STUB_PULL="$(pull 41 "$sha" '[]')"

expect "API errors are retried (finding 7)" \
  "version=v0.16.1 publish=true pull_request=41" \
  "${push_env[@]}" STUB_FAILURES=2 STUB_PULL="$(pull 41 "$sha" '[]')"
expect_api_calls "two failures and one success" 3

expect_error "API errors that do not stop fail the run (finding 7)" \
  "Could not read pull request #41" plan \
  "${push_env[@]}" STUB_FAILURES=9 STUB_PULL="$(pull 41 "$sha" '[]')"
expect_api_calls "gives up after 3 calls" 3

expect "pull request number that does not exist here, for example a fork sync (finding 6)" \
  "bump=patch version=v0.16.1 release=true publish=true pull_request=" \
  "${push_env[@]}" STUB_STATUS=404
expect_message "warns that no labels apply" "not the merge commit of a pull request"
expect_api_calls "404 is not retried" 1

expect "pull request whose merge commit is another commit (finding 6)" \
  "bump=patch release=true publish=true pull_request=" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$other_sha" '["release:skip"]')"
expect_message "warns about the other commit" "not the merge commit of a pull request"

expect "pull request into another branch" \
  "release=true publish=true pull_request=" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$sha" '["release:skip"]' develop)"

expect "pull request that is not merged" \
  "release=true publish=true pull_request=" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$sha" '["release:skip"]' main false)"

expect "pull request of another repository" \
  "release=true publish=true pull_request=" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$sha" '["release:skip"]' main true manicminer/gitlab-migrator)"

expect_error "API answer that is not a JSON object" "is not a JSON object" plan \
  "${push_env[@]}" STUB_PULL='[]'

expect "direct push without a pull request (finding 6)" \
  "bump=patch version=v0.16.1 release=true publish=true pull_request=" \
  "${push_env[@]}" COMMIT_MESSAGE=$'fixes a typo\n\nsome text'
expect_api_calls "direct push makes no API call" 0
expect_message "warns about the direct push" "not the merge commit of a pull request"

expect "a title that ends with an issue number is a direct push (finding 6)" \
  "release=true publish=true pull_request=" \
  "${push_env[@]}" COMMIT_MESSAGE='fixes the retry (#12)'
expect_api_calls "issue number makes no API call" 0

# GitHub can send a second push event for the same commit (issue 98), and
# "Re-run all jobs" on a finished release run is a push run too.
expect "push of a commit that already has a release tag skips the release (finding 3, issue 98)" \
  "previous=v0.16.0 release=false publish=false pull_request=" \
  "${push_env[@]}" SHA_TAGS=v0.16.0 STUB_PULL="$(pull 41 "$sha" '[]')"
if [ "$(get reason)" = "already released as v0.16.0" ]; then
  report "and gives the tag as the reason" ""
else
  report "and gives the tag as the reason" "reason=$(get reason)"
fi
expect_message "and writes a notice" "::notice::$sha is already released as v0.16.0"
if grep -qF "::error::" <<< "$out"; then
  report "and writes no error" "output: $out"
else
  report "and writes no error" ""
fi
expect_api_calls "and makes no API call" 0

expect "push of an older released commit after a newer release skips the release too" \
  "release=false publish=false" \
  "${push_env[@]}" SHA_TAGS=v0.15.1 MERGED_TAGS=$'v0.9.0\nv0.15.1' STUB_PULL="$(pull 41 "$sha" '[]')"

expect "other tags on the commit do not count as a release" \
  "version=v0.16.1 publish=true" \
  "${push_env[@]}" SHA_TAGS=$'latest\nv0.17.0-rc.1' STUB_PULL="$(pull 41 "$sha" '[]')"

expect_error "commit that does not contain the previous release" \
  "does not contain the previous release v0.16.0" plan \
  "${push_env[@]}" MERGED_TAGS=$'v0.9.0\nv0.15.1' STUB_PULL="$(pull 41 "$sha" '[]')"
expect_message "and names the queue order as a cause" "run of a newer merge first"

expect "first release without any tag" \
  "previous= version=v0.0.1 publish=true" \
  "${push_env[@]}" TAGS= MERGED_TAGS= STUB_PULL="$(pull 41 "$sha" '[]')"

expect_error "push outside main" "only made from main" plan \
  "${push_env[@]}" REF=refs/heads/feature STUB_PULL="$(pull 41 "$sha" '[]')"

echo "--- stack merge on main (issue 99)"

# A stack merge has one merge commit for all its pull requests. Its first
# line names the top pull request, whose base is the branch below it.
stack_env=("${push_env[@]}" COMMIT_MESSAGE=$'Merge pull request #56 from sebingel/feat/c\n\nadds c')

expect "only a pull request with another base than main has release:minor" \
  "bump=minor version=v0.17.0 release=true publish=true pull_request=56 pull_requests=52,59,56" \
  "${stack_env[@]}" STUB_PULLS="$(stack_pulls '[]' '["release:minor"]' '[]')"
if [ "$(get reason)" = "labels of the stack merge of pull requests #52, #59, #56" ]; then
  report "and names all pull requests of the stack as the reason" ""
else
  report "and names all pull requests of the stack as the reason" "reason=$(get reason)"
fi
expect_api_calls "reads the top pull request and one list per branch below it" 3
if grep -qxF "gh api repos/sebingel/gitlab-migrator/pulls?state=closed&head=sebingel%3Aci%2Fb&per_page=100" "$work/gh.log"; then
  report "lists the closed pull requests from the base branch of #56" ""
else
  report "lists the closed pull requests from the base branch of #56" "gh.log: $(cat "$work/gh.log")"
fi

expect "release:major of one pull request wins over release:minor of another" \
  "bump=major version=v1.0.0 release=true publish=true" \
  "${stack_env[@]}" STUB_PULLS="$(stack_pulls '["release:major"]' '[]' '["release:minor"]')"

expect "stack without release labels is a patch release" \
  "bump=patch version=v0.16.1 release=true publish=true pull_requests=52,59,56" \
  "${stack_env[@]}" STUB_PULLS="$(stack_pulls '[]' '[]' '[]')"

# Merged one by one, the pull requests without release:skip make releases
# and the one with it makes none. So its labels do not count.
expect "release:skip on one pull request neither skips the release nor counts its labels" \
  "bump=minor version=v0.17.0 release=true publish=true" \
  "${stack_env[@]}" STUB_PULLS="$(stack_pulls '[]' '["release:skip","release:major"]' '["release:minor"]')"

expect "release:skip on every pull request skips the release" \
  "bump=major release=false publish=false pull_requests=52,59,56" \
  "${stack_env[@]}" STUB_PULLS="$(stack_pulls '["release:skip"]' '["release:skip","release:major"]' '["release:skip"]')"
if [ "$(get reason)" = "label release:skip" ]; then
  report "and gives the label as the reason" ""
else
  report "and gives the label as the reason" "reason=$(get reason)"
fi

run plan "${stack_env[@]}" SUMMARY_FILE="$work/summary" \
  STUB_PULLS="$(stack_pulls '[]' '["release:minor"]' '[]')"
if grep -qxF "| Pull request | #52, #59, #56 |" "$work/summary"; then
  report "the job summary lists all pull requests of the stack" ""
else
  report "the job summary lists all pull requests of the stack" "summary: $(cat "$work/summary")"
fi

expect "a pull request below that is not merged breaks the chain" \
  "bump=patch version=v0.16.1 release=true publish=true pull_request= pull_requests=" \
  "${stack_env[@]}" STUB_PULLS="$(jq -sc . << PULLS
$(pull 52 "$sha" '[]' main true "" chore/a)
$(pull 59 "$sha" '["release:minor"]' chore/a false "" ci/b)
$(pull 56 "$sha" '[]' ci/b true "" feat/c)
PULLS
)"
expect_message "and names the branch without a merged pull request" \
  "Pull request #56 is based on ci/b, but no merged pull request"
expect_message "and warns that no labels apply" "not the merge commit of a pull request"

expect "a stack into another branch than main" \
  "bump=patch release=true publish=true pull_request= pull_requests=" \
  "${stack_env[@]}" STUB_PULLS="$(jq -sc . << PULLS
$(pull 52 "$sha" '["release:minor"]' develop true "" chore/a)
$(pull 59 "$sha" '["release:minor"]' chore/a true "" ci/b)
$(pull 56 "$sha" '[]' ci/b true "" feat/c)
PULLS
)"
expect_message "warns that no labels apply" "not the merge commit of a pull request"

expect_error "list API errors that do not stop fail the run" \
  "Could not read the pull requests from ci/b" plan \
  "${stack_env[@]}" STUB_LIST_STATUS=502 STUB_PULLS="$(stack_pulls '[]' '[]' '[]')"
expect_api_calls "the list call is retried too" 4

# api_get turns HTTP 404 into an empty answer. For the list this means that
# no pull request from the branch is found, so the chain breaks.
expect "list answer HTTP 404 breaks the chain" \
  "bump=patch version=v0.16.1 release=true publish=true pull_request= pull_requests=" \
  "${stack_env[@]}" STUB_LIST_STATUS=404 STUB_PULLS="$(stack_pulls '[]' '["release:minor"]' '[]')"
expect_message "and names the branch without a merged pull request" \
  "Pull request #56 is based on ci/b, but no merged pull request"
expect_message "and warns that no labels apply" "not the merge commit of a pull request"

expect_error "list answer that is not a JSON array" "is not a JSON array" plan \
  "${stack_env[@]}" STUB_LIST='{}' STUB_PULLS="$(stack_pulls '[]' '[]' '[]')"

expect_error "a chain that comes back to a pull request stops" \
  "Pull request #56 is twice" plan \
  "${stack_env[@]}" STUB_PULLS="$(jq -sc . << PULLS
$(pull 59 "$sha" '[]' feat/c true "" ci/b)
$(pull 56 "$sha" '[]' ci/b true "" feat/c)
PULLS
)"

echo "--- manual run"

expect "manual minor release on main" \
  "bump=minor version=v0.17.0 release=true publish=true build_version=v0.17.0" \
  "${dispatch_env[@]}" BUMP=minor
expect_api_calls "manual run makes no API call" 0

expect "manual major release" \
  "bump=major version=v1.0.0 publish=true" \
  "${dispatch_env[@]}" BUMP=major

expect_error "manual run outside main" "only made from main" plan \
  "${dispatch_env[@]}" REF=refs/heads/feature BUMP=patch

expect_error "manual run without new commits still fails (finding 3, issue 98)" \
  "already released as v0.16.0" plan \
  "${dispatch_env[@]}" SHA_TAGS=v0.16.0 BUMP=patch

expect_error "invalid manual bump" "expected patch, minor or major" plan \
  "${dispatch_env[@]}" BUMP=huge

expect_error "missing manual bump" "expected patch, minor or major" plan \
  "${dispatch_env[@]}"

echo "--- other input"

expect_error "unknown event" "Unexpected event" plan EVENT=schedule REF="$main"
expect_error "missing event" "EVENT" plan REF="$main"
expect_error "unknown mode" "Unknown mode" nonsense EVENT=push

echo "--- verify (publish job)"

run verify TAGS="$tags" PREVIOUS=v0.16.0 VERSION=v0.16.1
if [ "$status" -eq 0 ]; then report "plan is still current" ""; else report "plan is still current" "exit status $status"; fi

expect_error "a newer release was made after the plan" \
  "planned after v0.16.0, but the latest tag is now v0.17.0" verify \
  TAGS=$'v0.16.0\nv0.17.0' PREVIOUS=v0.16.0 VERSION=v0.16.1

expect_error "the planned version exists already" "latest tag is now v0.16.1" verify \
  TAGS=$'v0.16.0\nv0.16.1' PREVIOUS=v0.16.0 VERSION=v0.16.1

run verify TAGS= PREVIOUS= VERSION=v0.0.1
if [ "$status" -eq 0 ]; then report "first release without any tag" ""; else report "first release without any tag" "exit status $status"; fi

expect_error "tags appeared after a plan without tags" "latest tag is now v0.16.0" verify \
  TAGS="$tags" PREVIOUS= VERSION=v0.0.1

if [ "$failures" -gt 0 ]; then
  echo "$failures test(s) failed"
  exit 1
fi
echo "all tests passed"
