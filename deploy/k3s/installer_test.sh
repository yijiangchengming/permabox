#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=deploy/k3s/installer.sh
source ./installer.sh

reconcile() ( reconcile_node once )

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export HOST_ROOT="$tmp/host" OWNER=default/permabox K3S_DATA_DIR=/data
export READY_FILE="$tmp/ready" PAYLOAD="$tmp/permabox" RESTART_TIMEOUT=2
mkdir -p "$HOST_ROOT/data/agent/etc/containerd" "$HOST_ROOT/data/data/current/bin" "$HOST_ROOT/run"
# The fixture expands these variables when invoked as the payload.
# shellcheck disable=SC2016
printf '%s\n' '#!/bin/sh' \
  'if [ "${1:-}" = serve-workloads ]; then' \
  '  echo "$$" >"${READY_FILE}.bridge-pid"' \
  '  touch "$2.new" && mv -f "$2.new" "$2" || exit 1; exec sleep 15' \
  'fi' \
  'if [ "${1:-}" = check-workload-bridge ]; then' \
  '  [ -f "$2" ] && [ ! -f "${READY_FILE}.deny" ] && [ "${reject_bridge:-0}" = 0 ] || exit 1' \
  '  printf "%s\n" bridge >>"${READY_FILE}.events"; exit 0' \
  'fi' \
  '[ "$#" -eq 1 ] && [ "$1" = validate-config ] || exit 1' \
  'case "$PERMABOX_CONFIG" in "$HOST_ROOT"/opt/permabox-k3s/.stage.*/config.yaml) ;; *) exit 1 ;; esac' \
  '[ -s "$PERMABOX_CONFIG" ] && [ "${reject_config:-0}" = 0 ]' >"$PAYLOAD"
chmod 0755 "$PAYLOAD"
install -m 0755 "$PAYLOAD" "$HOST_ROOT/data/data/current/bin/runc"
templates="$HOST_ROOT/data/agent/etc/containerd"
printf '# generated\nversion = 3\n' >"$templates/config.toml"
printf '{{ template "base" . }}\n# custom setting\n' >"$templates/config-v3.toml.tmpl"
cp "$templates/config-v3.toml.tmpl" "$tmp/original"

fake_info() {
  if [[ -f $tmp/live ]]; then cat "$tmp/live"; else printf '%s\n' '{"config":{"containerd":{"defaultRuntimeName":"runc","runtimes":{"runc":{"options":{"SystemdCgroup":true}}}}}}'; fi
}
cri_info() {
  # The mock observes the CRI coordinates in the caller's subshell.
  # shellcheck disable=SC2031
  printf '%s %s %s\n' "$endpoint" "$k3s_bin" "$data_dir" >>"$tmp/cri-queries"
  fake_info
}
host_exec() {
  if [[ $1 == /usr/bin/test && -n ${service_exe:-} ]]; then
    [[ $2 == "$service_exe" && $3 == -ef && $4 == /proc/123/exe ]]
    return
  fi
  if [[ $* == '/opt/permabox-k3s/runtime gc' ]]; then
    test -f "$READY_FILE" || exit 1
    printf '%s\n' gc >>"$tmp/sweeps"
    # A failed sweep must not stop the next one.
    test "$(wc -l <"$tmp/sweeps")" -gt 1
  fi
}
service_cmd() { printf '%s\n' 123; }
export K3S_SERVICE=k3s.service

(
  unit=k3s.service k3s_bin=/usr/local/bin/k3s data_dir=/data
  service_exe="$k3s_bin" select_unit
  service_exe="$data_dir/data/current/bin/k3s" select_unit
  if service_exe=/usr/local/bin/unrelated select_unit 2>/dev/null; then
    printf '%s\n' 'accepted a service whose process was not K3s' >&2
    exit 1
  fi
)

