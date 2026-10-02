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
#   PR_TITLE        title of the pull request (pull_request)
#   PR_COMMIT_MESSAGES
#                   messages of all commits of the pull request, each one
#                   after a line "@@@ <short sha>" (pull_request)
#   BUMP            patch, minor or major (workflow_dispatch)
#   RESULT_FILE     file for the key=value results (default: stdout)
#   SUMMARY_FILE    file for a Markdown summary (optional)
#   API_RETRIES     calls to the pull request API before giving up (3)
#   API_RETRY_DELAY seconds between these calls (10)
#
# verify:
#   TAGS            all tags of the repository, one per line
#   PREVIOUS        "previous" from the plan
#   VERSION         "version" from the plan
#
# plan writes these keys:
#   previous       highest vMAJOR.MINOR.PATCH tag, empty if there is none
#   bump           patch, minor or major
#   version        the next version, for example v1.4.3
#   release        true, or false if the release is skipped
#   publish        true if this run publishes the release
#   build_version  version for the binaries; dry runs get -dryrun.<commit>
#   pull_request   number of the merged pull request, if there is one (for
#                  a stack merge the top one, from the first line)
#   pull_requests  numbers of all pull requests of the merge, from main up,
#                  separated by commas (more than one for a stack merge)
#   reason         a short text that explains the decision
#
# Rules: the default bump is patch. The label release:minor or release:major
# picks a bigger bump (major wins). The label release:skip skips a release.
# A stack merge (several pull requests with one merge commit) gets the
# highest bump of all its pull requests without release:skip. It skips the
# release only if all of them have release:skip. A push of a commit that
# already has a release tag skips it too, and a manual run of such a commit
# fails. Tags that are not plain vMAJOR.MINOR.PATCH (for example
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

