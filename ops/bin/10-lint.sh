#!/usr/bin/env bash

die() { echo "lint failed: $*" >&2; exit 1; }

[[ -n $CONTAINER ]] || die "CONTAINER env not set"

cd "$(dirname "$(git rev-parse --absolute-git-dir)")" || die "not in repo root"

hash aws 2>/dev/null        || die "missing dep: aws"
hash docker 2>/dev/null     || die "missing dep: docker"
hash shellcheck 2>/dev/null || die "missing dep: shellcheck"

shopt -s globstar extglob nullglob

echo "~~~ :bash: linting scripts"
for s in **/*.sh; do
    shellcheck -- "$s"
done

echo "~~~ :docker: pull container"
docker pull "$CONTAINER" || die "pull $CONTAINER"

docker run -it --rm \
  -e AWS_REGION=ap-southeast-2 \
  -e GIT_TERMINAL_PROMPT=0 \
  -v "$(pwd):/src" \
  -w /src \
  "$CONTAINER" sh -c "go vet -all ."
