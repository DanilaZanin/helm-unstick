#!/usr/bin/env bash
# End-to-end tests for helm-unstick on a kind cluster.
#
# The Helm CLI under test creates the stuck states (helm ... --wait, then kill -9);
# helm-unstick then has to recover them. Run it once per Helm generation:
#   HELM=/path/to/helm3 test/e2e.sh
#   HELM=/path/to/helm4 test/e2e.sh
#
# Environment:
#   HELM                     Helm CLI to test (default: helm from PATH)
#   UNSTICK                  binary under test (default: bin/helm-unstick, build it with make build)
#   E2E_USE_CURRENT_CONTEXT  1 = use the current kube context, do not create or delete a cluster
#   E2E_CLUSTER              kind cluster name (default: helm-unstick-e2e)
#   KEEP_CLUSTER             1 = keep the kind cluster afterwards
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
UNSTICK=${UNSTICK:-$ROOT/bin/helm-unstick}
HELM=${HELM:-helm}
CLUSTER=${E2E_CLUSTER:-helm-unstick-e2e}
USE_CURRENT=${E2E_USE_CURRENT_CONTEXT:-0}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}
IMAGE=busybox:1.36 # the image the test chart uses, see test/chart/values.yaml
CHART=$ROOT/test/chart
WORK=$(mktemp -d "${TMPDIR:-/tmp}/helm-unstick-e2e.XXXXXX")
CREATED_CLUSTER=0
BG_PID=""
FINDINGS=()

