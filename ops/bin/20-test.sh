#!/bin/bash

die() { echo "test failed: $*"; exit 1; }
org=MYOB-Technology

echo "~~~ test"

hash unzip &>/dev/null || die "no unzip"

cd "$(dirname "$(git rev-parse --absolute-git-dir)")" || die "cd to repo root"

# TODO better tests
unzip dist/hubr-linux.zip || die "unzip hubr"
echo "hubr tags"
./hubr tags -la $org/hubr || die "tags"
echo "hubr assets"
./hubr assets -l $org/hubr || die "assets"
echo "hubr get"
./hubr get $org/hubr:hubr-linux.zip || die "get"
echo "hubr bump"
./hubr bump -w patch || die "bump"
rm -f hubr