restart_service() {
  printf '%s\n' restart >>"$READY_FILE.events"
  printf '%s\n' restarted >>"$tmp/restarts"
  if [[ ${fail_restart:-0} == 1 ]]; then return 1; fi
  # The fake restart runs inside reconcile's subshell and sees its template path.
  # shellcheck disable=SC2031
  if [[ -f $template ]] && grep -q 'BEGIN PermaBox' "$template"; then
    local annotations
    annotations=$(sed -n 's/^  pod_annotations = //p' "$template")
    jq -n --argjson annotations "$annotations" '{config:{containerd:{defaultRuntimeName:"runc",runtimes:{runc:{options:{SystemdCgroup:true}},permabox:{runtimeType:"io.containerd.runc.v2",options:{BinaryName:"/opt/permabox-k3s/runtime",SystemdCgroup:true},PodAnnotations:$annotations}}}}}' >"$tmp/live"
  else
    rm -f "$tmp/live"
  fi
  if [[ ${drop_bridge:-0} == 1 ]]; then touch "$READY_FILE.deny"; fi
  if [[ ${interrupt_restart:-0} == 1 ]]; then kill -KILL "$BASHPID"; fi
}

export MODE=install
if [[ ${1:-all} == all || $1 == bridge ]]; then
  if reject_bridge=1 BRIDGE_TIMEOUT=1 reconcile 2>/dev/null; then
    printf '%s\n' 'activated the runtime without an authenticated bridge' >&2; exit 1
  fi
  test ! -e "$HOST_ROOT/opt/permabox-k3s/current"
  test ! -e "$tmp/restarts"
fi
if reject_config=1 reconcile 2>/dev/null; then
  printf '%s\n' 'published a config rejected by the payload' >&2; exit 1
fi
test ! -e "$HOST_ROOT/opt/permabox-k3s/config.yaml"
test ! -e "$HOST_ROOT/opt/permabox-k3s/runtime"
test ! -e "$HOST_ROOT/opt/permabox-k3s/permabox"
test ! -e "$HOST_ROOT/opt/permabox-k3s/owner"
test ! -e "$READY_FILE"
test ! -e "$tmp/restarts"
cmp "$tmp/original" "$templates/config-v3.toml.tmpl"
chmod 0755 "$HOST_ROOT/opt/permabox-k3s"
reconcile
if [[ ${1:-all} == all || $1 == bridge ]]; then
  test "$(sed -n '1,3p' "$READY_FILE.events")" = $'bridge\nrestart\nbridge'
fi
mode_bits=$(stat -c %a "$HOST_ROOT/opt/permabox-k3s")
if [[ $mode_bits != 700 ]]; then
  printf 'expected private workload socket directory, got %s\n' "$mode_bits" >&2
  exit 1
fi
grep -Fq '# custom setting' "$templates/config-v3.toml.tmpl"
grep -Fq 'plugins."io.containerd.cri.v1.runtime"' "$templates/config-v3.toml.tmpl"
test -x "$HOST_ROOT/opt/permabox-k3s/runtime"
test -f "$READY_FILE"
jq -e '.storage_root == "/var/lib/permabox/storage" and .underlying_runtime == "/data/data/current/bin/runc" and
       .storage_roots == {} and .underlying_runtimes == {} and .gc_idle_ttl == "0s"' \
  "$HOST_ROOT/opt/permabox-k3s/config.yaml" >/dev/null
jq -e '.config.containerd.runtimes.permabox.PodAnnotations | sort ==
       ["permabox.io/container", "permabox.io/persist", "permabox.io/storage-root", "permabox.io/underlying-runtime"]' "$tmp/live" >/dev/null
for annotation_key in permabox.io/storage-root permabox.io/underlying-runtime; do
  live_missing=$(jq --arg key "$annotation_key" '.config.containerd.runtimes.permabox.PodAnnotations -= [$key]' "$tmp/live")
  if annotation=permabox.io/persist systemd=true desired_handler "$live_missing"; then
    printf 'accepted a live handler missing %s\n' "$annotation_key" >&2; exit 1
  fi
done

