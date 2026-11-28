#!/usr/bin/env bash
#
# Off-vs-on benchmark of the composition-dynamic-controller's CPU-saturation gate
# (COMPOSITION_CONTROLLER_SATURATION_ENABLED) on a DISPOSABLE GKE cluster this script creates and
# deletes. See README.md for the question, the design and the cost.
#
#   ./run.sh                 create the cluster, run the sequence, tear the cluster down (trap on EXIT)
#   ./run.sh teardown NAME   delete a leftover sat-bench-* cluster in the benchmark project
#
# Never touches ~/.kube/config or the active kubectl context: every kubectl/helm/gcloud call uses the
# KUBECONFIG file below, which is exported before anything runs.
#
# Dev knobs (the published results used none of them but N/IDLE_SECS defaults):
#   REUSE_CLUSTER=sat-bench-xxxxxx  use that cluster, do not create or delete it (SKIP_INSTALL=1 ASSUME_ARM=off|on: also skip the install)
#   SEQUENCE="off on on off off on" N=150 IDLE_SECS=300 LIMIT=250m WORKERS=16
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT="integration-test-431120"
ZONE="${GKE_ZONE:-us-central1-a}"
MACHINE="${GKE_MACHINE:-e2-standard-4}"
NODES="${GKE_NODES:-3}"
GKE_VERSION="${GKE_VERSION:-1.36.4-gke.2377000}"
GKE_CHANNEL="${GKE_CHANNEL:-extended}"
CHART_VERSION="${CHART_VERSION:-2.13.135}"
N="${N:-150}"
LIMIT="${LIMIT:-250m}"
WORKERS="${WORKERS:-16}"
IDLE_SECS="${IDLE_SECS:-300}"
IDLE_GRACE="${IDLE_GRACE:-60}"
LIGHT_N="${LIGHT_N:-10}"
LIGHT_SECS="${LIGHT_SECS:-300}"
LIGHT_SETTLE="${LIGHT_SETTLE:-60}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
DELETE_TIMEOUT="${DELETE_TIMEOUT:-1800}"
SEQUENCE="${SEQUENCE:-off on on off off on}"
REUSE_CLUSTER="${REUSE_CLUSTER:-}"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="$HERE/results/$STAMP"
BENCH="$HERE/.bench"
KC="$BENCH/kubeconfig"
export KUBECONFIG="$KC"

NS_SYS="krateo-system"
PLURAL="satcharts.composition.krateo.io"
CDC="satcharts-v0-1-0-controller"
ENV_CM="satcharts-v0-1-0-configmap"
OCI="oci://ghcr.io/krateo-platformops/charts"

CLUSTER=""
CREATED=0
SAMPLER_PIDS=""

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }
die() { log "FATAL: $*"; exit 1; }

kube() { kubectl "$@"; }

stop_samplers() {
  local p
  for p in $SAMPLER_PIDS; do kill "$p" 2>/dev/null || true; done
  for p in $SAMPLER_PIDS; do wait "$p" 2>/dev/null || true; done
  SAMPLER_PIDS=""
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  stop_samplers
  if [ "$CREATED" = 1 ] && [ -z "$REUSE_CLUSTER" ]; then
    log "teardown: deleting cluster $CLUSTER"
    # KUBECONFIG stays pinned to the throwaway file, so gcloud prunes the context from it, not from ~/.kube/config.
    gcloud container clusters delete "$CLUSTER" --project "$PROJECT" --zone "$ZONE" --quiet || log "WARNING: delete of $CLUSTER failed; run ./run.sh teardown $CLUSTER"
  fi
  log "clusters in $PROJECT now:"
  gcloud container clusters list --project "$PROJECT" || true
  if [ -n "${KUBE_SUM_BEFORE:-}" ]; then
    local after
    after="$(shasum -a 256 "$HOME/.kube/config" | cut -d' ' -f1)"
    if [ "$after" = "$KUBE_SUM_BEFORE" ]; then log "$HOME/.kube/config: byte-identical before and after (sha256 $after)"; else log "$HOME/.kube/config: CHANGED (before $KUBE_SUM_BEFORE, after $after)"; fi
  fi
  exit "$rc"
}

