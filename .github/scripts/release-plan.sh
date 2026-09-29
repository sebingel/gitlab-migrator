#!/usr/bin/env bash
#
# Decides if a run of the Release workflow creates a release, and which
# version it gets. The workflow collects the facts (tags, commit message,
# labels) and this script makes every decision, so that
# release-plan_test.sh can test all of it.
#
# Usage:
#
#   release-plan.sh plan     plan the release (the Plan release job)
#   release-plan.sh verify   check that a plan is still current (the Publish
#                            release job, right before the release is made)
#
# All input comes from environment variables.
#
# plan:
#   EVENT           push, pull_request or workflow_dispatch
#   REF             Git ref of the run, for example refs/heads/main
#   SHA             the commit that the run releases
#   REPO            this repository, owner/name
#   TAGS            all tags of the repository, one per line
#   MERGED_TAGS     the tags that SHA contains (git tag --merged), one per line
#   SHA_TAGS        the tags on SHA itself (git tag --points-at), one per line
#   COMMIT_MESSAGE  full message of SHA (push)
#   PR_LABELS       labels of the pull request as a JSON array (pull_request)
#   PR_HEAD_SHA     head commit of the pull request (pull_request)
#   BUMP            patch, minor or major (workflow_dispatch)
#
# verify:
#   TAGS            all tags of the repository, one per line
#   PREVIOUS        "previous" from the plan
#   VERSION         "version" from the plan
#
# Both:
#   RESULT_FILE      file for the key=value results (default: stdout)
#   SUMMARY_FILE     file for a Markdown summary (optional)
#   API_RETRIES      calls to the pull request API before giving up (3)
#   API_RETRY_DELAY  seconds between these calls (10)
#
# plan writes these keys:
#   previous       highest vMAJOR.MINOR.PATCH tag, empty if there is none
#   bump           patch, minor or major
#   version        the next version, for example v1.4.3
#   release        true, or false if the release is skipped
#   publish        true if this run publishes the release
#   build_version  version for the binaries; dry runs get -dryrun.<commit>
#   pull_request   number of the merged pull request, if there is one
#   reason         a short text that explains the decision
#
# Rules: the default bump is patch. The label release:minor or release:major
# picks a bigger bump (major wins). Only the label release:skip skips a
# release. Tags that are not plain vMAJOR.MINOR.PATCH (for example
# v1.0.0-rc.1) are ignored.
#
# Needs: bash, jq, gh (only for push).

set -euo pipefail

semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
merge_re='^Merge pull request #([0-9]+) from '
main_ref="refs/heads/main"

fail() {
  echo "::error::$*"
  exit 1
}

warn() {
  echo "::warning::$*"
}

# highest LIST prints the highest vMAJOR.MINOR.PATCH tag of LIST.
highest() {
  { grep -E "$semver_re" <<< "$1" || true; } | sort -V | tail -n 1
}

# has_line LIST LINE is true if LIST contains exactly LINE.
has_line() {
  grep -qxF -- "$2" <<< "$1"
}

result() {
  printf '%s\n' "$@" >> "${RESULT_FILE:-/dev/stdout}"
}

# get_pull NUMBER FILE writes the pull request NUMBER of $REPO as JSON to
# FILE. FILE stays empty if the pull request does not exist (HTTP 404). Other
# API errors, for example HTTP 502 or a rate limit, are retried.
get_pull() {
  local number="$1" file="$2" retries="${API_RETRIES:-3}" attempt=1
  while true; do
    if gh api "repos/$REPO/pulls/$number" > "$file" 2> "$work/gh.err"; then
      return 0
    fi
    if grep -q "HTTP 404" "$work/gh.err"; then
      : > "$file"
      return 0
    fi
    echo "Reading pull request #$number failed (attempt $attempt of $retries): $(cat "$work/gh.err")"
    if [ "$attempt" -ge "$retries" ]; then
      fail "Could not read pull request #$number from the API. Run the failed jobs again later."
    fi
    attempt=$((attempt + 1))
    sleep "${API_RETRY_DELAY:-10}"
  done
}

# find_pull_labels reads the pull request that $SHA merged, if there is one.
# It sets $pull_request and $labels.
find_pull_labels() {
  local message="${COMMIT_MESSAGE:-}" first_line number file="$work/pull.json"
  first_line="${message%%$'\n'*}"
  if [[ "$first_line" =~ $merge_re ]]; then
    number="${BASH_REMATCH[1]}"
    get_pull "$number" "$file"
    if [ -s "$file" ]; then
      jq -e 'type == "object"' "$file" > /dev/null ||
        fail "The API answer for pull request #$number is not a JSON object."
      if jq -e --arg repo "$REPO" --arg sha "$SHA" \
        '.merged == true and .base.repo.full_name == $repo and .base.ref == "main" and .merge_commit_sha == $sha' \
        "$file" > /dev/null; then
        pull_request="$number"
        labels="$(jq -r '.labels[].name' "$file")"
        echo "Pull request #$number, labels: $(paste -sd ' ' - <<< "${labels:-none}")"
        return 0
      fi
    fi
  fi
  warn "$SHA is not the merge commit of a pull request into main of $REPO (first line: '$first_line'). So no labels apply and this is a patch release. For another bump, start a manual release."
}

