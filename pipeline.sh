#!/bin/bash

[[ -n $QUEUE ]] || { echo "QUEUE is not set sad face emoji" >&2; exit 1; }
# CONTAINER is required by build-static.sh, may as well check here too
[[ -n $CONTAINER ]] || { echo "CONTAINER is not set crying emoji" >&2; exit 1; }

cat <<🐈
steps:
  - label: ':female-astronaut: build, test and release'
    command:
      - ops/bin/10-lint.sh
      - ops/bin/30-build.sh
      - ops/bin/20-test.sh
      - ops/bin/40-release.sh
    agents:
      queue: $QUEUE
    env:
      CONTAINER: $CONTAINER
  - wait
  - trigger: 'hubr-myob'
    label: ':satellite_antenna: build private version'
    async: true
🐈