if [ "${1:-}" = "teardown" ]; then
  name="${2:-}"
  case "$name" in sat-bench-*) ;; *) die "teardown takes a sat-bench-* cluster name" ;; esac
  gcloud container clusters delete "$name" --project "$PROJECT" --zone "$ZONE" --quiet
  gcloud container clusters list --project "$PROJECT"
  exit 0
fi

for tool in gcloud kubectl helm jq awk python3; do command -v "$tool" >/dev/null || die "$tool is required"; done

KUBE_SUM_BEFORE="$(shasum -a 256 "$HOME/.kube/config" | cut -d' ' -f1)"
log "$HOME/.kube/config sha256 before: $KUBE_SUM_BEFORE"
mkdir -p "$BENCH" "$OUT"
rm -f "$KC"
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------------------------- cluster

create_cluster() {
  if [ -n "$REUSE_CLUSTER" ]; then
    case "$REUSE_CLUSTER" in sat-bench-*) ;; *) die "REUSE_CLUSTER must be a sat-bench-* cluster" ;; esac
    CLUSTER="$REUSE_CLUSTER"
    log "reusing $CLUSTER (dev mode: it is not deleted by this run)"
  else
    CLUSTER="sat-bench-$(printf '%04x%02x' "$RANDOM" $((RANDOM % 256)))"
    CREATED=1
    log "creating GKE cluster $CLUSTER in $PROJECT/$ZONE: $NODES x $MACHINE, $GKE_VERSION"
    # KUBECONFIG is the throwaway file: `create` runs get-credentials implicitly and would otherwise
    # switch the caller's context.
    gcloud container clusters create "$CLUSTER" --project "$PROJECT" --zone "$ZONE" \
      --num-nodes "$NODES" --machine-type "$MACHINE" --disk-size 50 \
      --cluster-version "$GKE_VERSION" --release-channel "$GKE_CHANNEL" \
      --quiet
  fi
  gcloud container clusters get-credentials "$CLUSTER" --project "$PROJECT" --zone "$ZONE"
  guard_context
}

guard_context() {
  local ctx want
  ctx="$(kube config current-context)"
  want="gke_${PROJECT}_${ZONE}_${CLUSTER}"
  [ "$ctx" = "$want" ] || die "context is $ctx, want $want"
  case "$CLUSTER" in sat-bench-*) ;; *) die "cluster $CLUSTER is not a sat-bench-* cluster" ;; esac
}

# ---------------------------------------------------------------------------------------------- helpers

# settle DESCRIPTION SECONDS CMD...: wait until CMD succeeds, for at most SECONDS.
wait_for() {
  local what="$1" secs="$2" deadline
  shift 2
  deadline=$(( $(date -u +%s) + secs ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date -u +%s)" -lt "$deadline" ] || die "timed out after ${secs}s waiting for: $what"
    sleep 3
  done
}

one_core_provider() {
  local n
  n="$(kube -n "$NS_SYS" get pods -l app.kubernetes.io/name=core-provider --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  [ "$n" = 1 ]
}

cdc_stable() {
  local d
  d="$(kube -n "$NS_SYS" get deploy "$CDC" -o json)" || return 1
  [ "$(jq -r '.status.observedGeneration == .metadata.generation and (.status.replicas // 0) == 1 and (.status.readyReplicas // 0) == 1 and (.status.updatedReplicas // 0) == 1' <<<"$d")" = true ] || return 1
  [ "$(kube -n "$NS_SYS" get pods --no-headers 2>/dev/null | awk -v p="$CDC" 'index($1,p)==1' | wc -l | tr -d ' ')" = 1 ]
}

# held DESCRIPTION CMD...: the condition must hold for 20 consecutive seconds.
held() {
  local what="$1" since="" now
  shift
  local deadline=$(( $(date -u +%s) + 300 ))
  while [ "$(date -u +%s)" -lt "$deadline" ]; do
    now="$(date -u +%s)"
    if "$@" >/dev/null 2>&1; then
      [ -n "$since" ] || since="$now"
      [ $(( now - since )) -ge 20 ] && return 0
    else
      since=""
    fi
    sleep 2
  done
  die "never settled: $what"
}

cdc_pod() { kube -n "$NS_SYS" get pods -o json | jq -r --arg p "$CDC" '[.items[] | select(.metadata.name | startswith($p)) | select(.metadata.deletionTimestamp == null)] | if length == 1 then .[0].metadata.name else error("want one cdc pod, found \(length)") end'; }

collector_text() { kube get --raw /api/v1/namespaces/otel/services/otel:8889/proxy/metrics 2>/dev/null || true; }

# One cadvisor sample of the cdc container, as CSV: usage_s, pressure_waiting_s, periods, throttled_periods, throttled_s, working_set.
# container_pressure_cpu_waiting_seconds_total is the kubelet's reading of the container's cpu.pressure "some total".
cadvisor_sample() {
  local node="$1" pod="$2" ctr="$3"
  kube get --raw "/api/v1/nodes/$node/proxy/metrics/cadvisor" 2>/dev/null | awk -v pod="$pod" -v ctr="$ctr" '
    index($0, "pod=\"" pod "\"") && index($0, "container=\"" ctr "\"") {
      name = $0; sub(/\{.*/, "", name)
      line = $0; sub(/^[^}]*\} */, "", line); split(line, a, " ")
      v[name] = a[1]
    }
    END {
      printf "%s,%s,%s,%s,%s,%s\n", v["container_cpu_usage_seconds_total"], v["container_pressure_cpu_waiting_seconds_total"], \
        v["container_cpu_cfs_periods_total"], v["container_cpu_cfs_throttled_periods_total"], v["container_cpu_cfs_throttled_seconds_total"], v["container_memory_working_set_bytes"]
    }'
}

