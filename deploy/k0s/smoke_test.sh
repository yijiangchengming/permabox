#!/usr/bin/env bash
# Run from the repository root on a disposable native systemd VM.
set -euo pipefail

k0s=$(realpath "${1:?pass the checksum-verified k0s binary}")
ns=permabox-demo
pod=permabox-demo-0
unit=k0scontroller.service
drop_in=/etc/k0s/containerd.d/permabox.toml
config=/etc/k0s/containerd.toml
containerd=/var/lib/k0s/bin/containerd
runtime=/var/lib/k0s/bin/runc
[[ -x $k0s && -x bin/permabox ]]
[[ $(ps -p 1 -o comm=) == systemd ]] || { printf 'Requires a native systemd VM\n' >&2; exit 1; }

node_exec() { sudo -n "$@"; }
for path in /etc/k0s /var/lib/k0s /run/k0s /etc/permabox /var/lib/permabox /run/permabox /opt/permabox /usr/local/bin/permabox; do
  if node_exec test -e "$path" || node_exec test -L "$path"; then
    printf 'Refusing to modify a VM with existing path %s\n' "$path" >&2
    exit 1
  fi
done
for service in "$unit" k0sworker.service; do
  if systemctl cat "$service" >/dev/null 2>&1; then
    printf 'Refusing to replace existing unit %s\n' "$service" >&2
    exit 1
  fi
done

work=$(mktemp -d)
installed=false
k() { node_exec "$k0s" kubectl --kubeconfig "$work/kubeconfig" --request-timeout=10s "$@"; }
app() { k -n "$ns" "$@"; }
# shellcheck source=deploy/kubernetes/smoke_lifecycle.sh
source deploy/kubernetes/smoke_lifecycle.sh

cleanup() {
  local status=$?
  if (( status != 0 )); then
    systemctl status "$unit" --no-pager || true
    node_exec journalctl -u "$unit" --no-pager -n 200 || true
    node_exec tail -n 100 /var/log/permabox.log || true
    if [[ -s $work/kubeconfig ]]; then
      k get nodes -o wide || true
      k get pods -A -o wide || true
      app describe pod "$pod" || true
      k get events -A --sort-by=.lastTimestamp || true
    fi
  fi
  if $installed; then
    node_exec timeout 90s systemctl stop "$unit" || status=1
    node_exec timeout 90s "$k0s" reset || status=1
  fi
  node_exec rm -f -- "$drop_in" /usr/local/bin/permabox /etc/permabox/config.yaml || status=1
  rm -rf -- "$work" || status=1
  exit "$status"
}
trap cleanup EXIT

node_exec "$k0s" version
node_exec install -d -m 0755 /etc/k0s/containerd.d /etc/permabox
node_exec install -d -m 0700 /opt/permabox
node_exec install -m 0755 bin/permabox /usr/local/bin/permabox
node_exec install -m 0644 deploy/containerd-2.x-v3.toml "$drop_in"
node_exec tee /etc/permabox/config.yaml >/dev/null <<EOF
underlying_runtime: $runtime
gc_on_workload_delete: true
gc_on_scale_to_zero: true
gc_workload_annotations: true
workload_socket: /opt/permabox/workload.sock
EOF
node_exec /usr/local/bin/permabox validate-config
node_exec "$k0s" install controller --single
installed=true
node_exec systemctl start "$unit"

kubeconfig_ready() {
  (umask 077; node_exec "$k0s" kubeconfig admin > "$work/kubeconfig") 2>/dev/null &&
    [[ -s $work/kubeconfig ]] && k get --raw /readyz >/dev/null 2>&1
}
node_registered() {
  node=$(k get nodes -o jsonpath='{.items[*].metadata.name}') && [[ -n $node && $node != *' '* ]]
}
wait_for kubeconfig_ready
wait_for node_registered
k wait --for=condition=Ready "node/$node" --timeout=180s

node_exec "$containerd" --version
node_exec "$runtime" --version
node_exec grep -Fx '# k0s_managed=true' "$config"
node_exec grep -Fx 'version = 3' "$drop_in"
node_exec "$containerd" --config "$config" config dump | python3 -c '
import sys, tomllib
cfg = tomllib.loads(sys.stdin.read())
r = cfg["plugins"]["io.containerd.cri.v1.runtime"]["containerd"]["runtimes"]["permabox"]
assert r["runtime_type"] == "io.containerd.runc.v2"
assert r["options"]["BinaryName"] == "/usr/local/bin/permabox"
assert r["options"]["SystemdCgroup"] is True
assert {"permabox.io/persist", "permabox.io/container"} <= set(r["pod_annotations"])
'
smoke_lifecycle
