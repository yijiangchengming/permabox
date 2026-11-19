#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=deploy/k3s/installer.sh
source ./installer.sh

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export HOST_ROOT="$tmp/host" OWNER=default/permabox K3S_DATA_DIR=/data
export READY_FILE="$tmp/ready" PAYLOAD="$tmp/permabox" RESTART_TIMEOUT=2 BRIDGE_LOG="$tmp/bridge"
mkdir -p "$HOST_ROOT/data/agent/etc/containerd" "$HOST_ROOT/data/data/current/bin" "$HOST_ROOT/run"
# The fixture expands these variables when invoked as the payload.
# shellcheck disable=SC2016
printf '%s\n' '#!/bin/sh' \
  'if [ "$#" -eq 2 ] && [ "$1" = serve-workloads ]; then printf "%s\n" "$2" >>"$BRIDGE_LOG"; exit 0; fi' \
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
cri_info() { fake_info; }
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
}

export MODE=install
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
       .storage_roots == {} and .underlying_runtimes == {} and .gc_idle_ttl == "0s" and
       (has("deployment_local_reuse") or has("cri_endpoint") or has("k3s_bin") | not)' \
  "$HOST_ROOT/opt/permabox-k3s/config.yaml" >/dev/null
jq -e '.config.containerd.runtimes.permabox.PodAnnotations | sort ==
       ["permabox.io/container", "permabox.io/persist", "permabox.io/storage-root", "permabox.io/underlying-runtime"]' "$tmp/live" >/dev/null
for annotation_key in permabox.io/storage-root permabox.io/underlying-runtime; do
  live_missing=$(jq --arg key "$annotation_key" '.config.containerd.runtimes.permabox.PodAnnotations -= [$key]' "$tmp/live")
  if annotation=permabox.io/persist systemd=true desired_handler "$live_missing"; then
    printf 'accepted a live handler missing %s\n' "$annotation_key" >&2; exit 1
  fi
done

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
test "$(wc -l <"$tmp/restarts")" -eq 1

cp "$HOST_ROOT/opt/permabox-k3s/config.yaml" "$tmp/original-config"
cp "$templates/config-v3.toml.tmpl" "$tmp/installed-template"
if PERSIST_ANNOTATION=example.test/persist fail_restart=1 reconcile 2>/dev/null; then
  printf '%s\n' 'accepted a failed runtime upgrade' >&2; exit 1
fi
cmp "$tmp/original-config" "$HOST_ROOT/opt/permabox-k3s/config.yaml"
cmp "$tmp/installed-template" "$templates/config-v3.toml.tmpl"

if (export OWNER=other/release; reconcile) 2>/dev/null; then
  printf '%s\n' 'accepted another release as the owner' >&2; exit 1
fi

mkdir -p "$HOST_ROOT/var/lib/permabox/storage/default/demo-0/upper"
touch "$HOST_ROOT/var/lib/permabox/storage/default/demo-0/upper/keep"
mkdir "$HOST_ROOT/run/permabox"
touch "$HOST_ROOT/run/permabox/live.runtime"
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
test "$(wc -l <"$tmp/restarts")" -eq 3
reconcile
test "$(wc -l <"$tmp/restarts")" -eq 3

rm -f "$templates/config-v3.toml.tmpl"
printf 'version = 2\n' >"$templates/config.toml"
export MODE=install
reconcile
grep -Fq 'plugins."io.containerd.grpc.v1.cri"' "$templates/config.toml.tmpl"
(
  sleep() {
    test "$1" = 7 || exit 1
    [[ ! -f $tmp/sweeps ]] || test "$(wc -l <"$tmp/sweeps")" -lt 2
  }
  GC_INTERVAL_SECONDS=7 main
)
test "$(wc -l <"$tmp/sweeps")" -eq 2
export MODE=remove
(
  sleep() { test "$1" = infinity; }
  main
)
test "$(wc -l <"$tmp/sweeps")" -eq 2
test ! -e "$templates/config.toml.tmpl"

printf '# custom v2 template\n' >"$templates/config.toml.tmpl"
cp "$templates/config.toml.tmpl" "$tmp/original-v2"
export MODE=install fail_restart=1
if (
  sleep() { touch "$tmp/slept-after-failure"; return 1; }
  main
) 2>/dev/null; then
  printf '%s\n' 'accepted a failed service restart' >&2; exit 1
fi
cmp "$tmp/original-v2" "$templates/config.toml.tmpl"
test ! -e "$READY_FILE"
test ! -e "$tmp/slept-after-failure"

unset fail_restart
(
  export DEPLOYMENT_LOCAL_REUSE=true K3S_CRI_ENDPOINT=unix:///custom/cri.sock K3S_BIN=/custom/bin/k3s
  export GC_ON_WORKLOAD_DELETE=false GC_ON_SCALE_TO_ZERO=false GC_WORKLOAD_ANNOTATIONS=false
  # Wait for the mocked bridge invocation, then stop before the next sweep.
  sleep() {
    for ((i=0; i<50; i++)); do
      [[ -s $BRIDGE_LOG ]] && break
      command sleep 0.02
    done
    return 1
  }
  main
  test "$(<"$BRIDGE_LOG")" = "$HOST_ROOT/opt/permabox-k3s/workload.sock"
  jq -e '.deployment_local_reuse == true and .cri_endpoint == "unix:///custom/cri.sock" and
         .k3s_bin == "/custom/bin/k3s" and
         (has("gc_on_workload_delete") or has("gc_on_scale_to_zero") or has("gc_workload_annotations") | not)' \
    "$HOST_ROOT/opt/permabox-k3s/config.yaml" >/dev/null
  jq -e '.config.containerd.runtimes.permabox.PodAnnotations | index("permabox.io/reuse-policy") != null' "$tmp/live" >/dev/null
  live_missing=$(jq '.config.containerd.runtimes.permabox.PodAnnotations -= ["permabox.io/reuse-policy"]' "$tmp/live")
  annotation=permabox.io/persist systemd=true DEPLOYMENT_LOCAL_REUSE=false desired_handler "$live_missing"
  if annotation=permabox.io/persist systemd=true desired_handler "$live_missing"; then
    printf '%s\n' 'accepted a live handler missing the reuse annotation with reuse enabled' >&2; exit 1
  fi
)

printf '%s\n' 'Installer lifecycle tests passed'
