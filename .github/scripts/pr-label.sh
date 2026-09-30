#!/usr/bin/env bash
#
# Adds a label to a pull request, chosen by the prefix of its branch. The
# label chooses the section of the pull request in the release notes
# (.github/release.yaml). The Labeler workflow runs this script after every
# push, and pr-label_test.sh tests it.
#
# Rules, in this order:
#
#   1. The branch prefix picks the label. Upper and lower case count as the
#      same. Other branches get no label, and the script makes no API call.
#        feat/, feature/          enhancement
#        fix/, bugfix/, hotfix/   bug
#        docs/                    documentation
#   2. If the pull request has a section label already (section_labels
#      below), nothing happens. So a label that someone set always wins.
#   3. If the label was on the pull request before, someone removed it, and
#      nothing happens. So a removed label does not come back.
#   4. Otherwise the script adds the label. It uses POST, which keeps all
#      other labels. PUT would replace them.
#
# The script reads the current labels through the API and not from the
# event, because "gh pr create --label" adds its labels right after the
# pull request is opened, so the "opened" event does not have them. A label
# that someone adds in the short time between that read and the POST can
# end up next to this one.
#
# Input (environment): BRANCH (head branch), NUMBER (pull request number),
# REPO (owner/name). Needs bash and gh with a token that may add labels.

set -euo pipefail

# The labels that choose a section in .github/release.yaml, without the
# release: labels, which choose the version. pr-label_test.sh checks that
# this list matches release.yaml.
section_labels=(enhancement bug documentation dependencies)

# label_for_branch BRANCH prints the label for BRANCH, or nothing.
label_for_branch() {
  shopt -s nocasematch
  case "$1" in
    feat/* | feature/*) echo "enhancement" ;;
    fix/* | bugfix/* | hotfix/*) echo "bug" ;;
    docs/*) echo "documentation" ;;
  esac
}

# has_label LIST LABEL is true if LIST (one label per line) contains LABEL.
# GitHub treats label names without case, so this does too.
has_label() {
  grep -qixF -- "$2" <<< "$1"
}

branch="${BRANCH:?BRANCH is required}"
number="${NUMBER:?NUMBER is required}"
repo="${REPO:?REPO is required}"

label="$(label_for_branch "$branch")"
if [ -z "$label" ]; then
  echo "Branch $branch has no prefix with a label."
  exit 0
fi

current="$(gh api --paginate "repos/$repo/issues/$number/labels" --jq '.[].name')"
for section_label in "${section_labels[@]}"; do
  if has_label "$current" "$section_label"; then
    echo "Pull request #$number has the section label $section_label already."
    exit 0
  fi
done

added="$(gh api --paginate "repos/$repo/issues/$number/events" \
  --jq '.[] | select(.event == "labeled") | .label.name')"
if has_label "$added" "$label"; then
  echo "Pull request #$number had the label $label before, and someone removed it. It is not added again."
  exit 0
fi

echo "Branch $branch gets the label $label."
gh api --method POST "repos/$repo/issues/$number/labels" -f "labels[]=$label" > /dev/null
