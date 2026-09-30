#!/usr/bin/env bash
#
# Keeps the toolchain line of go.mod on a supported Go release. The Go
# toolchain workflow collects the facts and this script makes every decision,
# so that go-toolchain_test.sh can test all of it.
#
# Usage:
#
#   go-toolchain.sh plan    decide if go.mod needs a newer toolchain, and
#                           what the workflow has to do for it
#   go-toolchain.sh apply   update the toolchain line of go.mod in the
#                           current directory to VERSION
#   go-toolchain.sh texts   write the commit message, the title and the body
#                           of the pull request for TARGET
#
# All input comes from environment variables.
#
# plan:
#   GO_MOD          path of go.mod (default: go.mod)
#   RELEASES_FILE   Go releases as JSON, from https://go.dev/dl/?mode=json
#   REPO            this repository, owner/name
#   RESULT_FILE     file for the key=value results (default: stdout)
#   SUMMARY_FILE    file for a Markdown summary (optional)
#   API_RETRIES     calls to the API before giving up (3)
#   API_RETRY_DELAY seconds between these calls (10)
#
# apply:
#   VERSION         the new toolchain, for example go1.27.2
#
# texts (the keys of the plan, in capitals):
#   CURRENT, TARGET, LATEST, KIND
#   HAS_TOKEN       true if the secret GO_TOOLCHAIN_TOKEN exists
#   RUN_URL         link to the workflow run (optional)
#   TEXT_DIR        directory for commit-message.txt, pr-title.txt and
#                   pr-body.md
#
# plan writes these keys:
#   current        toolchain that go.mod selects now, for example go1.27.1
#   latest         newest stable Go release, for example go1.28.0
#   target         the release to propose, for example go1.27.2, empty if
#                  there is none
#   update         true if a release newer than current exists
#   kind           patch (same Go 1.x release as current) or minor (a newer
#                  Go 1.x release), empty without target
#   setup_version  target without "go", for actions/setup-go
#   branch         branch of the target, for example go-toolchain/go1.27.2
#   action         none, open (the branch exists, open the pull request) or
#                  push-and-open (push the branch, then open the pull request)
#   pull_request   number of the pull request that stopped the plan, if any
#   reason         a short text that explains the decision
#
# Rules:
#
# * The go line (the minimum Go version) never changes.
# * Patch releases come first: if a newer patch release of the current Go 1.x
#   release exists, it is the target, even if a newer Go 1.x release exists.
#   Security fixes then do not wait for a new minor release, which can need
#   more work (for example a newer golangci-lint).
# * Otherwise the target is the newest stable release, also when it is a new
#   Go 1.x release. Go supports each of them only until two newer ones exist.
# * There is one branch and one pull request per version. If the pull request
#   of a candidate is open, the plan waits for it. If it is closed or merged,
#   that version was rejected on purpose, and the plan tries the next
#   candidate: first the patch release, then the newest release.
#
# Needs: bash, jq, base64, gh (plan, only if there is an update), go (apply).

set -euo pipefail

branch_prefix="go-toolchain/"

# fail prints to stderr, so that a message from inside $(...) is not lost.
fail() {
  echo "::error::$*" >&2
  exit 1
}

result() {
  printf '%s\n' "$@" >> "${RESULT_FILE:-/dev/stdout}"
}

