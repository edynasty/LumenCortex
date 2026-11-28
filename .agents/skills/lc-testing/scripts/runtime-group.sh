#!/usr/bin/env bash
# runtime-group.sh - the `runtime` package's test groups, defined by test file, as `go test -run`
# regexes. `runtime` holds ~670 serial top-level tests (~11 min idle); L2 runs the group(s) of the
# change instead of the whole package. Groups follow the production file stems.
#
# Usage (from the Core worktree root):
#   runtime-group.sh <Group>...   print one -run regex covering the groups' tests
#   runtime-group.sh --list       print group names and their file globs
#   runtime-group.sh --check [REF]  list runtime test files no group covers, in the checkout or at
#                                   commit REF (exit 1 if any; run it when adding a test file)
#
#   go test -count=1 -timeout=20m -run "$(runtime-group.sh Context Provider)" ./runtime
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# the module root: the Git worktree, else the current directory (a `git archive` export of a commit)
root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
[[ -d $root/runtime ]] || { echo "runtime-group.sh: no runtime/ under $root; run from the Core root" >&2; exit 2; }
cd "$root/runtime"

groups=(Prompt Provider Context Cognitive Persistence Lifecycle Process Worktree MCP Debugger Eval Hub)
globs() {
  case $1 in
    Prompt)      echo "prompt_test.go agents_test.go catalog_test.go capabilit*_test.go interactive_test.go models_test.go schema_test.go v2_prompt*_test.go" ;;
    Provider)    echo "auto_*_test.go astra_provider_error_test.go request_envelope_test.go echo_test.go e2e_provenance_test.go review_metering_test.go cache_*_test.go" ;;
    Context)     echo "context_*_test.go projection_test.go window_test.go taskstate_test.go threads_test.go messages_test.go readback_test.go receipt_*_test.go v2_note*_test.go" ;;
    Cognitive)   echo "cognition*_test.go cognitive*_test.go experience*_test.go governor*_test.go checks_test.go verification_*_test.go derived_test.go constraints*_test.go" ;;
    Persistence) echo "statedir_test.go globaldir_test.go state_test.go session_*_test.go deletion*_test.go journal_whitelist_test.go metadata_ownership_test.go txcount_engine_test.go commitcount_test.go run_id_test.go" ;;
    Lifecycle)   echo "agent_test.go agent_intelligence*_test.go lifecycle*_test.go checkpoint_resume_test.go crash_combined_unix_test.go execution_crash_unix_test.go execution_retry_test.go execution_test.go request_crash_unix_test.go runlock*_test.go revoke*_test.go start_*_test.go supervisor_test.go delivery*_test.go bench003_timeout_test.go jobgate_test.go engine_test.go events_test.go runtime_events_test.go approvals_test.go tasks_test.go" ;;
    Process)     echo "jobs*_test.go schedules_test.go crossprocess_unix_test.go command_hook_test.go hooks_platform_test.go" ;;
    Worktree)    echo "worktree*_test.go editing*_test.go git_cost_test.go readexits_test.go" ;;
    MCP)         echo "mcp_*_test.go plugins_test.go" ;;
    Debugger)    echo "dap*_test.go agent_debugger_test.go inspector_test.go lsp_test.go diagnostics_test.go" ;;
    Eval)        echo "eval*_test.go notebooks*_test.go execution_notebooks*_test.go xreview_process_*_test.go" ;;
    Hub)         echo "hub*_test.go subagents_test.go delegation_agents_test.go xreview_hub*_test.go" ;;
    *) echo "unknown group $1 (groups: ${groups[*]})" >&2; exit 2 ;;
  esac
}
# Files that hold no selectable test: shared fixtures, a measurement gated by LCX_T1_PERF, Windows-only.
ungrouped="testutil_test.go t1_perf_test.go jobs_windows_test.go"

case ${1:-} in
  ""|-h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
  --list) for g in "${groups[@]}"; do printf '%-12s %s\n' "$g" "$(globs "$g")"; done; exit 0 ;;
  --check)
    if [[ -n ${2:-} ]]; then
      all=$(git -C "$root" ls-tree --name-only "$2" runtime/ | sed -n 's#^runtime/\(.*_test\.go\)$#\1#p')
    else
      all=$(ls *_test.go)
    fi
    missing=""
    while IFS= read -r f; do
      [[ -z $f ]] && continue
      hit=""
      for g in "${groups[@]}"; do
        set -f; for p in $(globs "$g") $ungrouped; do
          # shellcheck disable=SC2053
          [[ $f == $p ]] && { hit=1; break; }
        done; set +f
        [[ -n $hit ]] && break
      done
      [[ -z $hit ]] && missing+="$f"$'\n'
    done <<<"$all"
    if [[ -n $missing ]]; then printf 'runtime test files in no group (%s):\n%s' "${2:-checkout}" "$missing"; exit 1; fi
    echo "every runtime test file is in a group (${2:-checkout})"; exit 0 ;;
esac

shopt -s nullglob
files=()
for g in "$@"; do for p in $(globs "$g"); do for f in $p; do files+=("runtime/$f"); done; done; done
((${#files[@]})) || { echo "runtime-group.sh: no files for: $*" >&2; exit 1; }
cd "$root"
"$here/run-regex.sh" "${files[@]}"
