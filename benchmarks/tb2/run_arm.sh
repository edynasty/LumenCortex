#!/bin/sh
# usage: run_arm.sh <job-name> <agent-import> [extra harbor args...]
set -eu
cd "$(dirname "$0")"
job=$1; agent=$2; shift 2
P=http://host.docker.internal:7897
args=""
for t in $(cat tb2-subset.txt); do args="$args -i $t"; done
PYTHONPATH=$PWD/adapters exec env -u all_proxy -u ALL_PROXY harbor run --path ./tb2/terminal-bench $args \
  --agent "$agent" -n 1 -k 2 --timeout-multiplier 2 \
  --ae http_proxy=$P --ae https_proxy=$P --ae HTTP_PROXY=$P --ae HTTPS_PROXY=$P \
  --ae no_proxy=host.docker.internal,localhost,127.0.0.1 --ae NO_PROXY=host.docker.internal,localhost,127.0.0.1 \
  -o ./jobs --job-name "$job" -y "$@"