# version_key VERSION prints a key that sorts like Go versions when compared
# as text: go1.27 < go1.27rc1 < go1.27.0 < go1.27.1 < go1.28rc1. A version
# without a patch number (go1.27) is a language version, older than every
# release of it. VERSION may have the prefix "go".
version_key() {
  local v="${1#go}" patch=0 rank
  if [[ "$v" =~ ^([0-9]+)\.([0-9]+)$ ]]; then
    rank=0
  elif [[ "$v" =~ ^([0-9]+)\.([0-9]+)rc([0-9]+)$ ]]; then
    rank=$((10#${BASH_REMATCH[3]}))
  elif [[ "$v" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    patch=$((10#${BASH_REMATCH[3]}))
    rank=99999
  else
    fail "'$1' is not a Go version that this script understands."
  fi
  printf '%05d.%05d.%05d.%05d\n' "$((10#${BASH_REMATCH[1]}))" \
    "$((10#${BASH_REMATCH[2]}))" "$patch" "$rank"
}

# newer A B is true if Go version A is newer than Go version B.
newer() {
  local a b
  # newer runs in if conditions, where set -e does not apply.
  a="$(version_key "$1")" || exit 1
  b="$(version_key "$2")" || exit 1
  [[ "$a" > "$b" ]]
}

# minor_of VERSION prints the Go 1.x release of VERSION, for example go1.27.
minor_of() {
  [[ "${1#go}" =~ ^([0-9]+)\.([0-9]+) ]] || fail "'$1' is not a Go version."
  echo "go${BASH_REMATCH[1]}.${BASH_REMATCH[2]}"
}

# directive FILE NAME prints the value of the one NAME line of go.mod FILE,
# or nothing if there is no such line. Like actions/setup-go, it only reads
# lines that start with NAME. An indented NAME line stops the script, because
# Go would read it and setup-go would not.
directive() {
  local values
  if grep -Eq "^[[:space:]]+$2[[:space:]]" "$1"; then
    fail "$1 has an indented $2 line. Run \"go mod edit -fmt\" to format go.mod."
  fi
  # [:space:] also matches the CR of CRLF line ends.
  values="$(sed -n "s/^$2[[:space:]]\{1,\}\([^[:space:]]\{1,\}\).*/\1/p" "$1")"
  if [ "$(grep -c . <<< "$values")" -gt 1 ]; then
    fail "$1 has more than one $2 line."
  fi
  printf '%s' "$values"
}

# toolchain_of FILE prints the toolchain that go.mod FILE selects: the
# toolchain line, or the go line if that is newer or if there is no toolchain
# line. "go 1.25.0" means go1.25.0, and Go never runs a toolchain older than
# the go line (https://go.dev/doc/toolchain). A toolchain line like "default"
# or "go1.27.1-custom" stops the script, because it cannot compare it.
toolchain_of() {
  local toolchain go
  # This function runs inside $(...), where set -e does not apply.
  toolchain="$(directive "$1" toolchain)" || exit 1
  go="$(directive "$1" go)" || exit 1
  if [ -n "$toolchain" ] && ! [[ "$toolchain" =~ ^go[0-9]+\.[0-9]+(\.[0-9]+|rc[0-9]+)?$ ]]; then
    fail "The toolchain line of $1 is '$toolchain'. This script only handles Go release names like go1.27.1."
  fi
  if [ -n "$go" ] && ! [[ "$go" =~ ^[0-9]+\.[0-9]+(\.[0-9]+|rc[0-9]+)?$ ]]; then
    fail "The go line of $1 is '$go', which is not a Go version."
  fi
  if [ -z "$toolchain" ] && [ -z "$go" ]; then
    fail "$1 has neither a toolchain nor a go line."
  fi
  if [ -z "$toolchain" ] || { [ -n "$go" ] && newer "go$go" "$toolchain"; }; then
    echo "go$go"
  else
    echo "$toolchain"
  fi
}

# newest_release FILE [MINOR] prints the newest stable Go release of the
# go.dev JSON in FILE, or with MINOR (for example go1.27) the newest one of
# that Go 1.x release, or nothing if there is none. It ignores release
# candidates and anything that is not goX.Y.Z.
newest_release() {
  local versions version best=""
  # tr: jq on Windows ends lines with CRLF.
  if ! versions="$(jq -r '.[] | select(.stable == true) | .version' "$1" 2> /dev/null | tr -d '\r')"; then
    fail "$1 is not the JSON list of Go releases from go.dev."
  fi
  while IFS= read -r version; do
    [[ "$version" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
    if [ -n "${2:-}" ] && [ "$(minor_of "$version")" != "$2" ]; then
      continue
    fi
    if [ -z "$best" ] || newer "$version" "$best"; then
      best="$version"
    fi
  done <<< "$versions"
  echo "$best"
}

# api PATH FILE writes the answer of "gh api PATH" to FILE. FILE stays empty
# if PATH does not exist (HTTP 404). Temporary errors (HTTP 5xx, a rate limit,
# or no HTTP answer at all) are retried. Other errors, for example HTTP 401 or
# a missing permission, stop the script at once, because a retry cannot fix
# them.
api() {
  local path="$1" file="$2" retries="${API_RETRIES:-3}" attempt=1
  while true; do
    if gh api "$path" > "$file" 2> "$work/gh.err"; then
      return 0
    fi
    if grep -q "HTTP 404" "$work/gh.err"; then
      : > "$file"
      return 0
    fi
    if grep -q "HTTP 4[0-9][0-9]" "$work/gh.err" && ! grep -qi "HTTP 429\|rate limit" "$work/gh.err"; then
      fail "Calling the API ($path) failed: $(cat "$work/gh.err")"
    fi
    echo "Calling the API ($path) failed (attempt $attempt of $retries): $(cat "$work/gh.err")"
    if [ "$attempt" -ge "$retries" ]; then
      fail "Could not read $path from the API. Run the workflow again later."
    fi
    attempt=$((attempt + 1))
    sleep "${API_RETRY_DELAY:-10}"
  done
}

# find_pull BRANCH sets $pull_number and $pull_state (open, closed or merged)
# for the pull request of BRANCH into main, or empties them if there is none.
# An open pull request wins over closed ones. Only branches of this
# repository count, a branch with the same name in a fork is not ours.
find_pull() {
  local pick='[.[] | select(.state == "open")] + [.[] | select(.state != "open")] | .[0] // empty'
  api "repos/$REPO/pulls?state=all&base=main&head=${REPO%%/*}:$1" "$work/pulls.json"
  if ! jq -e 'type == "array"' "$work/pulls.json" > /dev/null 2>&1; then
    fail "The API returned no list of pull requests for $1."
  fi
  # -j prints no line end, so no CR on Windows either.
  pull_number="$(jq -j "$pick | .number" "$work/pulls.json")"
  pull_state="$(jq -j "$pick | if .merged_at then \"merged\" else .state end" "$work/pulls.json")"
}

# check_branch BRANCH VERSION GO stops the script unless go.mod on BRANCH
# selects VERSION and still has the go line GO. A branch without a pull
# request is left over from a run that pushed it but could not open the pull
# request. The plan only reuses it if it contains the right update.
check_branch() {
  local selects go
  api "repos/$REPO/contents/go.mod?ref=$1" "$work/branch-go.json"
  # The API wraps the base64 text. tr: jq on Windows ends lines with CRLF.
  if ! jq -j '.content // empty' "$work/branch-go.json" 2> /dev/null | tr -d '\r\n' \
    | base64 -d > "$work/branch-go.mod" 2> /dev/null || ! [ -s "$work/branch-go.mod" ]; then
    fail "Could not read go.mod of the branch $1. Delete the branch, the next run creates it again."
  fi
  selects="$(toolchain_of "$work/branch-go.mod")"
  if [ "$selects" != "$2" ]; then
    fail "The branch $1 exists without a pull request, but its go.mod selects $selects, not $2. Delete the branch, the next run creates it again."
  fi
  go="$(directive "$work/branch-go.mod" go)"
  if [ "$go" != "$3" ]; then
    fail "The branch $1 exists without a pull request, but its go line is '$go', not '$3'. Delete the branch, the next run creates it again."
  fi
}

plan() {
  local go_mod="${GO_MOD:-go.mod}" releases="${RELEASES_FILE:?RELEASES_FILE is required}"
  local current go_line latest patch candidate
  local target="" update=false kind="" setup_version="" branch="" action=none
  local pull_request="" reason="" skipped=""
  local -a candidates=()
  : "${REPO:?REPO is required}"

  [ -f "$go_mod" ] || fail "$go_mod does not exist."
  [ -f "$releases" ] || fail "$releases does not exist."

  current="$(toolchain_of "$go_mod")"
  # Without a go line, "go get toolchain@..." writes one, and apply stops
  # because only the toolchain line may change.
  go_line="$(directive "$go_mod" go)"
  [ -n "$go_line" ] || fail "$go_mod has no go line. Add one (the minimum Go version) first."
  latest="$(newest_release "$releases")"
  [ -n "$latest" ] || fail "$releases lists no stable Go release."
  # Empty if the Go 1.x release of current is not supported any more.
  patch="$(newest_release "$releases" "$(minor_of "$current")")"

  if [ -n "$patch" ] && newer "$patch" "$current"; then
    candidates+=("$patch")
  fi
  if newer "$latest" "$current" && [ "$latest" != "$patch" ]; then
    candidates+=("$latest")
  fi

  # Without candidates, current is latest or newer than it.
  if [ "${#candidates[@]}" -eq 0 ]; then
    if [ "$latest" = "$current" ]; then
      reason="$current is the newest Go release"
    else
      reason="$current is newer than the newest stable Go release $latest"
    fi
  else
    update=true
    for candidate in "${candidates[@]}"; do
      find_pull "$branch_prefix$candidate"
      if [ -z "$pull_number" ]; then
        target="$candidate"
        break
      fi
      pull_request="$pull_number"
      if [ "$pull_state" = open ]; then
        reason="pull request #$pull_number for $candidate is open"
        break
      fi
      # A candidate is newer than current. If its pull request was merged,
      # someone lowered the toolchain after the merge. Maybe on purpose, so
      # the plan does not fail, but it says so.
      if [ "$pull_state" = merged ]; then
        echo "::warning::Pull request #$pull_number for $candidate was merged, but go.mod selects $current now. The workflow does not propose $candidate again."
      fi
      skipped="$skipped${skipped:+, }$candidate (#$pull_number $pull_state)"
    done

    if [ -n "$target" ]; then
      pull_request=""
      setup_version="${target#go}"
      branch="$branch_prefix$target"
      # From a release candidate or a language version (go1.28rc2, go1.28)
      # to a release is the first release of that Go version for this
      # repository, so it is a minor update too.
      if [[ "$current" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]] \
        && [ "$(minor_of "$target")" = "$(minor_of "$current")" ]; then
        kind="patch"
      else
        kind="minor"
      fi
      api "repos/$REPO/git/ref/heads/$branch" "$work/ref.json"
      if [ -s "$work/ref.json" ]; then
        check_branch "$branch" "$target" "$go_line"
        action=open
        reason="$target is newer than $current, and the branch $branch exists without a pull request"
      else
        action=push-and-open
        reason="$target is newer than $current"
      fi
      if [ -n "$skipped" ]; then
        reason="$reason (skipped: $skipped)"
      fi
    elif [ -z "$reason" ]; then
      reason="the pull requests of all newer releases are closed: $skipped"
    fi
  fi

  echo "Plan: $reason."
  result "current=$current" "latest=$latest" "target=$target" "update=$update" \
    "kind=$kind" "setup_version=$setup_version" "branch=$branch" "action=$action" \
    "pull_request=$pull_request" "reason=$reason"

  if [ -n "${SUMMARY_FILE:-}" ]; then
    {
      echo "### Go toolchain"
      echo
      echo "| | |"
      echo "|---|---|"
      echo "| Toolchain in go.mod | \`$current\` |"
      echo "| Newest Go release | \`$latest\` |"
      echo "| Target | ${target:+\`}${target:-none}${target:+\`}${kind:+ ($kind)} |"
      echo "| Branch | ${branch:+\`}${branch:-none}${branch:+\`} |"
      echo "| Action | \`$action\` ($reason) |"
    } >> "$SUMMARY_FILE"
  fi
}

apply() {
  local version="${VERSION:?VERSION is required}" go_before go_after toolchain

  [[ "$version" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "VERSION '$version' is not a Go release like go1.27.2."
  [ -f go.mod ] || fail "There is no go.mod in $(pwd)."

  go_before="$(directive go.mod go)"
  go get "toolchain@$version"
  go mod tidy
  go_after="$(directive go.mod go)"
  toolchain="$(toolchain_of go.mod)"

  # The go line is the minimum Go version for everyone who builds from
  # source, and it sets the language version and the GODEBUG defaults.
  if [ "$go_after" != "$go_before" ]; then
    fail "The go line changed from '$go_before' to '$go_after'. Only the toolchain line may change."
  fi
  if [ "$toolchain" != "$version" ]; then
    fail "go.mod selects $toolchain after the update, expected $version."
  fi
  echo "go.mod now selects $version, the go line is still $go_before."
}

texts() {
  local current="${CURRENT:?CURRENT is required}" target="${TARGET:?TARGET is required}"
  local latest="${LATEST:?LATEST is required}" kind="${KIND:?KIND is required}"
  local dir="${TEXT_DIR:?TEXT_DIR is required}" minor notes

  [[ "$target" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "TARGET '$target' is not a Go release like go1.27.2."
  [ "$kind" = patch ] || [ "$kind" = minor ] || fail "KIND '$kind' is not patch or minor."
  [ -d "$dir" ] || fail "$dir does not exist."
  minor="$(minor_of "$target")"
  if [ "$kind" = minor ]; then
    notes="https://go.dev/doc/$minor and https://go.dev/doc/devel/release#$target"
  else
    notes="https://go.dev/doc/devel/release#$target"
  fi

  echo "updates the Go toolchain to $target" > "$dir/pr-title.txt"

  {
    echo "updates the Go toolchain to $target"
    echo
    echo "go.mod selected $current. The go line (the minimum Go version)"
    echo "stays as it is."
    echo
    echo "Created by the Go toolchain workflow."
  } > "$dir/commit-message.txt"

  {
    echo "Updates the toolchain line of \`go.mod\` from \`$current\` to \`$target\`."
    echo
    echo "* Release notes: $notes"
    echo "* The \`go\` line (the minimum Go version) stays as it is, so the language version and the default GODEBUG settings do not change."
    echo "* CI, Lint, Test and the Release dry run build with $target, because \`actions/setup-go\` reads the \`toolchain\` line."
    echo "* Merging this pull request creates a new release with binaries built by $target, because \`go.mod\` changes."
    if [ "$latest" != "$target" ]; then
      echo "* $latest is newer. Patch releases of the current Go release come first, see CONTRIBUTING.md."
    fi
    if [ "$kind" = minor ]; then
      echo
      echo "**$minor is a new minor release.** Three things to check:"
      echo
      echo "* golangci-lint stops if it was built with an older Go than the toolchain line. If the golangci-lint check fails with \"used to build golangci-lint is lower than the targeted Go version\", set \`version\` in \`.github/workflows/lint.yaml\` to a golangci-lint release that supports $minor, on this branch."
      echo "* \`isTransientNetworkError\` in \`cmd/gitlab-migrator/app.go\` finds HTTP/2 errors by their text. Check that these texts still exist in $minor."
      echo "* The Ports section of the release notes. The release binaries are for Linux, macOS and Windows on amd64 and arm64. If $minor drops a system version (Go 1.27 for example needs macOS 13), users of that version cannot run the next release. Mention it in the release notes, and think about the label \`release:minor\`."
    fi
    if [ "${HAS_TOKEN:-}" != true ]; then
      echo
      echo "**The checks wait for an approval.** \`GITHUB_TOKEN\` opened this pull request, so its workflow runs start only after you select \"Approve workflows to run\" in the merge box. With the secret \`GO_TOOLCHAIN_TOKEN\` they start on their own, see CONTRIBUTING.md."
    fi
    echo
    echo "Other open Go toolchain pull requests change the same line, so after a merge they have a conflict. Close the ones for an older version than the merged one: merging them would go back to an older toolchain. Resolve the conflict only for a newer version."
    if [ -n "${RUN_URL:-}" ]; then
      echo
      echo "Opened by the [Go toolchain workflow]($RUN_URL)."
    fi
  } > "$dir/pr-body.md"
}

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

case "${1:-}" in
  plan) plan ;;
  apply) apply ;;
  texts) texts ;;
  *) fail "Unknown mode '${1:-}', expected plan, apply or texts." ;;
esac