notice() {
  echo "::notice::$*"
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

# api_get PATH FILE WHAT writes the answer of "gh api PATH" as JSON to FILE.
# FILE stays empty if the API answers HTTP 404. Other API errors, for
# example HTTP 502 or a rate limit, are retried. WHAT names the request in
# the messages.
api_get() {
  local path="$1" file="$2" what="$3" retries="${API_RETRIES:-3}" attempt=1
  while true; do
    if gh api "$path" > "$file" 2> "$work/gh.err"; then
      return 0
    fi
    if grep -q "HTTP 404" "$work/gh.err"; then
      : > "$file"
      return 0
    fi
    echo "Reading $what failed (attempt $attempt of $retries): $(cat "$work/gh.err")"
    if [ "$attempt" -ge "$retries" ]; then
      fail "Could not read $what from the API. Run the failed jobs again later."
    fi
    attempt=$((attempt + 1))
    sleep "${API_RETRY_DELAY:-10}"
  done
}

# get_pull NUMBER FILE writes the pull request NUMBER of $REPO as JSON to
# FILE. FILE stays empty if the pull request does not exist (HTTP 404).
get_pull() {
  api_get "repos/$REPO/pulls/$1" "$2" "pull request #$1"
}

# get_pull_from BRANCH FILE writes the merged pull request of $REPO from
# BRANCH whose merge commit is $SHA as JSON to FILE. FILE stays empty if
# there is none. BRANCH is the base of another pull request, so it is a
# branch of $REPO itself, and its owner is the owner of $REPO.
get_pull_from() {
  local branch="$1" file="$2" head list="$work/pulls.json"
  head="$(jq -rn --arg head "${REPO%%/*}:$branch" '$head | @uri')"
  api_get "repos/$REPO/pulls?state=closed&head=$head&per_page=100" "$list" \
    "the pull requests from $branch"
  if [ ! -s "$list" ]; then
    : > "$file"
    return 0
  fi
  jq -e 'type == "array"' "$list" > /dev/null ||
    fail "The API answer for the pull requests from $branch is not a JSON array."
  # The list has no "merged" field, only merged_at. It only holds pull
  # requests into $REPO, so the base repository needs no check.
  jq -c --arg sha "$SHA" \
    'map(select(.merged_at != null and .merge_commit_sha == $sha)) | first // empty' \
    "$list" > "$file"
}

# find_pull_labels reads the pull requests that $SHA merged into main, if
# there are any. A normal merge has one. A stack merge has one merge commit
# for all pull requests of the stack, and all of them have it as their
# merge_commit_sha. Its first line names the top pull request. The base of
# each pull request is the head branch of the one below it, down to the one
# into main. find_pull_labels follows this chain from the top down.
#
# It sets $pull_request (the one of the first line), $pull_requests (all of
# them from main up, separated by commas) and $labels. The labels are those
# of all pull requests without release:skip, so the bump is the highest of
# their labels. Only if every pull request has release:skip, the labels are
# those of all of them, and the release is skipped. This follows what
# merging the pull requests one by one would do: each one without
# release:skip would make a release, and each one with it none.
find_pull_labels() {
  local message="${COMMIT_MESSAGE:-}" first_line top number base pr_labels
  local file="$work/pull.json" stack="" release_labels="" skip_labels="" releases=0
  first_line="${message%%$'\n'*}"
  if [[ "$first_line" =~ $merge_re ]]; then
    top="${BASH_REMATCH[1]}"
    number="$top"
    get_pull "$number" "$file"
    if [ -s "$file" ]; then
      jq -e 'type == "object"' "$file" > /dev/null ||
        fail "The API answer for pull request #$number is not a JSON object."
      if jq -e --arg repo "$REPO" --arg sha "$SHA" \
        '.merged == true and .base.repo.full_name == $repo and .merge_commit_sha == $sha' \
        "$file" > /dev/null; then
        while true; do
          if has_line "${stack//,/$'\n'}" "$number"; then
            fail "Pull request #$number is twice in the chain of pull requests of $SHA."
          fi
          stack="$number${stack:+,$stack}"
          pr_labels="$(jq -r '.labels[].name' "$file")"
          echo "Pull request #$number, labels: $(paste -sd ' ' - <<< "${pr_labels:-none}")"
          if has_line "$pr_labels" release:skip; then
            skip_labels="$skip_labels"$'\n'"$pr_labels"
          else
            release_labels="$release_labels"$'\n'"$pr_labels"
            releases=$((releases + 1))
          fi
          base="$(jq -r '.base.ref' "$file")"
          if [ "$base" = "${main_ref#refs/heads/}" ]; then
            pull_request="$top"
            pull_requests="$stack"
            if [ "$releases" -gt 0 ]; then
              labels="$release_labels"
            else
              labels="$skip_labels"
            fi
            return 0
          fi
          get_pull_from "$base" "$file"
          if [ ! -s "$file" ]; then
            echo "Pull request #$number is based on $base, but no merged pull request of $REPO from $base has the merge commit $SHA."
            break
          fi
          number="$(jq -r '.number' "$file")"
        done
      fi
    fi
  fi
  warn "$SHA is not the merge commit of a pull request into main of $REPO (first line: '$first_line'). So no labels apply and this is a patch release. For another bump, start a manual release."
}

# GitHub starts no push workflow if any commit message of the push contains
# one of its skip markers or a "skip-checks: true" trailer. The push of a
# merge contains the merge commit, whose message contains the pull request
# title, and every commit of the pull request. So a marker in the title or a
# marker or trailer in any of these commit messages stops the release in
# silence. The dry run warns about it. The trailer is matched on any line,
# not only at the end of the message: a false warning costs little, a missed
# release is what this check prevents.
warn_skip_markers() {
  local marker title line commit="" found=""
  local -a markers=('[skip ci]' '[ci skip]' '[no ci]' '[skip actions]' '[actions skip]')
  local commit_re='^@@@ ([0-9a-f]+)$'
  local trailer_re='^skip-checks: ?true[[:space:]]*$'
  title="$(tr '[:upper:]' '[:lower:]' <<< "${PR_TITLE:-}")"
  for marker in "${markers[@]}"; do
    if [[ "$title" == *"$marker"* ]]; then
      warn "The title contains '$marker'. GitHub puts the title into the merge commit and then starts no push workflow, so the merge creates no release. Remove it from the title if you want a release."
    fi
  done
  while IFS= read -r line; do
    if [[ "$line" =~ $commit_re ]]; then
      commit="${BASH_REMATCH[1]}"
      continue
    fi
    for marker in "${markers[@]}"; do
      if [[ "$line" == *"$marker"* ]] && ! has_line "$found" "$commit $marker"; then
        found="$found"$'\n'"$commit $marker"
        warn "Commit $commit of this pull request contains '$marker' in its message. GitHub starts no push workflow if any commit of a push has it, so the merge creates no release. Change the commit message (for example with git rebase) if you want a release."
      fi
    done
    if [[ "$line" =~ $trailer_re ]] && ! has_line "$found" "$commit skip-checks"; then
      found="$found"$'\n'"$commit skip-checks"
      warn "Commit $commit of this pull request has a 'skip-checks: true' trailer in its message. GitHub starts no push workflow if any commit of a push has it, so the merge creates no release. Remove the trailer (for example with git rebase) if you want a release."
    fi
  done <<< "$(tr '[:upper:]' '[:lower:]' <<< "${PR_COMMIT_MESSAGES:-}")"
}

plan() {
  local event="${EVENT:?EVENT is required}" ref="${REF:?REF is required}"
  local previous released reason bump release="true" publish="false"
  pull_request=""
  pull_requests=""
  labels=""
  previous="$(highest "${TAGS:-}")"

  case "$event" in
    pull_request)
      labels="$(jq -r '.[]' <<< "${PR_LABELS:-[]}")"
      reason="labels of this pull request"
      warn_skip_markers
      ;;
    push | workflow_dispatch)
      if [ "$ref" != "$main_ref" ]; then
        fail "Releases are only made from main, not from $ref."
      fi
      : "${SHA:?SHA is required}"
      # "Re-run all jobs" on a finished run, a second push event for the
      # same commit (GitHub can send one), or a manual run without new
      # commits would release the same code again as a new version. A push
      # run skips the release and ends green, because nothing is wrong. A
      # manual run fails, because the user expects a new release.
      #
      # "Re-run all jobs" on an old run keeps the old commit, but sees the
      # new tags. Queued release runs (queue: max) can also start in another
      # order than the merges. So the elif below never releases a commit
      # that is older than the last release.
      released="$(highest "${SHA_TAGS:-}")"
      if [ -n "$released" ]; then
        if [ "$event" = "workflow_dispatch" ]; then
          fail "$SHA is already released as $released. There is nothing new to release."
        fi
        notice "$SHA is already released as $released. This run skips the release."
        release="false"
        reason="already released as $released"
      elif [ -n "$previous" ] && ! has_line "${MERGED_TAGS:-}" "$previous"; then
        fail "$SHA does not contain the previous release $previous. This happens when an old run is started again, or when GitHub started the release run of a newer merge first. Use a manual release on main if you need another bump."
      elif [ "$event" = "push" ]; then
        : "${REPO:?REPO is required}"
        find_pull_labels
        if [[ "$pull_requests" == *,* ]]; then
          reason="labels of the stack merge of pull requests #${pull_requests//,/, #}"
        elif [ -n "$pull_request" ]; then
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
    "pull_requests=$pull_requests" "reason=$reason"

  if [ -n "${SUMMARY_FILE:-}" ]; then
    local pulls="none"
    if [ -n "$pull_requests" ]; then
      pulls="#${pull_requests//,/, #}"
    fi
    {
      echo "### Release plan"
      echo
      echo "| | |"
      echo "|---|---|"
      echo "| Event | \`$event\` |"
      echo "| Pull request | $pulls |"
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
