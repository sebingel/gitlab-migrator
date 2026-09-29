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

# The gh stub answers "gh api repos/<repo>/pulls/<n>" and logs every call.
#   STUB_PULL      JSON that it prints on success
#   STUB_STATUS    404 makes it fail like gh does for HTTP 404
#   STUB_FAILURES  number of calls that fail with HTTP 502 first
mkdir -p "$work/bin"
cat > "$work/bin/gh" << 'EOF'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_DIR/gh.log"
count=$(($(cat "$STUB_DIR/gh.count" 2> /dev/null || echo 0) + 1))
echo "$count" > "$STUB_DIR/gh.count"
if [ "$count" -le "${STUB_FAILURES:-0}" ]; then
  echo "gh: Server Error (HTTP 502)" >&2
  exit 1
fi
if [ "${STUB_STATUS:-200}" = "404" ]; then
  echo '{"message":"Not Found","status":"404"}'
  echo "gh: Not Found (HTTP 404)" >&2
  exit 1
fi
printf '%s\n' "$STUB_PULL"
EOF
chmod +x "$work/bin/gh"

# run MODE [VAR=value ...] runs release-plan.sh with only these variables.
# It sets $status, $out (all messages) and $result (the key=value lines).
run() {
  local mode="$1"
  shift
  rm -f -- "$work/gh.log" "$work/gh.count" "$work/result"
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

# pull NUMBER MERGE_COMMIT_SHA LABELS [BASE_REF] prints a pull request as the
# API returns it. LABELS is a JSON array of label names.
pull() {
  jq -cn --argjson n "$1" --arg sha "$2" --argjson labels "$3" --arg base "${4:-main}" \
    '{number: $n, merged: true, merge_commit_sha: $sha,
      base: {ref: $base, repo: {full_name: "sebingel/gitlab-migrator"}},
      labels: ($labels | map({name: .}))}'
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

echo "--- push to main"

expect "merge of a pull request without labels" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=true publish=true build_version=v0.16.1 pull_request=41" \
  "${push_env[@]}" STUB_PULL="$(pull 41 "$sha" '[]')"
expect_api_calls "reads only pull request #41 (finding 5)" 1
if grep -qxF "gh api repos/sebingel/gitlab-migrator/pulls/41" "$work/gh.log"; then
  report "API path is repos/<repo>/pulls/41" ""
else
  report "API path is repos/<repo>/pulls/41" "gh.log: $(cat "$work/gh.log")"
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

expect "direct push without a pull request (finding 6)" \
  "bump=patch version=v0.16.1 release=true publish=true pull_request=" \
  "${push_env[@]}" COMMIT_MESSAGE=$'fixes a typo\n\nsome text'
expect_api_calls "direct push makes no API call" 0
expect_message "warns about the direct push" "not the merge commit of a pull request"

expect "a title that ends with an issue number is a direct push (finding 6)" \
  "release=true publish=true pull_request=" \
  "${push_env[@]}" COMMIT_MESSAGE='fixes the retry (#12)'
expect_api_calls "issue number makes no API call" 0

expect_error "commit that already has a release tag (finding 3)" \
  "already released as v0.16.0" plan \
  "${push_env[@]}" SHA_TAGS=v0.16.0 STUB_PULL="$(pull 41 "$sha" '[]')"

expect "other tags on the commit do not count as a release" \
  "version=v0.16.1 publish=true" \
  "${push_env[@]}" SHA_TAGS=$'latest\nv0.17.0-rc.1' STUB_PULL="$(pull 41 "$sha" '[]')"

expect_error "commit that does not contain the previous release" \
  "does not contain the previous release v0.16.0" plan \
  "${push_env[@]}" MERGED_TAGS=$'v0.9.0\nv0.15.1' STUB_PULL="$(pull 41 "$sha" '[]')"

expect "first release without any tag" \
  "previous= version=v0.0.1 publish=true" \
  "${push_env[@]}" TAGS= MERGED_TAGS= STUB_PULL="$(pull 41 "$sha" '[]')"

expect_error "push outside main" "only made from main" plan \
  "${push_env[@]}" REF=refs/heads/feature STUB_PULL="$(pull 41 "$sha" '[]')"

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

expect_error "manual run without new commits (finding 3)" \
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
