#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=deploy/k3s/installer.sh
source ./installer.sh

if [[ ${1:-all} == chart ]]; then
  manifest=$("${HELM:-helm}" template permabox ../helm/permabox --show-only templates/bridge-daemonset.yaml \
    --set-string image.repository=example.invalid/installer --set-string 'nodeSelector.kubernetes\.io/hostname=test-node' \
    --set-string bridgeImage.tag=pinned)
  if [[ $(awk '$1 == "path:" { print $2 }' <<<"$manifest") != /opt/permabox-k3s-bridge ]]; then
    printf '%s\n' 'expected the bridge to mount only its dedicated host directory' >&2; exit 1
  fi
  grep -Fq 'mountPath: /host/opt/permabox-k3s' <<<"$manifest"
  grep -Fq 'image: "example.invalid/installer:pinned"' <<<"$manifest"
  exit 0
fi

reconcile() ( reconcile_node once )

tmp=$(mktemp -d)
bridge_supervisor=''
legacy_pid=''
create_pid=''
trap 'for pid in "$create_pid" "$bridge_supervisor" "$legacy_pid"; do if [[ -n $pid ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; done; rm -rf "$tmp"' EXIT
export HOST_ROOT="$tmp/host" OWNER=default/permabox K3S_DATA_DIR=/data
export READY_FILE="$tmp/ready" PAYLOAD="$tmp/permabox" RESTART_TIMEOUT=2
export BRIDGE_READY_FILE="$tmp/bridge-ready"
export PERMABOX_STATE_HELPER="$tmp/state-helper"
"${GO:-go}" build -o "$PERMABOX_STATE_HELPER" ../../cmd/permabox
mkdir -p "$HOST_ROOT/data/agent/etc/containerd" "$HOST_ROOT/data/data/current/bin" "$HOST_ROOT/run"
# The fixture expands these variables when invoked as the payload.
# shellcheck disable=SC2016
printf '%s\n' '#!/bin/sh' \
  'case "${1:-}" in' \
  '  prepare-bridge-state|check-bridge-marker|prepare-installer-state|prepare-runtime-cache|verify-runtime-cache|validate-installed-runtime|validate-containerd-directory) exec "$PERMABOX_STATE_HELPER" "$@";;' \
  '  validate-host-executable|validate-host-runtimes) [ "$2" = /proc/1/root ] && exec "$PERMABOX_STATE_HELPER" "$1" "$HOST_ROOT" "$3"; exit 1;;' \
  'esac' \
  'if [ "${1:-}" = serve-workloads ]; then' \
  '  exec 8>>"$2.lock"; flock -n 8 || exit 1' \
  '  echo "$$" >"${READY_FILE}.bridge-pid"' \
  '  touch "$2.new" && mv -f "$2.new" "$2" || exit 1; exec sleep 120' \
  'fi' \
  'if [ "${1:-}" = check-workload-bridge ]; then' \
  '  [ -f "$2" ] && [ ! -f "${READY_FILE}.deny" ] && [ "${reject_bridge:-0}" = 0 ] || exit 1' \
  '  if [ -n "${expect_bridge_generation:-}" ]; then' \
  '    [ "$(readlink "$HOST_ROOT/opt/permabox-k3s/current")" = "$expect_bridge_generation" ] || exit 1' \
  '  fi' \
  '  printf "%s\n" bridge >>"${READY_FILE}.events"; exit 0' \
  'fi' \
  'case "${1:-}" in' \
  '  --root) shift 2 ;;' \
  'esac' \
  'case "${1:-}" in' \
  '  create)' \
  '    echo "$$" >"${READY_FILE}.create"' \
  '    while [ ! -e "${READY_FILE}.release-create" ]; do sleep 0.05; done' \
  '    jq -n --arg runtime "${FIXTURE_RUNTIME:-/data/data/current/bin/runc}" '\''{Runtime:$runtime,Bundle:"/bundle"}'\'' >"$HOST_ROOT/run/permabox/in-flight.runtime"; exit $? ;;' \
  '  delete) touch "${READY_FILE}.delete"; exit 0 ;;' \
  'esac' \
  '[ "$#" -eq 1 ] && [ "$1" = validate-config ] || exit 1' \
  'case "$PERMABOX_CONFIG" in "$HOST_ROOT"/opt/permabox-k3s/.stage.*/config.yaml) ;; *) exit 1 ;; esac' \
  '[ -s "$PERMABOX_CONFIG" ] && [ "${reject_config:-0}" = 0 ]' >"$PAYLOAD"