if [[ ${1:-all} == all || $1 == legacy ]]; then
  legacy="$HOST_ROOT/srv/legacy/default/demo-0"
  mkdir -p "$legacy/upper"
  touch "$legacy/upper/keep"
  if STORAGE_ROOTS='{"legacy":"/srv/legacy"}' reconcile 2>"$tmp/legacy-error"; then
    printf '%s\n' 'activated a generation with an unverified legacy slot' >&2; exit 1
  fi
  grep -Fq '/srv/legacy/default/demo-0' "$tmp/legacy-error"
  test -f "$legacy/upper/keep"
  test ! -e "$legacy/.permabox-owner.json"
  printf '%s\n' '{"namespace":"default","kind":"StatefulSet","name":"demo","uid":"owner-123"}' >"$legacy/.permabox-workload.json"
  STORAGE_ROOTS='{"legacy":"/srv/legacy"}' reconcile
  test ! -e "$legacy/.permabox-owner.json"
fi

if [[ ${1:-all} == all || $1 == trust ]]; then
  TRUSTED_NAMESPACES='["tenant-a","tenant-b"]' reconcile
  jq -e '.trusted_namespaces == ["tenant-a", "tenant-b"]' "$HOST_ROOT/opt/permabox-k3s/config.yaml" >/dev/null || {
    printf '%s\n' 'trusted namespaces missing from staged config' >&2; exit 1
  }
  if TRUSTED_NAMESPACES='"tenant-a"' reconcile 2>/dev/null; then exit 1; fi
fi

STORAGE_ROOTS='{"fast":"/srv/data # \"fast\""}' UNDERLYING_RUNTIMES='{"gpu":"/opt/runtime bin/runc"}' \
GC_IDLE_TTL=48h GC_ON_WORKLOAD_DELETE=true GC_ON_SCALE_TO_ZERO=true GC_WORKLOAD_ANNOTATIONS=true reconcile
jq -e '.storage_roots == {fast:"/srv/data # \"fast\""} and
       .underlying_runtimes == {gpu:"/opt/runtime bin/runc"} and .gc_idle_ttl == "48h" and
       .gc_on_workload_delete == true and .gc_on_scale_to_zero == true and .gc_workload_annotations == true' \
  "$HOST_ROOT/opt/permabox-k3s/config.yaml" >/dev/null
if UNDERLYING_RUNTIME=/opt/permabox-k3s/runtime reconcile 2>/dev/null; then
  printf '%s\n' 'accepted the installer wrapper as the default runtime' >&2; exit 1
fi
if UNDERLYING_RUNTIMES='{"self":"/opt/permabox-k3s/runtime"}' reconcile 2>/dev/null; then
  printf '%s\n' 'accepted the installer wrapper as a runtime alias' >&2; exit 1
fi
reconcile
jq -e 'has("trusted_namespaces") | not' "$HOST_ROOT/opt/permabox-k3s/config.yaml" >/dev/null
test "$(wc -l <"$tmp/restarts")" -eq 1

cp "$HOST_ROOT/opt/permabox-k3s/config.yaml" "$tmp/original-config"
cp "$templates/config-v3.toml.tmpl" "$tmp/installed-template"
if [[ ${1:-all} == all || $1 == bridge ]]; then
  if PERSIST_ANNOTATION=example.test/persist drop_bridge=1 reconcile 2>"$tmp/bridge-error"; then
    printf '%s\n' 'committed with an unhealthy replacement bridge' >&2; exit 1
  fi
  grep -Fq 'unavailable before commit' "$tmp/bridge-error"
  rm "$READY_FILE.deny"
  test ! -e "$READY_FILE"
  test ! -e "$HOST_ROOT/opt/permabox-k3s/.transaction"
  cmp "$tmp/original-config" "$HOST_ROOT/opt/permabox-k3s/config.yaml"
  cmp "$tmp/installed-template" "$templates/config-v3.toml.tmpl"
  jq -e '.config.containerd.runtimes.permabox.PodAnnotations | index("permabox.io/persist") != null' "$tmp/live" >/dev/null
fi
restarts_before_recovery=$(wc -l <"$tmp/restarts")
if (
  mv() {
    if [[ ${*: -1} == "$templates/config-v3.toml.tmpl" && $* == *'.permabox-candidate.'* ]]; then return 1; fi
    command mv "$@"
  }
  PERSIST_ANNOTATION=example.test/persist reconcile
) 2>/dev/null; then
  printf '%s\n' 'accepted a failed template publication' >&2; exit 1
