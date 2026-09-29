#!/usr/bin/env bash
#
# Tests for release-plan.sh. Run it from anywhere:
#
#   bash .github/scripts/release-plan_test.sh

set -euo pipefail

script="$(dirname "$0")/release-plan.sh"
failures=0

# Runs release-plan.sh with only the given variables set.
run_plan() {
  env -i PATH="$PATH" "$@" bash "$script"
}

# expect NAME WANT [VAR=value ...]
# WANT lists the previous, bump, version and release lines, joined by spaces.
expect() {
  local name="$1" want="$2" got
  shift 2
  if got="$(run_plan "$@" | grep -E '^(previous|bump|version|release)=' | paste -sd ' ' -)" &&
    [ "$got" = "$want" ]; then
    echo "ok    $name"
  else
    echo "FAIL  $name"
    echo "      want: $want"
    echo "      got:  ${got:-<error>}"
    failures=$((failures + 1))
  fi
}

# expect_error NAME [VAR=value ...]
expect_error() {
  local name="$1"
  shift
  if run_plan "$@" > /dev/null 2>&1; then
    echo "FAIL  $name (expected an error)"
    failures=$((failures + 1))
  else
    echo "ok    $name"
  fi
}

tags=$'v0.9.0\nv0.15.1\nv0.16.0\nv0.10.0'

expect "no tag starts at v0.0.1" \
  "previous= bump=patch version=v0.0.1 release=true"

expect "no tag with release:minor" \
  "previous= bump=minor version=v0.1.0 release=true" \
  LABELS=release:minor

expect "patch is the default" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=true" \
  TAGS="$tags"

expect "release:minor" \
  "previous=v0.16.0 bump=minor version=v0.17.0 release=true" \
  TAGS="$tags" LABELS=$'dependencies\nrelease:minor'

expect "release:major" \
  "previous=v0.16.0 bump=major version=v1.0.0 release=true" \
  TAGS="$tags" LABELS=release:major

expect "major wins over minor" \
  "previous=v0.16.0 bump=major version=v1.0.0 release=true" \
  TAGS="$tags" LABELS=$'release:minor\nrelease:major'

expect "release:skip" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=false" \
  TAGS="$tags" LABELS=release:skip

expect "release:skip wins over release:major" \
  "previous=v0.16.0 bump=major version=v1.0.0 release=false" \
  TAGS="$tags" LABELS=$'release:major\nrelease:skip'

expect "[skip release] in the commit message" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=false" \
  TAGS="$tags" MESSAGE=$'Merge pull request #5 from a/b\n\nfixes a typo [skip release]'

expect "labels must match exactly" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=true" \
  TAGS="$tags" LABELS=$'Release:Major\nrelease:minor-ish\nrelease:skip '

expect "versions sort as numbers" \
  "previous=v0.10.0 bump=patch version=v0.10.1 release=true" \
  TAGS=$'v0.9.0\nv0.10.0\nv0.2.0'

expect "tags that are not vX.Y.Z are ignored" \
  "previous=v0.16.0 bump=patch version=v0.16.1 release=true" \
  TAGS=$'v0.16.0\nv0.17.0-rc.1\nv1.0\nv01.0.0\nlatest\n1.2.3'

expect "manual bump wins over labels and message" \
  "previous=v0.16.0 bump=minor version=v0.17.0 release=true" \
  TAGS="$tags" BUMP=minor LABELS=release:skip MESSAGE='[skip release]'

expect "manual major bump" \
  "previous=v1.2.3 bump=major version=v2.0.0 release=true" \
  TAGS=v1.2.3 BUMP=major

expect_error "invalid manual bump" TAGS="$tags" BUMP=huge

if [ "$failures" -gt 0 ]; then
  echo "$failures test(s) failed"
  exit 1
fi
echo "all tests passed"