chmod 0755 "$PAYLOAD"
install -m 0755 "$PAYLOAD" "$HOST_ROOT/data/data/current/bin/runc"
for fixture in /usr/local/bin/k3s /original/k3s /other/k3s; do
  install -D -m 0755 "$PAYLOAD" "$HOST_ROOT$fixture"
done
templates="$HOST_ROOT/data/agent/etc/containerd"
test "$(containerd_directory /data)" = /proc/1/root/data/agent/etc/containerd
containerd_directory() { printf '%s%s/agent/etc/containerd\n' "$HOST_ROOT" "$1"; }
printf '# generated\nversion = 3\n' >"$templates/config.toml"
printf '{{ template "base" . }}\n# custom setting\n' >"$templates/config-v3.toml.tmpl"
cp "$templates/config-v3.toml.tmpl" "$tmp/original"

fake_info() {
  if [[ -f $tmp/live ]]; then cat "$tmp/live"; else printf '%s\n' '{"config":{"containerd":{"defaultRuntimeName":"runc","runtimes":{"runc":{"options":{"SystemdCgroup":true}}}}}}'; fi
}
cri_info() {
  # The mock observes the CRI coordinates in the caller's subshell.
  # shellcheck disable=SC2031
  printf '%s %s %s\n' "$endpoint" "$1" "$data_dir" >>"$tmp/cri-queries"
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
state_dir="$HOST_ROOT/opt/permabox-k3s"
mkdir -p "$state_dir"
umask 000
printf '%s\n' 'keep existing lock' >"$state_dir/lock"
umask 077
chmod 0777 "$state_dir"
if BRIDGE_TIMEOUT=1 reconcile 2>"$tmp/unsafe-state-error"; then
  printf '%s\n' 'accepted a writable installer state directory' >&2; exit 1
fi
test "$(<"$state_dir/lock")" = 'keep existing lock'
grep -Fq 'unsafe directory' "$tmp/unsafe-state-error"
test ! -e "$tmp/cri-queries"
chmod 0700 "$state_dir"
"$PERMABOX_STATE_HELPER" prepare-installer-state "$state_dir"
test "$(stat -c %a "$state_dir/lock")" = 600
rm "$state_dir/lock"

chmod 0777 "$HOST_ROOT/usr/local/bin/k3s"
if (k3s_bin=/usr/local/bin/k3s; endpoint=unix:///fixture.sock; data_dir=/data; get_info >/dev/null) 2>"$tmp/unsafe-probe-error"; then
  printf '%s\n' 'queried CRI through a writable executable' >&2; exit 1
fi
test ! -e "$tmp/cri-queries"
chmod 0755 "$HOST_ROOT/usr/local/bin/k3s"
install -m 0755 "$PAYLOAD" "$HOST_ROOT/data/data/current/bin/k3s"
ln -s /data/data/current/bin/k3s "$HOST_ROOT/usr/local/bin/linked-k3s"
test "$("$PERMABOX_STATE_HELPER" validate-host-executable "$HOST_ROOT" /usr/local/bin/linked-k3s)" = /data/data/current/bin/k3s
mv "$HOST_ROOT/data/data/current" "$HOST_ROOT/data/data/v1"
ln -s /data/data/v1 "$HOST_ROOT/data/data/current"

mkfifo "$state_dir/permabox"
if reconcile 2>"$tmp/unsafe-installed-error"; then
  printf '%s\n' 'accepted a non-regular installed runtime' >&2; exit 1
fi
grep -Fq 'validate installed permabox' "$tmp/unsafe-installed-error"
test ! -e "$tmp/cri-queries"
rm "$state_dir/permabox" "$state_dir/lock"

(
  install_host="$tmp/cache-test"
  payload=$PAYLOAD
  mkdir -p "$install_host"
  stage="$tmp/cache-stage"
  mkdir "$stage"
  install -m 0755 "$PAYLOAD" "$stage/permabox"
  install -m 0755 "$PAYLOAD" "$stage/runtime"
  printf '{}\n' >"$stage/config.yaml"
  digest=$(sha256sum "$stage/permabox" "$stage/config.yaml" | awk '{ print $1 }' | sha256sum)
  version="$install_host/versions/${digest%% *}"
  mkdir -p "$version"
  install -m 0755 "$PAYLOAD" "$version/permabox"
  printf '{}\n# changed\n' >"$version/config.yaml"
  if publish_runtime "$stage" 2>"$tmp/unsafe-cache-error"; then
    printf '%s\n' 'activated a mismatched cached generation' >&2; exit 1
  fi
  test ! -L "$install_host/current"
  mv "$version" "$tmp/cached-version"
  ln -s "$tmp/cached-version" "$version"
  if publish_runtime "$stage" 2>"$tmp/linked-cache-error"; then
    printf '%s\n' 'activated a linked cached generation' >&2; exit 1
  fi
  test ! -L "$install_host/current"
  rm "$version"
  mv "$tmp/cached-version" "$version"
  cp "$stage/config.yaml" "$version/config.yaml"
  publish_runtime "$stage"
  test "$(readlink "$install_host/current")" = "versions/${digest%% *}"
)
ln -s "$tmp/original" "$state_dir/lock"
if reconcile 2>"$tmp/unsafe-lock-error"; then
  printf '%s\n' 'accepted a symlinked installer lock' >&2; exit 1
fi
grep -Fq 'unsafe installer lock' "$tmp/unsafe-lock-error"
cmp "$tmp/original" "$templates/config-v3.toml.tmpl"
test ! -e "$tmp/cri-queries"
rm "$state_dir/lock"

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
bridge_state="$HOST_ROOT/opt/permabox-k3s-bridge"
test "$(stat -c %a "$bridge_state")" = 700
printf '%s\n' 'other/release' >"$bridge_state/owner"
if (install_dir=/opt/permabox-k3s-bridge; bridge_main) 2>"$tmp/bridge-owner-error"; then
  printf '%s\n' 'bridge accepted another release owner' >&2; exit 1
fi
grep -Fq 'runtime is owned by another Helm release' "$tmp/bridge-owner-error"
test "$(stat -c %a "$state_dir")" = 755
rm "$bridge_state/owner"
socket_path="$HOST_ROOT/opt/permabox-k3s/workload.sock"
"$PAYLOAD" serve-workloads "$socket_path" &
legacy_pid=$!
for ((i=0; i<100; i++)); do
  if [[ -f $socket_path ]]; then break; fi
  sleep 0.05
done
test -f "$socket_path"
legacy_socket=$(socket_identity "$socket_path")
if BRIDGE_TIMEOUT=1 reconcile 2>"$tmp/legacy-bridge-error"; then
  printf '%s\n' 'accepted the old socket during bridge migration' >&2; exit 1
fi
grep -Fq 'authenticated readiness check' "$tmp/legacy-bridge-error"
(install_dir=/opt/permabox-k3s-bridge; bridge_main) &
bridge_supervisor=$!
for ((i=0; i<100; i++)); do
  if [[ -f $BRIDGE_READY_FILE ]]; then break; fi
  sleep 0.05
done
test -f "$BRIDGE_READY_FILE"
(install_dir=/opt/permabox-k3s-bridge; bridge_probe)
test "$(socket_identity "$socket_path")" = "$legacy_socket"
kill "$legacy_pid"
wait "$legacy_pid" || true
legacy_pid=''
marker="$bridge_state/workload.sock.ready"
cp "$marker" "$tmp/bridge-marker"
printf '\n' >>"$marker"
if bridge_probe --node; then
  printf '%s\n' 'accepted a bridge marker with trailing data' >&2; exit 1
fi
mv -T "$tmp/bridge-marker" "$marker"
: >"$READY_FILE.events"
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
       .workload_socket == "/opt/permabox-k3s-bridge/workload.sock" and
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

install -D -m 0755 "$PAYLOAD" "$HOST_ROOT/opt/runtime bin/runc"
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

if [[ ${1:-all} == all || $1 == runtime ]]; then
  runtime_generation=$(readlink "$state_dir/current")
  install -D -m 0775 "$PAYLOAD" "$HOST_ROOT/opt/unsafe-runc"
  if UNDERLYING_RUNTIME=/opt/unsafe-runc reconcile 2>"$tmp/runtime-error"; then
    printf '%s\n' 'activated a writable default runtime' >&2; exit 1
  fi
  grep -Fq '/opt/unsafe-runc' "$tmp/runtime-error"
  if UNDERLYING_RUNTIMES='{"gpu":"/opt/unsafe-runc"}' reconcile 2>"$tmp/runtime-error"; then
    printf '%s\n' 'activated a writable runtime alias' >&2; exit 1
  fi
  grep -Fq 'runtime alias "gpu"' "$tmp/runtime-error"
  ln -s /opt/permabox-k3s/runtime "$HOST_ROOT/opt/linked-wrapper"
  if UNDERLYING_RUNTIME=/opt/linked-wrapper reconcile 2>"$tmp/runtime-error"; then
    printf '%s\n' 'activated a runtime pointing to the wrapper' >&2; exit 1
  fi
  grep -Fq 'points to PermaBox' "$tmp/runtime-error"
  mkdir -m 0700 "$HOST_ROOT/run/permabox"
  sed "s|^base=.*|base=$state_dir|" "$state_dir/runtime" >"$tmp/admission-wrapper"
  FIXTURE_RUNTIME=/opt/unsafe-runc sh "$tmp/admission-wrapper" create test-id &
  create_pid=$!
  for ((i=0; i<100; i++)); do
    if [[ -f $READY_FILE.create ]]; then break; fi
    sleep 0.05
  done
  test -f "$READY_FILE.create"
  (
    # Observe when reconciliation reaches the exclusive create gate.
    # shellcheck disable=SC2317
    flock() {
      if [[ $1 == -x ]]; then touch "$tmp/waiting-for-create"; fi
      command flock "$@"
    }
    reconcile
  ) 2>"$tmp/runtime-error" &
  install_pid=$!
  for ((i=0; i<100; i++)); do
    if [[ -e $tmp/waiting-for-create ]]; then break; fi
    sleep 0.05
  done
  test -e "$tmp/waiting-for-create"
  touch "$READY_FILE.release-create"
  wait "$create_pid"
  create_pid=''
  if wait "$install_pid"; then
    printf '%s\n' 'activated with an invalid runtime pinned by an in-flight create' >&2; exit 1
  fi
  grep -Fq 'pinned runtime for in-flight' "$tmp/runtime-error"
  test "$(readlink "$state_dir/current")" = "$runtime_generation"
  test ! -e "$state_dir/.transaction"
  test ! -e "$READY_FILE"
  test "$(wc -l <"$tmp/restarts")" -eq 1
  chmod 0755 "$HOST_ROOT/opt/unsafe-runc"
  reconcile
  test -f "$READY_FILE"
  rm "$READY_FILE.create" "$READY_FILE.release-create" "$HOST_ROOT/run/permabox/in-flight.runtime"
  rmdir "$HOST_ROOT/run/permabox"
fi

if [[ ${1:-all} == all || $1 == bridge ]]; then
  isolated_generation=$(readlink "$state_dir/current")
  (
    install_host=$state_dir payload=$PAYLOAD
    stage=$(mktemp -d "$state_dir/.stage.XXXXXXXX")
    cp "$state_dir/runtime" "$stage/runtime"
    cp -L "$state_dir/permabox" "$stage/permabox"
    jq 'del(.workload_socket)' "$state_dir/config.yaml" >"$stage/config.yaml"
    publish_runtime "$stage"
    rm -rf "$stage"
  )
  legacy_generation=$(readlink "$state_dir/current")
  test "$legacy_generation" != "$isolated_generation"
  mkdir -m 0700 "$HOST_ROOT/run/permabox"
  printf '%s\n' '{"Runtime":"/data/data/current/bin/runc","Bundle":"/bundle"}' >"$HOST_ROOT/run/permabox/live.runtime"
  if GC_IDLE_TTL=48h reconcile 2>"$tmp/migration-config-error"; then
    printf '%s\n' 'allowed an unrelated configuration change during socket migration' >&2; exit 1
  fi
  grep -Fq 'stop PermaBox workloads before changing runtime settings' "$tmp/migration-config-error"
  if expect_bridge_generation=$legacy_generation reconcile 2>"$tmp/migration-rollback-error"; then
    printf '%s\n' 'committed the socket migration when the new bridge stopped responding' >&2; exit 1
  fi
  grep -Fq 'unavailable before commit' "$tmp/migration-rollback-error"
  test "$(readlink "$state_dir/current")" = "$legacy_generation"
  jq -e 'has("workload_socket") | not' "$state_dir/config.yaml" >/dev/null
  test ! -e "$state_dir/.transaction"
  test ! -e "$READY_FILE"
  reconcile
  test "$(readlink "$state_dir/current")" = "$isolated_generation"
  test -f "$HOST_ROOT/run/permabox/live.runtime"
  test -f "$READY_FILE"
  test "$(wc -l <"$tmp/restarts")" -eq 1
  rm "$HOST_ROOT/run/permabox/live.runtime"
  rmdir "$HOST_ROOT/run/permabox"
fi

cp "$HOST_ROOT/opt/permabox-k3s/config.yaml" "$tmp/original-config"
cp "$templates/config-v3.toml.tmpl" "$tmp/installed-template"
if [[ ${1:-all} == all || $1 == bridge ]]; then
  if PERSIST_ANNOTATION=example.test/persist drop_bridge=1 reconcile 2>"$tmp/bridge-error"; then
    printf '%s\n' 'committed with an unhealthy replacement bridge' >&2; exit 1
  fi
  grep -Fq 'unavailable before commit' "$tmp/bridge-error"
  rm "$READY_FILE.deny"
  bridge_probe --node
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
  cp "$templates/config-v3.toml.tmpl" "$tmp/interrupted-template"
  interrupted_generation=$(readlink "$state_dir/current")
  previous_generation=$(readlink "$state_dir/.transaction/current")
  cp "$state_dir/$previous_generation/config.yaml" "$tmp/previous-generation-config"
  printf '\n# damaged cached configuration\n' >>"$state_dir/$previous_generation/config.yaml"
  if reconcile 2>"$tmp/damaged-rollback"; then
    printf '%s\n' 'recovered a damaged cached generation' >&2; exit 1
  fi
  grep -Fq 'does not match the expected digest' "$tmp/damaged-rollback"
  cmp "$tmp/interrupted-template" "$templates/config-v3.toml.tmpl"
  test "$(readlink "$state_dir/current")" = "$interrupted_generation"
  test -f "$state_dir/.transaction/restart"
  cp "$tmp/previous-generation-config" "$state_dir/$previous_generation/config.yaml"
  if service_exe=/usr/local/bin/unrelated reconcile 2>"$tmp/unverified-rollback"; then
    printf '%s\n' 'recovered a transaction for an unrelated service' >&2; exit 1
  fi
  grep -Fq 'unverified main process' "$tmp/unverified-rollback"
  cmp "$tmp/interrupted-template" "$templates/config-v3.toml.tmpl"
  test "$(readlink "$state_dir/current")" = "$interrupted_generation"
  test -f "$state_dir/.transaction/restart"
  mv "$templates" "$templates-real"
  ln -s "$templates-real" "$templates"
  if reconcile 2>"$tmp/linked-rollback"; then
    printf '%s\n' 'recovered through a linked template directory' >&2; exit 1
  fi
  grep -Fq 'unsafe directory' "$tmp/linked-rollback"
  cmp "$tmp/interrupted-template" "$templates-real/config-v3.toml.tmpl"
  test -f "$state_dir/.transaction/restart"
  rm "$templates"
  mv "$templates-real" "$templates"
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
cp "$HOST_ROOT/opt/permabox-k3s/runtime" "$tmp/gated-runtime"
sed '/^# PermaBox create admission v1$/d' "$tmp/gated-runtime" >"$HOST_ROOT/opt/permabox-k3s/runtime"
if MODE=remove reconcile 2>"$tmp/legacy-remove-error"; then exit 1; fi
grep -Fq 'legacy runtime wrapper' "$tmp/legacy-remove-error"
touch "$HOST_ROOT/run/permabox/live.runtime"
if ALLOW_LEGACY_WRAPPER_MIGRATION=true reconcile 2>"$tmp/legacy-install-error"; then exit 1; fi
grep -Fq 'before migrating the legacy runtime wrapper' "$tmp/legacy-install-error"
rm "$HOST_ROOT/run/permabox/live.runtime"
if reconcile 2>"$tmp/legacy-install-error"; then exit 1; fi
grep -Fq 'allowLegacyWrapperMigration=true' "$tmp/legacy-install-error"
ALLOW_LEGACY_WRAPPER_MIGRATION=true reconcile
cmp "$tmp/gated-runtime" "$HOST_ROOT/opt/permabox-k3s/runtime"
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
bridge_probe --node
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
  if ! kill -0 "$bridge_process" 2>/dev/null; then
    printf '%s\n' 'installer termination stopped the metadata bridge' >&2; exit 1
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

if [[ ${1:-all} == all || $1 == gate ]]; then
  unset fail_restart
  reconcile
  # Run the actual generated wrapper against the mock host directory.
  sed "s|^base=.*|base=$HOST_ROOT/opt/permabox-k3s|" "$HOST_ROOT/opt/permabox-k3s/runtime" >"$tmp/runtime"
  sh "$tmp/runtime" --root /somewhere create test-id &
  create_pid=$!
  for ((i=0; i<100; i++)); do
    if [[ -f $READY_FILE.create ]]; then break; fi
    sleep 0.05
  done
  test -f "$READY_FILE.create"
  test "$(<"$READY_FILE.create")" = "$create_pid"
  (if MODE=remove reconcile; then touch "$tmp/removal-succeeded"; fi) &
  remove_pid=$!
  for ((i=0; i<100; i++)); do
    if [[ -e $HOST_ROOT/opt/permabox-k3s/create-closed ]]; then break; fi
    sleep 0.05
  done
  if [[ -e $tmp/removal-succeeded ]]; then
    touch "$READY_FILE.release-create"
    wait "$create_pid" "$remove_pid"
    printf '%s\n' 'removed the runtime while a create was still in flight' >&2; exit 1
  fi
  sh "$tmp/runtime" --root /somewhere delete test-id
  test -f "$READY_FILE.delete"
  touch "$READY_FILE.release-create"
  wait "$create_pid" "$remove_pid"
  create_pid=''
  test ! -e "$tmp/removal-succeeded"
  test -e "$HOST_ROOT/opt/permabox-k3s/create-closed"
  test -x "$HOST_ROOT/opt/permabox-k3s/runtime"
  if sh "$tmp/runtime" create rejected-during-drain 2>"$tmp/gate-error"; then exit 1; fi
  grep -Fq 'admission is closed' "$tmp/gate-error"
  rm "$HOST_ROOT/run/permabox/in-flight.runtime"
  if MODE=remove fail_restart=1 reconcile 2>/dev/null; then exit 1; fi
  test -e "$HOST_ROOT/opt/permabox-k3s/create-closed"
  test -x "$HOST_ROOT/opt/permabox-k3s/runtime"
  bridge_probe --node
  reconcile
  test ! -e "$HOST_ROOT/opt/permabox-k3s/create-closed"
  MODE=remove reconcile
  if sh "$tmp/runtime" create late-create; then exit 1; fi
  test -e "$HOST_ROOT/opt/permabox-k3s/create.lock"
fi

printf '%s\n' 'Installer lifecycle tests passed'
