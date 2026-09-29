#!/usr/bin/env bash
#
# Decides if a release is created and which version it gets.
#
# All input comes from environment variables, so that multi line values like
# commit messages need no quoting. Every variable is optional.
#
#   TAGS     all tags of the repository, one per line
#   LABELS   labels of the pull request, one per line
#   MESSAGE  message of the pushed commit, or the title of the pull request
#   BUMP     patch, minor or major. Manual runs set it. It wins over LABELS
#            and MESSAGE.
#
# The output is key=value lines that can go straight into $GITHUB_OUTPUT.
#
#   previous  the highest vMAJOR.MINOR.PATCH tag, empty if there is none
#   bump      patch, minor or major
#   version   the next version, for example v1.4.3
#   release   true, or false if the release is skipped
#   reason    a short text that explains the decision
#
# Rules: the default bump is patch. The label release:minor or release:major
# picks a bigger bump (major wins). The label release:skip or the text
# "[skip release]" in MESSAGE skips the release. Tags that are not plain
# vMAJOR.MINOR.PATCH (for example v1.0.0-rc.1) are ignored.

set -euo pipefail

semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

has_label() {
  grep -qxF -- "$1" <<< "${LABELS:-}"
}

previous="$({ grep -E "$semver_re" <<< "${TAGS:-}" || true; } | sort -V | tail -n 1)"

release="true"
if [ -n "${BUMP:-}" ]; then
  case "$BUMP" in
    patch | minor | major)
      bump="$BUMP"
      reason="manual run with bump $BUMP"
      ;;
    *)
      echo "invalid BUMP '$BUMP', expected patch, minor or major" >&2
      exit 1
      ;;
  esac
else
  if has_label release:major; then
    bump="major"
    reason="label release:major"
  elif has_label release:minor; then
    bump="minor"
    reason="label release:minor"
  else
    bump="patch"
    reason="default patch bump"
  fi

  if has_label release:skip; then
    release="false"
    reason="label release:skip"
  elif grep -qF -- '[skip release]' <<< "${MESSAGE:-}"; then
    release="false"
    reason="[skip release] in the commit message"
  fi
fi

base="${previous:-v0.0.0}"
IFS=. read -r major minor patch <<< "${base#v}"
case "$bump" in
  major)
    major=$((major + 1))
    minor=0
    patch=0
    ;;
  minor)
    minor=$((minor + 1))
    patch=0
    ;;
  patch)
    patch=$((patch + 1))
    ;;
esac

echo "previous=$previous"
echo "bump=$bump"
echo "version=v$major.$minor.$patch"
echo "release=$release"
echo "reason=$reason"