# One collector sample, as CSV: capacity, saturation input (%), input enabled, in-flight reconciles. Empty = not reported.
otel_sample() {
  collector_text | { grep 'job="composition-dynamic-controller"' || true; } | awk '
    function val(l) { sub(/^[^}]*\} */, "", l); split(l, a, " "); return a[1] }
    /^unstructured_runtime_ratelimit_local_capacity[{ ]/ { cap = val($0) }
    /^unstructured_runtime_ratelimit_saturation_input(_percent)?[{ ]/ { inp = val($0) }
    /^unstructured_runtime_ratelimit_saturation_input_enabled[{ ]/ { en = val($0) }
    /^unstructured_runtime_reconcile_in_flight[{ ]/ { fl = val($0) }
    END { printf "%s,%s,%s,%s\n", cap, inp, en, fl }'
}

sampler_loop() { # file, function, args...
  local file="$1"
  shift
  while :; do
    printf '%s,%s\n' "$(date -u +%s)" "$("$@" || true)" >>"$file"
    sleep "${SAMPLE_PERIOD:-5}"
  done
}

# ---------------------------------------------------------------------------------------------- install

install_stack() {
  log "installing core-provider $CHART_VERSION (crds first) into $NS_SYS"
  helm install core-provider-crds "$OCI/core-provider-crds" --version "$CHART_VERSION" -n "$NS_SYS" --create-namespace --wait --timeout 5m >/dev/null
  kube apply -f "$HERE/otel-collector.yaml"
  kube apply -f "$HERE/chartrepo.yaml"
  helm package "$HERE/chart" -d "$BENCH" >/dev/null
  kube -n chartrepo create configmap satchart --from-file="$BENCH/satchart-0.1.0.tgz"
  helm install core-provider "$OCI/core-provider" --version "$CHART_VERSION" -n "$NS_SYS" \
    -f "$HERE/values-base.yaml" \
    --set-string "cdc.env.COMPOSITION_CONTROLLER_WORKERS=$WORKERS" \
    --set "cdc.resources.limits.cpu=$LIMIT" \
    --set-string cdc.env.COMPOSITION_CONTROLLER_SATURATION_ENABLED=false \
    --wait --timeout 10m >/dev/null
  wait_for "otel collector" 300 kube -n otel rollout status deploy/otel --timeout=5s
  wait_for "chartrepo" 300 kube -n chartrepo rollout status deploy/chartrepo --timeout=5s
  kube apply -f "$HERE/compositiondefinition.yaml"
  wait_for "the CompositionDefinition Ready" 900 kube -n bench wait --for=condition=Ready compositiondefinition/satchart --timeout=5s
  wait_for "the cdc Deployment" 600 cdc_stable
  # The instrument must see PSI at all, or the whole comparison is vacuous.
  local node pod
  pod="$(cdc_pod)"
  node="$(kube -n "$NS_SYS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
  local n
  n="$(kube get --raw "/api/v1/nodes/$node/proxy/metrics/cadvisor" | grep -c '^container_pressure_cpu_waiting_seconds_total' || true)"
  log "preflight: cadvisor exposes container_pressure_cpu_waiting_seconds_total for $n series on $node"
  [ "$n" -gt 0 ] || die "the kubelet exports no cpu pressure: PSI is unavailable on this node image, the gate would be disabled"
  kube version -o json | jq -c '{server: .serverVersion.gitVersion}' | tee "$OUT/cluster-version.json"
  kube get nodes -o json | jq -c '[.items[] | {name: .metadata.name, type: .metadata.labels["node.kubernetes.io/instance-type"], os: .status.nodeInfo.osImage, kernel: .status.nodeInfo.kernelVersion, runtime: .status.nodeInfo.containerRuntimeVersion}]' | tee "$OUT/nodes.json"
}

# ---------------------------------------------------------------------------------------------- arms

CURRENT_ARM=""

set_arm() {
  local arm="$1" want got lim
  if [ "$arm" = on ]; then want=true; else want=false; fi
  if [ "$CURRENT_ARM" != "$arm" ]; then
    log "arm -> $arm: helm upgrade with COMPOSITION_CONTROLLER_SATURATION_ENABLED=$want"
    helm upgrade core-provider "$OCI/core-provider" --version "$CHART_VERSION" -n "$NS_SYS" \
      -f "$HERE/values-base.yaml" \
      --set-string "cdc.env.COMPOSITION_CONTROLLER_WORKERS=$WORKERS" \
      --set "cdc.resources.limits.cpu=$LIMIT" \
      --set-string "cdc.env.COMPOSITION_CONTROLLER_SATURATION_ENABLED=$want" \
      --wait --timeout 5m >/dev/null
    # The old core-provider replica must be gone, or two engines reconcile the cdc during the upgrade.
    held "one core-provider pod" one_core_provider
    local deadline=$(( $(date -u +%s) + 300 ))
    while :; do
      got="$(kube -n "$NS_SYS" get cm "$ENV_CM" -o json 2>/dev/null | jq -r '.data.COMPOSITION_CONTROLLER_SATURATION_ENABLED // "unset"')"
      lim="$(kube -n "$NS_SYS" get deploy "$CDC" -o json 2>/dev/null | jq -r '.spec.template.spec.containers[0].resources.limits.cpu // "none"')"
      [ "$got" = "$want" ] && [ "$lim" = "$LIMIT" ] && break
      [ "$(date -u +%s)" -lt "$deadline" ] || die "arm never applied: env=$got want $want, cpu limit=$lim want $LIMIT"
      sleep 5
    done
    CURRENT_ARM="$arm"
  fi
}

restart_for_run() {
  kube -n "$NS_SYS" rollout restart "deploy/$CDC" >/dev/null
  wait_for "the cdc rollout" 600 kube -n "$NS_SYS" rollout status "deploy/$CDC" --timeout=5s
  held "the cdc Deployment, one pod, no rollout" cdc_stable
  # A fresh collector AFTER the old cdc pod is gone: it keeps a series for 10m, so an off arm would read the
  # previous on arm's last gauge values, and the old pod's counters (same `instance` label as the new pod's)
  # would otherwise be overwritten by the new pod's, which made one burst's reconcile delta negative.
  kube -n otel rollout restart deploy/otel >/dev/null
  wait_for "otel collector restart" 300 kube -n otel rollout status deploy/otel --timeout=5s
}

compositions_yaml() { # namespace count
  local i
  for i in $(seq 1 "$2"); do
    printf -- '---\napiVersion: composition.krateo.io/v0-1-0\nkind: Satchart\nmetadata:\n  name: bench-%03d\n  namespace: %s\nspec:\n  marker: "run"\n' "$i" "$1"
  done
}

ready_count() { # namespace
  kube -n "$1" get "$PLURAL" -o json | jq '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length'
}

delete_run() { # namespace
  local ns="$1" before objs deps cms deadline
  before="$(kube -n "$ns" get "$PLURAL" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  log "cleanup: deleting $before compositions in $ns and waiting for their releases to go"
  kube -n "$ns" delete "$PLURAL" --all --wait=false >/dev/null
  deadline=$(( $(date -u +%s) + DELETE_TIMEOUT ))
  while :; do
    objs="$(kube -n "$ns" get "$PLURAL" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    deps="$(kube -n "$ns" get deploy -l app.kubernetes.io/part-of=satchart --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    cms="$(kube -n "$ns" get cm -l app.kubernetes.io/part-of=satchart --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    [ "$objs" = 0 ] && [ "$deps" = 0 ] && [ "$cms" = 0 ] && break
    [ "$(date -u +%s)" -lt "$deadline" ] || die "cleanup of $ns did not finish: $objs compositions, $deps deployments, $cms configmaps left"
    sleep 10
  done
  log "cleanup: $before compositions gone, 0 chart deployments and configmaps left"
  kube delete ns "$ns" --wait=true >/dev/null
}

run_one() { # index arm
  local idx="$1" arm="$2" dir ns pod node ctr digest t0 ready_n deadline t_ready timed_out=0
  dir="$OUT/run-$idx-$arm"
  ns="run-$idx"
  mkdir -p "$dir"
  guard_context
  set_arm "$arm"
  restart_for_run
  pod="$(cdc_pod)"
  node="$(kube -n "$NS_SYS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
  ctr="$(kube -n "$NS_SYS" get pod "$pod" -o jsonpath='{.spec.containers[0].name}')"
  digest="$(kube -n "$NS_SYS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].imageID}')"
  log "run $idx ($arm): pod $pod on $node, image $digest; settling 45s past the gate's startup ramp"
  sleep 45
  if [ "$arm" = on ]; then
    local pre
    pre="$(otel_sample)"
    log "run $idx pre-burst gate sample (capacity,input,enabled,in_flight): $pre"
    case "$pre" in "$WORKERS",*) ;; *) log "WARNING: the gate's capacity is not at workers=$WORKERS before the burst: $pre" ;; esac
  fi

  printf 'epoch,usage_s,pressure_waiting_s,periods,throttled_periods,throttled_s,working_set_bytes\n' >"$dir/cadvisor.csv"
  printf 'epoch,capacity,input_pct,input_enabled,in_flight\n' >"$dir/gate.csv"
  collector_text | grep '^unstructured_runtime_' >"$dir/prom-start.txt" || true

  kube create ns "$ns" >/dev/null
  t0="$(date -u +%s)"
  log "run $idx: creating $N compositions in $ns at $t0"
  compositions_yaml "$ns" "$N" | kube create -f - >/dev/null
  SAMPLE_PERIOD=5 sampler_loop "$dir/cadvisor.csv" cadvisor_sample "$node" "$pod" "$ctr" &
  SAMPLER_PIDS="$SAMPLER_PIDS $!"
  SAMPLE_PERIOD=2 sampler_loop "$dir/gate.csv" otel_sample &
  SAMPLER_PIDS="$SAMPLER_PIDS $!"

  deadline=$(( t0 + READY_TIMEOUT ))
  ready_n=0
  while :; do
    ready_n="$(ready_count "$ns" || echo 0)"
    [ "$ready_n" = "$N" ] && break
    if [ "$(date -u +%s)" -ge "$deadline" ]; then timed_out=1; log "run $idx: TIMEOUT with $ready_n of $N Ready"; break; fi
    sleep 5
  done
  t_ready="$(date -u +%s)"
  log "run $idx: $ready_n of $N Ready after $(( t_ready - t0 ))s (timed_out=$timed_out)"
  kube -n "$ns" get "$PLURAL" -o json | jq '{first_create: ([.items[].metadata.creationTimestamp | fromdateiso8601] | min), last_ready: ([.items[].status.conditions[]? | select(.type == "Ready" and .status == "True") | .lastTransitionTime | fromdateiso8601] | max), ready: ([.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length), total: (.items | length)}' >"$dir/ready.json"
  collector_text | grep '^unstructured_runtime_' >"$dir/prom-ready.txt" || true

  log "run $idx: idle phase, $IDLE_SECS s with $ready_n compositions at rest"
  sleep "$IDLE_SECS"
  collector_text | grep '^unstructured_runtime_' >"$dir/prom-end.txt" || true
  local t_end
  t_end="$(date -u +%s)"
  delete_run "$ns"

  # Light phase: the 150 at rest are NOT idle at this limit (their resync alone saturates 250m), so the
  # false-throttle question (d) is asked of a small fleet whose resync fits well inside the limit.
  local lns="run-$idx-light" t_light_start t_light_end light_ready ldeadline
  log "run $idx: light phase, settling ${LIGHT_SETTLE}s after the cleanup, then $LIGHT_N compositions"
  sleep "$LIGHT_SETTLE"
  kube create ns "$lns" >/dev/null
  compositions_yaml "$lns" "$LIGHT_N" | kube create -f - >/dev/null
  ldeadline=$(( $(date -u +%s) + 900 ))
  until [ "$(ready_count "$lns" || echo 0)" = "$LIGHT_N" ]; do
    [ "$(date -u +%s)" -lt "$ldeadline" ] || { log "run $idx: light phase TIMEOUT"; break; }
    sleep 5
  done
  light_ready="$(ready_count "$lns" || echo 0)"
  sleep "$IDLE_GRACE"
  t_light_start="$(date -u +%s)"
  collector_text | grep '^unstructured_runtime_' >"$dir/prom-light-start.txt" || true
  log "run $idx: light phase: $light_ready of $LIGHT_N Ready, sampling ${LIGHT_SECS}s"
  sleep "$LIGHT_SECS"
  collector_text | grep '^unstructured_runtime_' >"$dir/prom-light-end.txt" || true
  t_light_end="$(date -u +%s)"
  stop_samplers

  kube -n "$NS_SYS" logs "$pod" >"$dir/cdc.log" 2>&1 || true
  {
    echo "total_lines $(wc -l <"$dir/cdc.log" | tr -d ' ')"
    grep -i 'lowering local capacity\|saturation input disabled' "$dir/cdc.log" || true
  } >"$dir/gate-log.txt"
  kube -n "$NS_SYS" get pod "$pod" -o json | jq -c '{restarts: ([.status.containerStatuses[].restartCount] | add), lastState: [.status.containerStatuses[].lastState], oomkilled: ([.status.containerStatuses[].lastState.terminated.reason? // empty] | any(. == "OOMKilled"))}' >"$dir/pod-status.json"
  kube -n "$NS_SYS" get events --field-selector "involvedObject.name=$pod" -o json | jq -c '[.items[] | {reason, message}]' >"$dir/pod-events.json" || true
  jq -n --arg idx "$idx" --arg arm "$arm" --arg pod "$pod" --arg node "$node" --arg digest "$digest" --arg chart "$CHART_VERSION" \
    --argjson n "$N" --arg limit "$LIMIT" --argjson workers "$WORKERS" --argjson t0 "$t0" --argjson t_ready "$t_ready" --argjson t_end "$t_end" \
    --argjson idle_secs "$IDLE_SECS" --argjson idle_grace "$IDLE_GRACE" --argjson timed_out "$timed_out" --argjson ready "$ready_n" \
    --argjson light_n "$LIGHT_N" --argjson light_ready "$light_ready" --argjson t_light_start "$t_light_start" --argjson t_light_end "$t_light_end" \
    '{run: ($idx | tonumber), arm: $arm, pod: $pod, node: $node, image_id: $digest, chart: $chart, n: $n, cpu_limit: $limit, workers: $workers, t0: $t0, t_ready_poll: $t_ready, t_end: $t_end, idle_secs: $idle_secs, idle_grace: $idle_grace, timed_out: ($timed_out == 1), ready: $ready, light_n: $light_n, light_ready: $light_ready, t_light_start: $t_light_start, t_light_end: $t_light_end}' >"$dir/meta.json"

  delete_run "$lns"
}

# ---------------------------------------------------------------------------------------------- main

create_cluster
if [ -z "${SKIP_INSTALL:-}" ]; then install_stack; else log "SKIP_INSTALL set (dev): reusing the installed stack"; CURRENT_ARM="${ASSUME_ARM:-}"; fi
log "sequence: $SEQUENCE (N=$N, cpu limit $LIMIT, workers $WORKERS, idle ${IDLE_SECS}s)"
idx=0
for arm in $SEQUENCE; do
  idx=$((idx + 1))
  run_one "$idx" "$arm"
done
python3 -I "$HERE/analyze.py" "$OUT" | tee "$OUT/summary.md"
log "done: raw data and summary in $OUT"
