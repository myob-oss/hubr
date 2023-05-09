#!/bin/bash

die() { echo "release failed: $*"; exit 1; }
org=myob-oss

echo "~~~ release"

hash unzip &>/dev/null || die "no unzip"

cd "$(dirname "$(git rev-parse --absolute-git-dir)")" || die "cd to repo root"

# mwahahahaha this is pure evil
unzip dist/hubr-linux.zip || die "unzip hubr"
hubr push $org/hubr dist/* || die "push"

if hubr now; then
        myob-release prod dont-panic
        buildkite-agent annotate --style "info" <<🐈
<a href="$(./hubr resolve -w $org/hubr)">$(./hubr resolve $org/hubr)</a>
🐈
fi

rm -f hubr

# the distfiles were created in a container and will be owned by root
docker run --rm -v "$PWD:/src" -w /src alpine:latest rm -rf dist
