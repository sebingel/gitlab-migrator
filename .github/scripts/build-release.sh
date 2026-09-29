#!/usr/bin/env bash
#
# Builds the release assets for all platforms: one archive per platform and a
# SHA-256 checksums file. Linux and macOS get a .tar.gz, Windows gets a .zip
# with gitlab-migrator.exe. Every archive also holds LICENSE and README.md.
#
# Usage, from the repository root:
#
#   bash .github/scripts/build-release.sh VERSION [OUT_DIR]
#
# VERSION is written into the binary (see the -version flag) and into the
# file names. OUT_DIR defaults to dist. Old gitlab-migrator_* files in OUT_DIR
# are removed first.
#
# Needs: bash, go, zip, GNU tar or bsdtar (macOS), and sha256sum or
# shasum (macOS). The script checks them before it builds anything.

set -euo pipefail

version="${1:?usage: build-release.sh VERSION [OUT_DIR]}"
out_dir="${2:-dist}"
name="gitlab-migrator"
targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)

# The version goes into file names and into -ldflags, so keep it simple.
if ! [[ "$version" =~ ^[0-9A-Za-z._-]+$ ]]; then
  echo "invalid version '$version', allowed characters: 0-9 A-Z a-z . _ -" >&2
  exit 1
fi

for tool in go zip tar; do
  if ! command -v "$tool" > /dev/null; then
    echo "$tool is missing" >&2
    exit 1
  fi
done

# Every archive entry belongs to root (uid and gid 0). GNU tar and bsdtar
# name these options differently.
tar_version="$(tar --version 2>&1 || true)"
case "$tar_version" in
  *"GNU tar"*) tar_owner=(--owner=0 --group=0 --numeric-owner) ;;
  *bsdtar*) tar_owner=(--uid 0 --gid 0 --numeric-owner) ;;
  *)
    echo "unsupported tar '${tar_version%%$'\n'*}', expected GNU tar or bsdtar" >&2
    exit 1
    ;;
esac

if command -v sha256sum > /dev/null; then
  sha256=(sha256sum)
elif command -v shasum > /dev/null; then
  sha256=(shasum -a 256)
else
  echo "sha256sum or shasum is missing" >&2
  exit 1
fi

mkdir -p "$out_dir"
out_dir="$(cd "$out_dir" && pwd)"
rm -f -- "$out_dir/${name}_"*

stage_root="$(mktemp -d)"
trap 'rm -rf -- "$stage_root"' EXIT

host_target="$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
assets=()

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  base="${name}_${version#v}_${goos}_${goarch}"
  stage="$stage_root/$base"
  mkdir -p "$stage"

  binary="$name"
  if [ "$goos" = "windows" ]; then
    binary="$name.exe"
  fi

  echo "building $base"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags="-X 'main.version=$version'" -o "$stage/$binary" ./cmd/gitlab-migrator

  # The binary for this machine can run, so check the embedded version.
  if [ "$target" = "$host_target" ]; then
    got="$("$stage/$binary" -version)"
    if [ "$got" != "$name $version" ]; then
      echo "$target binary reports '$got', expected '$name $version'" >&2
      exit 1
    fi
    echo "checked: $target binary reports '$got'"
  fi

  cp LICENSE README.md "$stage/"
  chmod 0755 "$stage/$binary"
  chmod 0644 "$stage/LICENSE" "$stage/README.md"
  if [ "$goos" = "windows" ]; then
    asset="$base.zip"
    (cd "$stage" && zip -q -X "$out_dir/$asset" "$binary" LICENSE README.md)
  else
    asset="$base.tar.gz"
    tar -C "$stage" "${tar_owner[@]}" -czf "$out_dir/$asset" "$binary" LICENSE README.md
  fi
  assets+=("$asset")
done

checksums="${name}_${version#v}_checksums.txt"
(cd "$out_dir" && "${sha256[@]}" -- "${assets[@]}" > "$checksums")

echo "assets in $out_dir:"
(cd "$out_dir" && ls -l -- "${assets[@]}" "$checksums")
