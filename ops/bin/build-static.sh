#!/bin/bash

die() { echo "build-static failed: $*"; exit 1; }

cd "$(dirname "$(git rev-parse --absolute-git-dir)")" || die "cd to repo root"

echo "~~~ :recycle: pre build"
shopt -s extglob
rm -rf dist
mkdir -p dist || die "dist"

LDFLAGS=(
  "-X main.hubr=$(head -n1 VERSION)-static"
  "-X main.defaultChain=env:GITHUB_API_TOKEN,env:TOKEN,ssm:/etc/tokens/oss-gh"
)

apt update
apt -y install zip

for os in linux darwin windows; do
    echo "~~~ :go: :clipboard: build $os"
    rm -f hubr hubr.exe
    CGO_ENABLED=0 GOOS="$os" go build \
     -buildvcs=false -ldflags="${LDFLAGS[*]}" || die "build"
    zip -j "dist/hubr-$os.zip" hubr?(.exe) || die "zip"
done

rm -f hubr hubr.exe