log()  { printf '\n=== %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
finding() { FINDINGS+=("$*"); info "FINDING: $*"; }
die()  { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck disable=SC2329  # invoked through the EXIT trap
cleanup() {
  local code=$?
  if [[ -n "$BG_PID" ]]; then kill -9 "$BG_PID" 2>/dev/null || true; fi
  if [[ $CREATED_CLUSTER == 1 && $KEEP_CLUSTER != 1 ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  exit "$code"
}
trap cleanup EXIT

need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }
need kubectl
need jq
need "$HELM"
[[ -x "$UNSTICK" ]] || die "binary not found: $UNSTICK (run make build)"

# run CMD...: capture stdout in OUT, stderr in ERR and the exit code in RC without tripping set -e.
run() {
  local errfile
  errfile=$(mktemp "$WORK/err.XXXXXX")
  set +e
  OUT=$("$@" 2>"$errfile")
  RC=$?
  set -e
  ERR=$(cat "$errfile")
}

expect_rc() { # WANT LABEL
  [[ "$RC" == "$1" ]] || die "$2: exit code $RC, want $1
--- stdout
$OUT
--- stderr
$ERR"
}

expect_out() { # PATTERN LABEL
  grep -qF -- "$1" <<<"$OUT$ERR" || die "$2: output does not contain '$1'
--- stdout
$OUT
--- stderr
$ERR"
}

wait_for() { # SECONDS DESCRIPTION CMD...
  local timeout=$1 what=$2 waited=0
  shift 2
  until "$@" >/dev/null 2>&1; do
    if ((waited >= timeout)); then die "timed out after ${timeout}s waiting for $what"; fi
    sleep 1
    waited=$((waited + 1))
  done
}

# ---------------------------------------------------------------- cluster

if [[ $USE_CURRENT != 1 ]]; then
  need kind
  # A private kubeconfig keeps the user's own one untouched.
  export KUBECONFIG=$WORK/kubeconfig
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    info "reusing kind cluster $CLUSTER"
    kind get kubeconfig --name "$CLUSTER" >"$KUBECONFIG"
  else
    log "creating kind cluster $CLUSTER"
    kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 180s
    CREATED_CLUSTER=1
  fi
fi

if command -v kind >/dev/null 2>&1 && command -v docker >/dev/null 2>&1 &&
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  info "preloading $IMAGE into the kind nodes"
  docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
  if ! kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null 2>&1; then
    # kind load fails for multi-platform images kept by the Docker containerd store.
    # Fall back to a pull inside the node through a mirror, then give it the expected name.
    info "kind load failed, pulling $IMAGE inside the node through mirror.gcr.io"
    node=$(kind get nodes --name "$CLUSTER" | head -n1)
    docker exec "$node" crictl pull "mirror.gcr.io/library/$IMAGE" >/dev/null
    docker exec "$node" ctr -n k8s.io images tag "mirror.gcr.io/library/$IMAGE" "docker.io/library/$IMAGE" >/dev/null
  fi
fi

log "environment"
info "helm:      $("$HELM" version --short 2>&1)"
info "unstick:   $("$UNSTICK" version)"
info "context:   $(kubectl config current-context)"

# ---------------------------------------------------------------- helpers

new_namespace() { kubectl create namespace "$1" >/dev/null; }
drop_namespace() { kubectl delete namespace "$1" --wait=false >/dev/null 2>&1 || true; }

# status of the newest revision
latest_status() { "$HELM" history "$2" -n "$1" -o json | jq -r 'max_by(.revision).status'; }

wait_status() { # NS RELEASE STATUS SECONDS
  wait_for "$4" "$2 to reach status $3" bash -c "[[ \$('$HELM' history '$2' -n '$1' -o json | jq -r 'max_by(.revision).status') == '$3' ]]"
}

# annotation of the pod template that the Deployment currently carries
deployed_rollout() { kubectl -n "$1" get deploy "$2" -o jsonpath='{.spec.template.metadata.annotations.rollout}'; }

wait_rollout_annotation() { # NS RELEASE VALUE
  wait_for 90 "deployment $2 to carry rollout=$3" bash -c "[[ \$(kubectl -n '$1' get deploy '$2' -o jsonpath='{.spec.template.metadata.annotations.rollout}' 2>/dev/null) == '$3' ]]"
}

start_bg() { # runs the command in the background; BG_PID is its PID
  "$@" >"$WORK/bg.log" 2>&1 &
  BG_PID=$!
}

kill_bg() { # simulate a CI job killed by a timeout or an evicted runner
  kill -9 "$BG_PID" 2>/dev/null || true
  wait "$BG_PID" 2>/dev/null || true
  BG_PID=""
}

expect_helm_blocked() { # NS RELEASE: the stuck state must be real
  run "$HELM" upgrade "$2" "$CHART" -n "$1" --set rollout=blocked
  [[ "$RC" != 0 ]] || die "helm upgrade succeeded on a release that should be stuck"
  if grep -qi "another operation" <<<"$OUT$ERR"; then
    info "helm refuses as expected: $(grep -i 'another operation' <<<"$OUT$ERR" | head -n1)"
  else
    info "WARN: helm failed, but not with the usual 'another operation ... in progress' text:"
    info "$ERR"
  fi
}

expect_helm_works() { # NS RELEASE ROLLOUT
  run "$HELM" upgrade --install "$2" "$CHART" -n "$1" --wait --timeout 3m --set "rollout=$3"
  expect_rc 0 "helm upgrade after recovery"
  [[ "$(latest_status "$1" "$2")" == deployed ]] || die "$2 is not deployed after the follow-up upgrade"
}

# scan_stuck NS: scan one namespace with a 0s threshold; sets OUT, RC and VERDICT (of the first row).
scan_stuck() {
  run "$UNSTICK" scan -n "$1" --older-than 0s -o json
  VERDICT=$(jq -r '.[0].verdict // "none"' <<<"$OUT")
}

pass() { printf 'PASS: %s\n' "$*"; }

# ---------------------------------------------------------------- scenario 1

# Interrupted upgrade: the CI job dies while helm upgrade --wait is waiting for readiness.
# $1 = strategy for helm-unstick fix
scenario_interrupted_upgrade() {
  local strategy=$1 ns="e2e-s1-$1" rel=web
  log "scenario 1 ($strategy): interrupted upgrade"
  new_namespace "$ns"
  "$HELM" install "$rel" "$CHART" -n "$ns" --wait --timeout 3m --set rollout=1 >/dev/null

  start_bg "$HELM" upgrade "$rel" "$CHART" -n "$ns" --wait --timeout 10m --set rollout=2 --set readinessDelay=20
  wait_status "$ns" "$rel" pending-upgrade 60
  wait_rollout_annotation "$ns" "$rel" 2
  kill_bg
  [[ "$(latest_status "$ns" "$rel")" == pending-upgrade ]] || die "expected the release to be left in pending-upgrade"
  expect_helm_blocked "$ns" "$rel"

  kubectl -n "$ns" rollout status "deploy/$rel" --timeout=180s >/dev/null
  scan_stuck "$ns"
  [[ "$VERDICT" == stale ]] || die "scenario 1: expected verdict stale, got: $OUT"
  expect_rc 2 "scan exit code for a stuck release"
  pass "scan reports the interrupted upgrade as stale"

  run "$UNSTICK" fix "$rel" -n "$ns" --older-than 0s --yes --strategy "$strategy"
  case $strategy in
    direct)
      if [[ "$RC" == 0 ]]; then
        finding "helm $HELM_MAJOR: a direct SDK rollback works on a pending-upgrade revision"
      else
        finding "helm $HELM_MAJOR: a direct SDK rollback is refused on a pending-upgrade revision: $(head -n1 <<<"$ERR")"
        [[ "$(latest_status "$ns" "$rel")" == pending-upgrade ]] || die "a refused direct rollback changed the release"
        run "$UNSTICK" fix "$rel" -n "$ns" --older-than 0s --yes --strategy mark-failed
        expect_rc 0 "fix --strategy mark-failed after a refused direct rollback"
      fi
      ;;
    *)
      expect_rc 0 "fix --strategy $strategy"
      finding "helm $HELM_MAJOR: fix --strategy $strategy: $(grep '^Rollback path' <<<"$OUT")"
      ;;
  esac
  [[ "$(latest_status "$ns" "$rel")" == deployed ]] || die "release is not deployed after fix"
  [[ "$(deployed_rollout "$ns" "$rel")" == 1 ]] || die "fix did not restore revision 1 (rollout annotation is $(deployed_rollout "$ns" "$rel"))"
  if "$HELM" history "$rel" -n "$ns" -o json | jq -e 'map(select(.status | startswith("pending"))) | length > 0' >/dev/null; then
    die "a pending revision is still listed in the history"
  fi
  pass "fix rolled back to revision 1"

  expect_helm_works "$ns" "$rel" 3
  pass "the next helm upgrade goes through"
  drop_namespace "$ns"
}

# ---------------------------------------------------------------- scenario 2

# A live, slow upgrade must be left alone.
scenario_live_upgrade() {
  local ns=e2e-s2 rel=web
  log "scenario 2: live slow upgrade"
  new_namespace "$ns"
  "$HELM" install "$rel" "$CHART" -n "$ns" --wait --timeout 3m --set rollout=1 >/dev/null

  start_bg "$HELM" upgrade "$rel" "$CHART" -n "$ns" --wait --timeout 10m --set rollout=2 --set readinessDelay=120
  wait_status "$ns" "$rel" pending-upgrade 60
  wait_rollout_annotation "$ns" "$rel" 2
  wait_for 60 "helm-unstick to see the rollout" bash -c "'$UNSTICK' scan -n '$ns' --older-than 0s | grep -q possibly-running"

  scan_stuck "$ns"
  [[ "$VERDICT" == possibly-running ]] || die "scenario 2: expected possibly-running, got: $OUT"
  grep -qF "rollout in progress" <<<"$OUT" || die "scenario 2: the verdict must name the rollout: $OUT"
  pass "scan reports possibly-running because of the rollout, not the age (threshold 0s)"

  run "$UNSTICK" fix "$rel" -n "$ns" --older-than 0s --yes
  expect_rc 3 "fix on a live release"
  expect_out "Refused" "fix on a live release"
  run "$UNSTICK" fix "$rel" -n "$ns" --older-than 0s --yes --force-unknown
  expect_rc 3 "fix --force-unknown on a live release"
  [[ "$(latest_status "$ns" "$rel")" == pending-upgrade ]] || die "fix touched a live release"
  pass "fix refuses with exit 3, and --force-unknown does not override it"

  run "$UNSTICK" scan -n "$ns"
  expect_out possibly-running "scan with the default threshold"
  pass "the default 10m threshold also protects it"

  kill_bg
  drop_namespace "$ns"
}

# ---------------------------------------------------------------- scenario 3

# First install interrupted: there is nothing to roll back to.
# $1 = mark-failed | uninstall
scenario_interrupted_install() {
  local mode=$1 ns="e2e-s3-$1" rel=fresh
  log "scenario 3 ($mode): interrupted first install"
  new_namespace "$ns"
  start_bg "$HELM" install "$rel" "$CHART" -n "$ns" --wait --timeout 10m --set readinessDelay=20
  wait_status "$ns" "$rel" pending-install 60
  wait_for 60 "the Deployment to exist" kubectl -n "$ns" get "deploy/$rel"
  kill_bg
  expect_helm_blocked "$ns" "$rel"
  kubectl -n "$ns" rollout status "deploy/$rel" --timeout=180s >/dev/null

  run "$UNSTICK" fix "$rel" -n "$ns" --older-than 0s --yes
  expect_rc 2 "fix without --first-install"
  expect_out "--first-install=mark-failed" "the plan"
  expect_out "--first-install=uninstall" "the plan"
  expect_out "Nothing was changed" "the plan"
  [[ "$(latest_status "$ns" "$rel")" == pending-install ]] || die "fix without --first-install changed the release"
  pass "fix without --first-install prints the plan and changes nothing"

  run "$UNSTICK" fix "$rel" -n "$ns" --older-than 0s --yes --first-install "$mode"
  expect_rc 0 "fix --first-install $mode"
  if [[ $mode == mark-failed ]]; then
    [[ "$(latest_status "$ns" "$rel")" == failed ]] || die "revision is not failed after mark-failed"
    expect_helm_works "$ns" "$rel" 2
    pass "after mark-failed, helm upgrade --install goes through"
  else
    if "$HELM" status "$rel" -n "$ns" >/dev/null 2>&1; then die "release still exists after uninstall"; fi
    if kubectl -n "$ns" get "deploy/$rel" >/dev/null 2>&1; then die "the Deployment survived the uninstall"; fi
    run "$HELM" install "$rel" "$CHART" -n "$ns" --wait --timeout 3m
    expect_rc 0 "helm install after uninstall"
    pass "after uninstall, helm install goes through"
  fi
  drop_namespace "$ns"
}

# ---------------------------------------------------------------- scenario 4

# Interrupted rollback: pending-rollback.
scenario_interrupted_rollback() {
  local ns=e2e-s4 rel=web
  log "scenario 4: interrupted rollback"
  new_namespace "$ns"
  "$HELM" install "$rel" "$CHART" -n "$ns" --wait --timeout 3m --set rollout=1 --set readinessDelay=20 >/dev/null
  "$HELM" upgrade "$rel" "$CHART" -n "$ns" --wait --timeout 3m --set rollout=2 --set readinessDelay=0 >/dev/null

  start_bg "$HELM" rollback "$rel" 1 -n "$ns" --wait --timeout 10m
  wait_status "$ns" "$rel" pending-rollback 60
  wait_rollout_annotation "$ns" "$rel" 1
  kill_bg
  expect_helm_blocked "$ns" "$rel"
  kubectl -n "$ns" rollout status "deploy/$rel" --timeout=180s >/dev/null

  scan_stuck "$ns"
  [[ "$VERDICT" == stale ]] || die "scenario 4: expected stale, got: $OUT"
  expect_out "pending-rollback" "scan output"
  run "$UNSTICK" fix "$rel" -n "$ns" --older-than 0s --yes
  expect_rc 0 "fix on pending-rollback"
  finding "helm $HELM_MAJOR: pending-rollback: $(grep '^Rollback path' <<<"$OUT")"
  [[ "$(latest_status "$ns" "$rel")" == deployed ]] || die "release is not deployed after fix"
  [[ "$(deployed_rollout "$ns" "$rel")" == 2 ]] || die "fix did not return to the last deployed revision (rollout=$(deployed_rollout "$ns" "$rel"), want 2)"
  pass "fix returned to the last deployed revision"

  expect_helm_works "$ns" "$rel" 5
  pass "the next helm upgrade goes through"
  drop_namespace "$ns"
}

# ---------------------------------------------------------------- run

HELM_MAJOR=$("$HELM" version --short 2>&1 | sed -n 's/^v\([0-9]*\)\..*/\1/p' | head -n1)
HELM_MAJOR=${HELM_MAJOR:-unknown}

scenario_interrupted_upgrade auto
scenario_interrupted_upgrade mark-failed
scenario_interrupted_upgrade direct
scenario_live_upgrade
scenario_interrupted_install mark-failed
scenario_interrupted_install uninstall
scenario_interrupted_rollback

log "findings (paste into the README section on rollback behavior)"
for f in ${FINDINGS[@]+"${FINDINGS[@]}"}; do printf '  - %s\n' "$f"; done
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    printf '### helm-unstick e2e, Helm %s\n\n' "$HELM_MAJOR"
    for f in ${FINDINGS[@]+"${FINDINGS[@]}"}; do printf -- '- %s\n' "$f"; done
  } >>"$GITHUB_STEP_SUMMARY"
fi

log "all scenarios passed on Helm $HELM_MAJOR"
exit 0
