#!/usr/bin/env bash
#
# Tests for go-toolchain.sh. Run it from anywhere:
#
#   bash .github/scripts/go-toolchain_test.sh
#
# The tests replace gh and go with stubs, so they need no network, no token
# and no Go. They need bash, jq and the usual POSIX tools.

set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/go-toolchain.sh"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
failures=0

# The gh stub answers the API calls of the plan and logs every call.
#   STUB_PULLS        JSON for "pulls?state=all&head=..." (default: [])
#   STUB_PULLS_HEAD   if set, STUB_PULLS only answers for a head that
#                     contains this text, all other heads get []
#   STUB_REF          "exists" if the branch exists (default: HTTP 404)
#   STUB_BRANCH_GOMOD go.mod on the branch for "contents/go.mod?ref=..."
#                     (default: HTTP 404)
#   STUB_FAILURES     number of calls that fail first
#   STUB_ERROR        the message of these failures
#                     (default: gh: Server Error (HTTP 502))
mkdir -p "$work/bin"
cat > "$work/bin/gh" << 'EOF'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_DIR/gh.log"
count=$(($(cat "$STUB_DIR/gh.count" 2> /dev/null || echo 0) + 1))
echo "$count" > "$STUB_DIR/gh.count"
if [ "$count" -le "${STUB_FAILURES:-0}" ]; then
  echo "${STUB_ERROR:-gh: Server Error (HTTP 502)}" >&2
  exit 1
fi
not_found() {
  echo '{"message":"Not Found","status":"404"}'
  echo "gh: Not Found (HTTP 404)" >&2
  exit 1
}
case "$2" in
  */pulls\?*)
    if [ -n "${STUB_PULLS_HEAD:-}" ] && [[ "$2" != *"$STUB_PULLS_HEAD"* ]]; then
      echo '[]'
    else
      printf '%s\n' "${STUB_PULLS:-[]}"
    fi
    ;;
  */git/ref/heads/*)
    if [ "${STUB_REF:-}" = "exists" ]; then
      printf '{"ref":"refs/heads/%s"}\n' "${2#*/git/ref/heads/}"
    else
      not_found
    fi
    ;;
  */contents/go.mod\?ref=*)
    if [ -z "${STUB_BRANCH_GOMOD:-}" ]; then
      not_found
    fi
    # Like the API: base64 in lines of 60 characters.
    jq -cn --arg c "$(printf '%s\n' "$STUB_BRANCH_GOMOD" | base64 | tr -d '\n' | fold -w 60)" \
      '{encoding: "base64", content: $c}'
    ;;
  *)
    echo "gh stub: unexpected call $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$work/bin/gh"

# The go stub changes go.mod in the current directory like
# "go get toolchain@VERSION" does, and logs every call.
#   STUB_GO=bump-go  also raises the go line (go get must never do that here)
#   STUB_GO=noop     changes nothing
#   STUB_GO=fail     fails like go get does for an unknown version
cat > "$work/bin/go" << 'EOF'
#!/usr/bin/env bash
echo "go $*" >> "$STUB_DIR/go.log"
case "$*" in
  "get toolchain@"*)
    version="${2#toolchain@}"
    case "${STUB_GO:-}" in
      noop) exit 0 ;;
      fail)
        echo "go: toolchain@$version: invalid toolchain" >&2
        exit 1
        ;;
    esac
    if grep -q '^toolchain ' go.mod; then
      sed "s/^toolchain .*/toolchain $version/" go.mod > go.mod.new
    else
      awk -v v="$version" '{ print } /^go / { print ""; print "toolchain " v }' go.mod > go.mod.new
    fi
    mv go.mod.new go.mod
    if [ "${STUB_GO:-}" = "bump-go" ]; then
      sed "s/^go .*/go ${version#go}/" go.mod > go.mod.new
      mv go.mod.new go.mod
    fi
    ;;
  "mod tidy") ;;
  *)
    echo "go stub: unexpected call $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$work/bin/go"

# releases VERSION:STABLE ... prints a go.dev release list, for example
# releases go1.27.1:true go1.28rc1:false.
releases() {
  local item
  for item in "$@"; do
    printf '{"version":"%s","stable":%s,"files":[]}\n' "${item%%:*}" "${item##*:}"
  done | jq -cs .
}