fi
cmp "$tmp/original-config" "$HOST_ROOT/opt/permabox-k3s/config.yaml"
cmp "$tmp/installed-template" "$templates/config-v3.toml.tmpl"

if (
  mv() {
    if [[ ${*: -1} == "$templates/config-v3.toml.tmpl" && $* == *'.permabox-candidate.'* ]]; then kill -KILL "$BASHPID"; fi
    command mv "$@"
  }
  PERSIST_ANNOTATION=example.test/persist reconcile
) 2>/dev/null; then
  printf '%s\n' 'accepted an interrupted upgrade' >&2; exit 1
fi
# Recovery must precede staging: rejecting the next config should leave the old one intact.
if [[ ${1:-all} == all || $1 == journal ]]; then
  if K3S_DATA_DIR=/other K3S_BIN=/other/k3s K3S_CRI_ENDPOINT=unix:///other.sock reconcile 2>/dev/null; then exit 1; fi
  if [[ -e $HOST_ROOT/opt/permabox-k3s/.transaction ]]; then
    printf '%s\n' 'incoming settings blocked recovery of the old transaction' >&2; exit 1
  fi
else
  if reject_config=1 reconcile 2>/dev/null; then exit 1; fi
fi
cmp "$tmp/original-config" "$HOST_ROOT/opt/permabox-k3s/config.yaml"
cmp "$tmp/installed-template" "$templates/config-v3.toml.tmpl"

if [[ ${1:-all} == all || $1 == journal ]]; then
  if (
    # In the recovered process this value is unset; only the forward restart is interrupted.
    K3S_CRI_ENDPOINT=unix:///original.sock K3S_BIN=/original/k3s \
      PERSIST_ANNOTATION=example.test/persist interrupt_restart=1 reconcile
  ) 2>/dev/null; then exit 1; fi
  test -f "$HOST_ROOT/opt/permabox-k3s/.transaction/restart"
  if K3S_DATA_DIR=/other K3S_BIN=/other/k3s K3S_CRI_ENDPOINT=unix:///other.sock reconcile 2>/dev/null; then exit 1; fi
  test ! -e "$HOST_ROOT/opt/permabox-k3s/.transaction"
  test "$(tail -n 2 "$tmp/cri-queries" | sort -u)" = 'unix:///original.sock /original/k3s /data'
  cmp "$tmp/original-config" "$HOST_ROOT/opt/permabox-k3s/config.yaml"
  cmp "$tmp/installed-template" "$templates/config-v3.toml.tmpl"
  test "$(wc -l <"$tmp/restarts")" -eq "$((restarts_before_recovery + 2))"
fi

restarts_before=$(wc -l <"$tmp/restarts")
if PERSIST_ANNOTATION=example.test/persist fail_restart=1 reconcile 2>/dev/null; then
  printf '%s\n' 'accepted a failed runtime upgrade' >&2; exit 1
fi
cmp "$tmp/original-config" "$HOST_ROOT/opt/permabox-k3s/config.yaml"
cmp "$tmp/installed-template" "$templates/config-v3.toml.tmpl"
test ! -e "$HOST_ROOT/opt/permabox-k3s/.transaction"
test "$(wc -l <"$tmp/restarts")" -eq "$((restarts_before + 1))"

if (export OWNER=other/release; reconcile) 2>/dev/null; then
  printf '%s\n' 'accepted another release as the owner' >&2; exit 1
fi

mkdir -p "$HOST_ROOT/var/lib/permabox/storage/default/demo-0/upper"
touch "$HOST_ROOT/var/lib/permabox/storage/default/demo-0/upper/keep"
mkdir "$HOST_ROOT/run/permabox"
touch "$HOST_ROOT/run/permabox/live.runtime"
if [[ ${1:-all} == all || $1 == trust ]]; then
  if TRUSTED_NAMESPACES='["tenant-a"]' reconcile 2>/dev/null; then
    printf '%s\n' 'changed trusted namespaces with an active runtime record' >&2; exit 1
  fi