plan() {
  local event="${EVENT:?EVENT is required}" ref="${REF:?REF is required}"
  local previous released reason bump release="true" publish="false"
  pull_request=""
  labels=""
  previous="$(highest "${TAGS:-}")"

  case "$event" in
    pull_request)
      labels="$(jq -r '.[]' <<< "${PR_LABELS:-[]}")"
      reason="labels of this pull request"
      ;;
    push | workflow_dispatch)
      if [ "$ref" != "$main_ref" ]; then
        fail "Releases are only made from main, not from $ref."
      fi
      : "${SHA:?SHA is required}"
      # "Re-run all jobs" on a finished run, or a manual run without new
      # commits, would release the same code again as a new version.
      released="$(highest "${SHA_TAGS:-}")"
      if [ -n "$released" ]; then
        fail "$SHA is already released as $released. There is nothing new to release."
      fi
      # "Re-run all jobs" on an old run keeps the old commit, but sees the
      # new tags. Never release a commit that is older than the last release.
      if [ -n "$previous" ] && ! has_line "${MERGED_TAGS:-}" "$previous"; then
        fail "$SHA does not contain the previous release $previous. An old run was probably started again. Use a manual release on main instead."
      fi
      if [ "$event" = "push" ]; then
        : "${REPO:?REPO is required}"
        find_pull_labels
        if [ -n "$pull_request" ]; then
          reason="labels of pull request #$pull_request"
        else
          reason="no pull request, default patch"
        fi
      fi
      ;;
    *)
      fail "Unexpected event '$event'."
      ;;
  esac

  if [ "$event" = "workflow_dispatch" ]; then
    case "${BUMP:-}" in
      patch | minor | major) bump="$BUMP" ;;
      *) fail "Invalid bump '${BUMP:-}', expected patch, minor or major." ;;
    esac
    reason="manual run with bump $bump"
  elif has_line "$labels" release:major; then
    bump="major"
  elif has_line "$labels" release:minor; then
    bump="minor"
  else
    bump="patch"
  fi

  if [ "$event" != "workflow_dispatch" ] && has_line "$labels" release:skip; then
    release="false"
    reason="label release:skip"
  fi

  local major minor patch
  IFS=. read -r major minor patch <<< "${previous:-v0.0.0}"
  major="${major#v}"
  case "$bump" in
    major) major=$((major + 1)) minor=0 patch=0 ;;
    minor) minor=$((minor + 1)) patch=0 ;;
    patch) patch=$((patch + 1)) ;;
  esac
  local version="v$major.$minor.$patch" build_version
  build_version="$version"

  if [ "$event" = "pull_request" ]; then
    : "${PR_HEAD_SHA:?PR_HEAD_SHA is required}"
    build_version="$version-dryrun.${PR_HEAD_SHA:0:7}"
  elif [ "$release" = "true" ]; then
    publish="true"
  fi

  echo "Plan: previous=${previous:-none} bump=$bump version=$version release=$release publish=$publish ($reason)"
  result "previous=$previous" "bump=$bump" "version=$version" "release=$release" \
    "publish=$publish" "build_version=$build_version" "pull_request=$pull_request" \
    "reason=$reason"

  if [ -n "${SUMMARY_FILE:-}" ]; then
    {
      echo "### Release plan"
      echo
      echo "| | |"
      echo "|---|---|"
      echo "| Event | \`$event\` |"
      echo "| Pull request | ${pull_request:+#}${pull_request:-none} |"
      echo "| Previous version | \`${previous:-none}\` |"
      echo "| Bump | \`$bump\` |"
      echo "| Next version | \`$version\` |"
      echo "| Release | \`$release\` ($reason) |"
      echo "| Publish in this run | \`$publish\` |"
      echo "| Version in the binaries | \`$build_version\` |"
    } >> "$SUMMARY_FILE"
  fi
}

verify() {
  local version="${VERSION:?VERSION is required}" previous="${PREVIOUS:-}" now
  now="$(highest "${TAGS:-}")"
  # "Re-run failed jobs" reuses the old plan. If another release was made
  # since then, this run would publish older code as the latest release.
  if [ "$now" != "$previous" ]; then
    fail "This run was planned after ${previous:-no tag}, but the latest tag is now ${now:-none}. Start a manual release on main instead."
  fi
  if has_line "${TAGS:-}" "$version"; then
    fail "Tag $version already exists."
  fi
  echo "The plan is current: $version follows ${previous:-no tag}."
}

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

case "${1:-}" in
  plan) plan ;;
  verify) verify ;;
  *) fail "Unknown mode '${1:-}', expected plan or verify." ;;
esac