# gomod LINES... writes a go.mod with these lines (after the module line) to
# $work/go.mod.
gomod() {
  {
    echo "module github.com/sebingel/gitlab-migrator"
    echo
    printf '%s\n' "$@"
    echo
    echo "require github.com/hashicorp/go-cleanhttp v0.5.2"
  } > "$work/go.mod"
}

# run MODE [VAR=value ...] runs go-toolchain.sh with only these variables.
# It sets $status, $out (all messages) and $result (the key=value lines).
# apply runs in $work/mod, which has a copy of $work/go.mod.
run() {
  local mode="$1"
  shift
  rm -rf -- "$work/gh.log" "$work/gh.count" "$work/go.log" "$work/result" "$work/mod"
  mkdir -p "$work/mod"
  cp "$work/go.mod" "$work/mod/go.mod"
  status=0
  out="$(cd "$work/mod" && env -i PATH="$work/bin:$PATH" STUB_DIR="$work" API_RETRY_DELAY=0 \
    RESULT_FILE="$work/result" GO_MOD="$work/go.mod" RELEASES_FILE="$work/releases.json" \
    REPO=sebingel/gitlab-migrator "$@" bash "$script" "$mode" 2>&1)" || status=$?
  result="$(cat "$work/result" 2> /dev/null || true)"
}

get() {
  sed -n "s/^$1=//p" <<< "$result"
}

