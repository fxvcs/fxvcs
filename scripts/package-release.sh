#!/usr/bin/env bash

set -euo pipefail

version="${VERSION:?VERSION must be set to a release tag such as v0.1.0}"
goos="${GOOS:?GOOS must be set}"
goarch="${GOARCH:?GOARCH must be set}"

if [[ ! "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
  echo "refusing to package: VERSION must be a v-prefixed semantic version, got $version" >&2
  exit 1
fi

case "$goos/$goarch" in
  linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64) ;;
  *) echo "refusing to package unsupported target $goos/$goarch" >&2; exit 1 ;;
esac

release_version="${version#v}"
archive_base="fxvcs_${release_version}_${goos}_${goarch}"
stage="dist/.stage/$archive_base"
exe="fxvcs"
if [[ "$goos" == "windows" ]]; then
  exe="fxvcs.exe"
fi

rm -rf "$stage"
mkdir -p "$stage"

VERSION="$version" GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 mise run build
cp "bin/$exe" "$stage/$exe"

mkdir -p dist
if [[ "$goos" == "windows" ]]; then
  archive="dist/$archive_base.zip"
  rm -f "$archive"
  (cd "$stage" && zip -q "../../../$archive" "$exe")
else
  archive="dist/$archive_base.tar.gz"
  rm -f "$archive"
  tar -C "$stage" -czf "$archive" "$exe"
fi

echo "$archive"
