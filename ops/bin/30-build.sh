#!/bin/bash

die() { echo "build failed: $*"; exit 1; }

[[ -n $CONTAINER ]] || die "CONTAINER env not set"

cd "$(dirname "$(git rev-parse --absolute-git-dir)")" || die "not in repo root"

echo "~~~ :docker: pull container"
docker pull "$CONTAINER" || die "pull $CONTAINER"

echo "~~~ :docker: run build"
docker run -it --rm \
  -e AWS_REGION=ap-southeast-2 \
  -e GIT_TERMINAL_PROMPT=0 \
  -v "$PWD:/src" \
  -w /src \
  "$CONTAINER" ops/bin/build-static.sh || die "run build-static"