fi
if GC_IDLE_TTL=48h reconcile 2>/dev/null; then
  printf '%s\n' 'changed config with an active runtime override record' >&2; exit 1
fi
cmp "$tmp/original-config" "$HOST_ROOT/opt/permabox-k3s/config.yaml"
rm "$HOST_ROOT/run/permabox/live.runtime"
export MODE=remove
touch "$HOST_ROOT/run/permabox/live.json"
if reconcile 2>/dev/null; then
  printf '%s\n' 'removed a runtime with an active container record' >&2; exit 1
fi
rm "$HOST_ROOT/run/permabox/live.json"
touch "$HOST_ROOT/run/permabox/live.runtime"
if reconcile 2>/dev/null; then
  printf '%s\n' 'removed a runtime with an active runtime override record' >&2; exit 1
fi
rm "$HOST_ROOT/run/permabox/live.runtime"
reconcile
cmp "$tmp/original" "$templates/config-v3.toml.tmpl"
test -f "$HOST_ROOT/var/lib/permabox/storage/default/demo-0/upper/keep"
test ! -e "$HOST_ROOT/opt/permabox-k3s/runtime"
test "$(wc -l <"$tmp/restarts")" -eq "$((restarts_before + 2))"
reconcile
test "$(wc -l <"$tmp/restarts")" -eq "$((restarts_before + 2))"

rm -f "$templates/config-v3.toml.tmpl"
printf 'version = 2\n' >"$templates/config.toml"
export MODE=install
rm -f "$HOST_ROOT/var/lib/permabox/storage/default/demo-0/upper/keep"
rmdir "$HOST_ROOT/var/lib/permabox/storage/default/demo-0/upper"
reconcile
grep -Fq 'plugins."io.containerd.grpc.v1.cri"' "$templates/config.toml.tmpl"
(
  # The reconciliation worker calls this override through sleep_interval.
  # shellcheck disable=SC2317
  sleep() {
    if [[ $1 == 0.2 ]]; then command sleep "$@"; return; fi
    test "$1" = 7 || exit 1
    [[ ! -f $tmp/sweeps ]] || test "$(wc -l <"$tmp/sweeps")" -lt 2
  }
  GC_INTERVAL_SECONDS=7 main
)
test "$(wc -l <"$tmp/sweeps")" -eq 2
if [[ ${1:-all} == all || $1 == signal ]]; then
  rm -f "$READY_FILE"
  main &
  main_pid=$!
  for ((i=0; i<100; i++)); do
    if [[ -f $READY_FILE ]]; then break; fi
    sleep 0.05
  done
  test -f "$READY_FILE"
  bridge_process=$(<"$READY_FILE.bridge-pid")
  kill -TERM "$main_pid"
  wait "$main_pid"
  if kill -0 "$bridge_process" 2>/dev/null; then
    kill "$bridge_process" 2>/dev/null || true
    printf '%s\n' 'installer termination left its bridge running' >&2; exit 1
  fi
  test ! -e "$READY_FILE"
fi
export MODE=remove
(
  # shellcheck disable=SC2317
  sleep() { test "$1" = infinity; }
  main
)
test "$(wc -l <"$tmp/sweeps")" -eq 2
test ! -e "$templates/config.toml.tmpl"

printf '# custom v2 template\n' >"$templates/config.toml.tmpl"
cp "$templates/config.toml.tmpl" "$tmp/original-v2"
export MODE=install fail_restart=1
if (
  # shellcheck disable=SC2317
  sleep() {
    if [[ $1 == 0.2 ]]; then command sleep "$@"; return; fi
    touch "$tmp/slept-after-failure"; return 1
  }
  main
) 2>/dev/null; then
  printf '%s\n' 'accepted a failed service restart' >&2; exit 1
fi
cmp "$tmp/original-v2" "$templates/config.toml.tmpl"
test ! -e "$READY_FILE"
test ! -e "$tmp/slept-after-failure"

printf '%s\n' 'Installer lifecycle tests passed'
