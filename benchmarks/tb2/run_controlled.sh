#!/bin/sh
# usage: EVAL_AUTHORIZED=1 run_controlled.sh <sitting> <arm>...    (EVAL_PLAN_TB2_DUAL_BASELINE.md §5)
#   arm: LCpost | LCpre | pi | opencode | specpi
# One sitting = every listed arm once over the 13 tasks (-k 1), in the order given; rotate the order
# between sittings (plan §5.3). Each arm is its own Harbor job, named cc-<arm>-s<sitting>. Before each
# job the metering proxy's tag file is set to <arm>/<job>, so the requests of arms that cannot send
# X-Exp-Tag are still attributed (bench/analysis/analyze_tb2.py, tagfile matching, needs -n 1).
# Same scaffold as run_w13.sh: same tasks, -n 1, timeout multiplier 2, same in-container proxy env.
set -eu
cd "$(dirname "$0")"
if [ "${EVAL_AUTHORIZED:-}" != 1 ]; then
  echo "refusing: a counted run needs owner authorization (plan §8); set EVAL_AUTHORIZED=1" >&2
  exit 2
fi
sitting=$1; shift
P=http://host.docker.internal:7897
ENDPOINT=http://host.docker.internal:18080/v1
TAGFILE=jobs/metering/tag
# Any HTTP answer will do (curl without -f exits 0 on 4xx); a refused connection will not.
curl -sS -o /dev/null -m 5 http://127.0.0.1:18080/ || { echo "metering proxy on 127.0.0.1:18080 is not answering" >&2; exit 2; }
tasks=""
for t in $(cat tb2-widened13.txt); do tasks="$tasks -i $t"; done
# Arguments shared by the Harbor built-in arms: same model id, endpoint and wire as the LC arms.
builtin="-m oi/deepseek-v4.1-flash --base-url $ENDPOINT --api-format openai_chat_completions \
  --api-key-name OPENAI_API_KEY --ae OPENAI_API_KEY=dummy-exp-key"
for arm in "$@"; do
  case $arm in
    LCpost) agent="--agent lcx_eval:LcxPost"; test -x build/bin/lcx-go-post ;;
    LCpre) agent="--agent lcx_eval:LcxPre"; test -x build/bin/lcx-go-Aplus ;;
    pi) agent="--agent pi --ak version=0.87.1 --ak model_api=openai-completions $builtin" ;;
    opencode) agent="--agent opencode_local:OpenCodeLocal $builtin" ;;
    specpi) agent="--agent specpi_agent:SpecPi033 --ak version=0.87.1 --ak model_api=openai-completions $builtin" ;;
    *) echo "unknown arm: $arm" >&2; exit 2 ;;
  esac
  job=cc-$arm-s$sitting
  printf '%s' "$arm/$job" > "$TAGFILE"
  echo "== $(date -u +%FT%TZ) $job"
  # shellcheck disable=SC2086 # word splitting of $tasks and $agent is intended
  PYTHONPATH=$PWD/adapters env -u all_proxy -u ALL_PROXY harbor run --path ./tb2/terminal-bench $tasks \
    $agent -n 1 -k 1 --timeout-multiplier 2 \
    --ae http_proxy=$P --ae https_proxy=$P --ae HTTP_PROXY=$P --ae HTTPS_PROXY=$P \
    --ae no_proxy=host.docker.internal,localhost,127.0.0.1 --ae NO_PROXY=host.docker.internal,localhost,127.0.0.1 \
    -o ./jobs --job-name "$job" -y || echo "harbor exited $? for $job (kept; classify, do not drop)" >&2
done
printf '%s' "idle" > "$TAGFILE"