calls() {
  if [ -f "$work/$1.log" ]; then
    wc -l < "$work/$1.log" | tr -d ' '
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
# Values cannot contain spaces.
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

# The next three check the last run.
expect_message() {
  if grep -qF -- "$2" <<< "$out"; then
    report "$1" ""
  else
    report "$1" "output does not contain '$2'"
  fi
}

expect_calls() {
  local got
  got="$(calls "$2")"
  if [ "$got" = "$3" ]; then
    report "$1" ""
  else
    report "$1" "want $3 $2 call(s), got $got"
  fi
}

expect_logged() {
  if grep -qF -- "$3" "$work/$2.log" 2> /dev/null; then
    report "$1" ""
  else
    report "$1" "the $2 calls do not contain '$3'"
  fi
}

releases go1.27.1:true go1.26.8:true > "$work/releases.json"

echo "--- no update"

gomod "go 1.25.0" "toolchain go1.27.1"
expect "the toolchain is the newest release" \
  "current=go1.27.1 latest=go1.27.1 target= update=false kind= branch= action=none"
expect_calls "no API call without an update" gh 0
expect_message "the reason says it is current" "go1.27.1 is the newest Go release"

gomod "go 1.25.0" "toolchain go1.28rc1"
expect "a release candidate newer than every release" \
  "current=go1.28rc1 latest=go1.27.1 target= update=false action=none"
expect_message "the reason says it is newer" "go1.28rc1 is newer than the newest stable Go release go1.27.1"

echo "--- updates"

gomod "go 1.25.0" "toolchain go1.27.0"
expect "a patch release" \
  "current=go1.27.0 latest=go1.27.1 target=go1.27.1 update=true kind=patch setup_version=1.27.1 branch=go-toolchain/go1.27.1 action=push-and-open pull_request="
expect_calls "the plan reads pull requests and the branch" gh 2
expect_logged "only pull requests of this repository into main count" gh \
  "gh api repos/sebingel/gitlab-migrator/pulls?state=all&base=main&head=sebingel:go-toolchain/go1.27.1"
expect_logged "the branch lookup is exact" gh \
  "gh api repos/sebingel/gitlab-migrator/git/ref/heads/go-toolchain/go1.27.1"

gomod "go 1.25.0" "toolchain go1.26.8"
expect "a new minor release" "current=go1.26.8 latest=go1.27.1 target=go1.27.1 update=true kind=minor action=push-and-open"

gomod "go 1.25.0"
expect "no toolchain line: the go line selects the toolchain" \
  "current=go1.25.0 latest=go1.27.1 target=go1.27.1 update=true kind=minor"

gomod "go 1.25"
expect "a go line with a language version" "current=go1.25 target=go1.27.1 update=true kind=minor"

gomod "go 1.25.0 // minimum for building from source" "toolchain go1.27.0 // newest"
expect "comments after the lines" "current=go1.27.0 update=true kind=patch"

gomod "go 1.25.0" "godebug default=go1.21" "toolchain go1.27.0"
expect "a godebug line is not the go line" "current=go1.27.0 update=true"

gomod "go 1.25.0" "toolchain go1.27rc2"
releases go1.27.0:true go1.26.8:true > "$work/releases.json"
expect "from a release candidate to its release is a minor update" \
  "current=go1.27rc2 latest=go1.27.0 target=go1.27.0 update=true kind=minor"

gomod "go 1.25.0" "toolchain go1.27.9"
releases go1.27.10:true go1.26.12:true > "$work/releases.json"
expect "two digit patch numbers" "latest=go1.27.10 target=go1.27.10 update=true kind=patch"

gomod "go 1.9.0" "toolchain go1.9.7"
releases go1.10.0:true go1.9.7:true > "$work/releases.json"
expect "two digit minor numbers" "latest=go1.10.0 target=go1.10.0 update=true kind=minor"

gomod "go 1.25.0" "toolchain go1.27.0"
releases go1.26.8:true go1.28rc1:false go1.27.1:true go1.27.2rc1:true > "$work/releases.json"
expect "unsorted list, release candidates are ignored" "latest=go1.27.1 target=go1.27.1 update=true"

printf 'module github.com/sebingel/gitlab-migrator\r\n\r\ngo 1.25.0\r\n\r\ntoolchain go1.27.0\r\n' > "$work/go.mod"
expect "go.mod with CRLF line ends" "current=go1.27.0 latest=go1.27.1 target=go1.27.1 update=true"

gomod "go 1.27.1" "toolchain go1.26.8"
expect "a go line newer than the toolchain line selects the go line" \
  "current=go1.27.1 target= update=false action=none"

echo "--- patch releases first"

releases go1.28.1:true go1.27.4:true > "$work/releases.json"

gomod "go 1.25.0" "toolchain go1.27.3"
expect "a patch release comes before a newer minor release" \
  "current=go1.27.3 latest=go1.28.1 target=go1.27.4 kind=patch branch=go-toolchain/go1.27.4 action=push-and-open"

gomod "go 1.25.0" "toolchain go1.27.4"
expect "the newest patch release: then the minor release" \
  "current=go1.27.4 latest=go1.28.1 target=go1.28.1 kind=minor branch=go-toolchain/go1.28.1 action=push-and-open"

gomod "go 1.25.0" "toolchain go1.25.0"
expect "an unsupported minor release: the newest release" "target=go1.28.1 kind=minor"

gomod "go 1.25.0" "toolchain go1.27.3"
expect "an open minor pull request does not block a patch release" \
  "target=go1.27.4 kind=patch action=push-and-open pull_request=" \
  STUB_PULLS_HEAD=go1.28.1 STUB_PULLS='[{"number":19,"state":"open","merged_at":null}]'

expect "an open patch pull request: wait for it" \
  "update=true target= action=none pull_request=20" \
  STUB_PULLS_HEAD=go1.27.4 STUB_PULLS='[{"number":20,"state":"open","merged_at":null}]'
expect_message "the reason names the open pull request" "pull request #20 for go1.27.4 is open"
expect_calls "no lookup of the minor release while the patch waits" gh 1

expect "a closed patch pull request: the newest release" \
  "update=true target=go1.28.1 kind=minor branch=go-toolchain/go1.28.1 action=push-and-open pull_request=" \
  STUB_PULLS_HEAD=go1.27.4 STUB_PULLS='[{"number":21,"state":"closed","merged_at":null}]'
expect_message "the reason names the skipped version" "skipped: go1.27.4 (#21 closed)"
expect_calls "two pull request lookups and one branch lookup" gh 3

expect "all candidates are closed" "update=true target= action=none pull_request=22" \
  STUB_PULLS='[{"number":22,"state":"closed","merged_at":null}]'
expect_message "the reason lists both" "closed: go1.27.4 (#22 closed), go1.28.1 (#22 closed)"

expect "an open pull request wins over a closed one of the same branch" \
  "target= action=none pull_request=31" \
  STUB_PULLS='[{"number":30,"state":"closed","merged_at":null},{"number":31,"state":"open","merged_at":null}]'
expect_message "the reason names the open one" "pull request #31 for go1.27.4 is open"

gomod "go 1.28.0" "toolchain go1.27.3"
expect "a go line newer than the toolchain line: its patch release" \
  "current=go1.28.0 target=go1.28.1 kind=patch"

echo "--- existing branches and pull requests"

releases go1.27.1:true go1.26.8:true > "$work/releases.json"
gomod "go 1.25.0" "toolchain go1.27.0"

expect "an open pull request exists" "update=true target= action=none pull_request=12" \
  STUB_PULLS='[{"number":12,"state":"open","merged_at":null}]'
expect_message "the reason names the open pull request" "pull request #12 for go1.27.1 is open"
expect_calls "no branch lookup when a pull request exists" gh 1

expect "a closed pull request is not proposed again" "update=true target= action=none pull_request=13" \
  STUB_PULLS='[{"number":13,"state":"closed","merged_at":null}]'
expect_message "the reason says closed" "go1.27.1 (#13 closed)"

expect "a merged pull request" "target= action=none pull_request=14" \
  STUB_PULLS='[{"number":14,"state":"closed","merged_at":"2026-09-01T10:00:00Z"}]'
expect_message "the reason says merged" "go1.27.1 (#14 merged)"
expect_message "a merged newer version means a downgrade: warn" \
  "::warning::Pull request #14 for go1.27.1 was merged, but go.mod selects go1.27.0 now."

expect "the branch exists without a pull request" "update=true target=go1.27.1 action=open pull_request=" \
  STUB_REF=exists STUB_BRANCH_GOMOD=$'module x\n\ngo 1.25.0\n\ntoolchain go1.27.1'
expect_message "the reason names the branch" "the branch go-toolchain/go1.27.1 exists without a pull request"
expect_calls "the plan reads go.mod of the branch" gh 3
expect_logged "the plan reads go.mod of this branch" gh \
  "gh api repos/sebingel/gitlab-migrator/contents/go.mod?ref=go-toolchain/go1.27.1"

expect_error "the branch has another toolchain" \
  "its go.mod selects go1.27.0, not go1.27.1. Delete the branch" plan \
  STUB_REF=exists STUB_BRANCH_GOMOD=$'module x\n\ngo 1.25.0\n\ntoolchain go1.27.0'
expect_error "the branch has no go.mod" "Could not read go.mod of the branch go-toolchain/go1.27.1" plan \
  STUB_REF=exists
expect_error "the branch has another go line" "its go line is '1.27.1', not '1.25.0'" plan \
  STUB_REF=exists STUB_BRANCH_GOMOD=$'module x\n\ngo 1.27.1\n\ntoolchain go1.27.1'
run plan STUB_PULLS='[{"number":13,"state":"closed","merged_at":null}]'
if grep -qF "::warning::" <<< "$out"; then
  report "a closed pull request gives no warning" "unexpected warning"
else
  report "a closed pull request gives no warning" ""
fi

echo "--- API errors"

expect "an API error is retried" "action=push-and-open" STUB_FAILURES=1
expect_calls "one failed call and two good ones" gh 3

expect_error "the API keeps failing" "Could not read repos/sebingel/gitlab-migrator/pulls" plan STUB_FAILURES=9

expect "a rate limit is retried" "action=push-and-open" \
  STUB_FAILURES=1 STUB_ERROR='gh: API rate limit exceeded for installation (HTTP 403)'
expect_calls "one rate limited call and two good ones" gh 3

expect_error "bad credentials are not retried" "Bad credentials (HTTP 401)" plan \
  STUB_FAILURES=9 STUB_ERROR='gh: Bad credentials (HTTP 401)'
expect_calls "only one call with bad credentials" gh 1
expect_error "a missing permission is not retried" "Resource not accessible by integration (HTTP 403)" plan \
  STUB_FAILURES=9 STUB_ERROR='gh: Resource not accessible by integration (HTTP 403)'
expect_calls "only one call without permission" gh 1

expect "an error without HTTP status is retried" "action=push-and-open" \
  STUB_FAILURES=1 STUB_ERROR='Post "https://api.github.com/graphql": dial tcp: i/o timeout'
expect_error "the API returns no list" "no list of pull requests" plan STUB_PULLS='{"message":"Bad credentials"}'

echo "--- bad input"

gomod "go 1.25.0" "toolchain default"
expect_error "toolchain default" "This script only handles Go release names" plan
gomod "go 1.25.0" "toolchain go1.27.1-custom"
expect_error "a custom toolchain name" "'go1.27.1-custom'" plan
gomod "go 1.25.0" "toolchain go1.27.0" "toolchain go1.27.1"
expect_error "two toolchain lines" "more than one toolchain line" plan
gomod "go 1.25.0" "  toolchain go1.27.0"
expect_error "an indented toolchain line" "has an indented toolchain line" plan
gomod "	go 1.25.0" "toolchain go1.27.0"
expect_error "an indented go line" "has an indented go line" plan
gomod "toolchain go1.27.0"
expect_error "only a toolchain line: go get would add a go line" "has no go line" plan
gomod "// no go line"
expect_error "neither a go nor a toolchain line" "neither a toolchain nor a go line" plan
gomod "go one"
expect_error "a go line that is no version" "'one', which is not a Go version" plan

gomod "go 1.25.0" "toolchain go1.27.0"
echo '<html>rate limited</html>' > "$work/releases.json"
expect_error "the release list is no JSON" "is not the JSON list of Go releases" plan
releases go1.28rc1:false > "$work/releases.json"
expect_error "the release list has no stable release" "lists no stable Go release" plan
expect_error "missing release list" "does not exist" plan RELEASES_FILE="$work/nothing.json"
expect_error "missing go.mod" "does not exist" plan GO_MOD="$work/nothing.mod"
expect_error "missing REPO" "REPO" plan REPO=
expect_error "unknown mode" "Unknown mode" nonsense

echo "--- summary"

releases go1.27.1:true go1.26.8:true > "$work/releases.json"
rm -f "$work/summary.md"
run plan SUMMARY_FILE="$work/summary.md"
# shellcheck disable=SC2016 # the backticks are Markdown, not a command
if grep -qF '| Newest Go release | `go1.27.1` |' "$work/summary.md" 2> /dev/null; then
  report "the summary shows the newest release" ""
else
  report "the summary shows the newest release" "summary: $(cat "$work/summary.md" 2> /dev/null || echo missing)"
fi

echo "--- apply"

gomod "go 1.25.0" "toolchain go1.27.1"
run apply VERSION=go1.27.2
if [ "$status" -eq 0 ] && grep -qx 'toolchain go1.27.2' "$work/mod/go.mod" && grep -qx 'go 1.25.0' "$work/mod/go.mod"; then
  report "apply updates the toolchain line" ""
else
  report "apply updates the toolchain line" "exit status $status, go.mod: $(tr '\n' '|' < "$work/mod/go.mod")"
fi
expect_logged "apply runs go get" go "go get toolchain@go1.27.2"
expect_logged "apply runs go mod tidy" go "go mod tidy"

gomod "go 1.25.0"
run apply VERSION=go1.27.2
if [ "$status" -eq 0 ] && grep -qx 'toolchain go1.27.2' "$work/mod/go.mod"; then
  report "apply adds a missing toolchain line" ""
else
  report "apply adds a missing toolchain line" "exit status $status, go.mod: $(tr '\n' '|' < "$work/mod/go.mod")"
fi

gomod "go 1.25.0" "toolchain go1.27.1"
expect_error "apply stops if the go line changes" "The go line changed from '1.25.0' to '1.27.2'" apply \
  VERSION=go1.27.2 STUB_GO=bump-go
expect_error "apply stops if go.mod did not change" "selects go1.27.1 after the update, expected go1.27.2" apply \
  VERSION=go1.27.2 STUB_GO=noop
expect_error "apply stops if go get fails" "invalid toolchain" apply VERSION=go1.27.2 STUB_GO=fail
expect_error "apply needs a release name" "is not a Go release like go1.27.2" apply VERSION=1.27.2
expect_error "apply refuses a release candidate" "is not a Go release like go1.27.2" apply VERSION=go1.28rc1
expect_error "apply needs VERSION" "VERSION is required" apply

echo "--- texts"

# texts_run NAME [VAR=value ...] runs texts mode into a fresh $work/texts.
texts_run() {
  rm -rf -- "$work/texts"
  mkdir -p "$work/texts"
  run texts TEXT_DIR="$work/texts" CURRENT=go1.27.1 "$@"
  body="$(cat "$work/texts/pr-body.md" 2> /dev/null || true)"
  commit="$(cat "$work/texts/commit-message.txt" 2> /dev/null || true)"
  title="$(cat "$work/texts/pr-title.txt" 2> /dev/null || true)"
}

# has NAME TEXT CONTENT: CONTENT must contain TEXT. lacks: must not.
has() {
  if grep -qF -- "$2" <<< "$3"; then report "$1" ""; else report "$1" "missing '$2'"; fi
}
lacks() {
  if grep -qF -- "$2" <<< "$3"; then report "$1" "unexpected '$2'"; else report "$1" ""; fi
}

texts_run TARGET=go1.27.2 LATEST=go1.27.2 KIND=patch HAS_TOKEN=false RUN_URL=https://example.test/run/1
if [ "$status" -eq 0 ]; then report "texts for a patch release" ""; else report "texts for a patch release" "exit status $status"; fi
has "the title names the target" "updates the Go toolchain to go1.27.2" "$title"
has "the commit message starts with the title" "updates the Go toolchain to go1.27.2" "$(head -n 1 <<< "$commit")"
# shellcheck disable=SC2016 # the backticks are Markdown, not a command
has "the body names both versions" 'from `go1.27.1` to `go1.27.2`' "$body"
has "a patch links its release notes" "* Release notes: https://go.dev/doc/devel/release#go1.27.2" "$body"
lacks "a patch has no minor checklist" "is a new minor release" "$body"
lacks "no note about a newer release" "is newer." "$body"
has "without a token the body asks for the approval" "Approve workflows to run" "$body"
has "the body links the run" "(https://example.test/run/1)" "$body"

texts_run TARGET=go1.28.0 LATEST=go1.28.0 KIND=minor HAS_TOKEN=true
has "a minor links the Go 1.x release notes" "https://go.dev/doc/go1.28 and https://go.dev/doc/devel/release#go1.28.0" "$body"
has "a minor has the golangci-lint check" "set \`version\` in \`.github/workflows/lint.yaml\`" "$body"
has "a minor has the HTTP/2 check" "isTransientNetworkError" "$body"
has "a minor has the ports check" "The Ports section of the release notes" "$body"
has "the body warns about a downgrade" "merging them would go back to an older toolchain" "$body"
lacks "with a token there is no approval note" "Approve workflows to run" "$body"

texts_run TARGET=go1.27.4 LATEST=go1.28.1 KIND=patch
has "a patch before a newer release says so" "go1.28.1 is newer. Patch releases of the current Go release come first" "$body"
lacks "and promises nothing about it" "after this pull request is merged" "$body"

for marker in '[skip ci]' '[ci skip]' '[no ci]' '[skip actions]' '[actions skip]' 'skip-checks'; do
  lacks "no $marker in the texts" "$marker" "$title$commit$body"
done

expect_error "texts need a release as TARGET" "TARGET 'go1.28rc1' is not a Go release" texts \
  TEXT_DIR="$work" CURRENT=go1.27.1 TARGET=go1.28rc1 LATEST=go1.28rc1 KIND=minor
expect_error "texts need patch or minor" "KIND 'major' is not patch or minor" texts \
  TEXT_DIR="$work" CURRENT=go1.27.1 TARGET=go1.28.0 LATEST=go1.28.0 KIND=major
expect_error "texts need TEXT_DIR" "TEXT_DIR is required" texts \
  CURRENT=go1.27.1 TARGET=go1.28.0 LATEST=go1.28.0 KIND=minor
expect_error "texts need an existing TEXT_DIR" "does not exist" texts TEXT_DIR="$work/nothing" \
  CURRENT=go1.27.1 TARGET=go1.28.0 LATEST=go1.28.0 KIND=minor

if [ "$failures" -gt 0 ]; then
  echo "$failures test(s) failed"
  exit 1
fi
echo "all tests passed"
