#!/usr/bin/env bash
# Run from the repository root on a disposable native systemd VM.
set -euo pipefail

rke2=$(realpath "${1:?pass a checksum-verified RKE2 binary}")
ns=permabox-demo
pod=permabox-demo-0
service=permabox-ci-rke2
data=/var/lib/rancher/rke2
agent=$data/agent
runtime=$data/bin/runc
kubeconfig=/etc/rancher/rke2/rke2.yaml
[[ -x $rke2 && -x bin/permabox && $(ps -p 1 -o comm=) == systemd ]]
node_exec() { sudo -n "$@"; }
k() { node_exec "$data/bin/kubectl" --kubeconfig "$kubeconfig" --request-timeout=10s "$@"; }
app() { k -n "$ns" "$@"; }
# shellcheck source=deploy/kubernetes/smoke_lifecycle.sh
source deploy/kubernetes/smoke_lifecycle.sh

for path in /etc/rancher/rke2 "$data" /etc/permabox /var/lib/permabox /run/permabox /opt/permabox /usr/local/bin/permabox; do
  if node_exec test -e "$path" || node_exec test -L "$path"; then
    printf 'Refusing to modify a VM with existing path %s\n' "$path" >&2
    exit 1
  fi
done
if systemctl cat "$service" >/dev/null 2>&1; then
  printf 'Refusing to replace existing unit %s\n' "$service" >&2
  exit 1
fi

cleanup() {
  local status=$?
  if (( status != 0 )); then
    node_exec journalctl -u "$service" --no-pager -n 150 || true
    node_exec tail -n 100 "$agent/containerd/containerd.log" || true
    node_exec tail -n 100 /var/log/permabox.log || true
    k get pods -A -o wide || true
    app describe pod "$pod" || true
  fi
  node_exec systemctl stop "$service" || status=1
  exit "$status"
}
trap cleanup EXIT

node_exec "$rke2" --version
node_exec install -d -o root -g root -m 0755 /opt
node_exec install -d "$agent/etc/containerd" /etc/permabox
node_exec install -d -o root -g root -m 0700 /opt/permabox
node_exec install -m 0755 bin/permabox /usr/local/bin/permabox
{
  printf '%s\n' '{{ template "base" . }}'
  sed '1,/^version = 3$/d' deploy/containerd-2.x-v3.toml
} | node_exec tee "$agent/etc/containerd/config-v3.toml.tmpl" >/dev/null
node_exec tee /etc/permabox/config.yaml >/dev/null <<EOF
underlying_runtime: $runtime
trusted_namespaces: ["$ns"]
gc_on_workload_delete: true
gc_on_scale_to_zero: true
gc_workload_annotations: true
workload_socket: /opt/permabox/workload.sock
EOF
node_exec /usr/local/bin/permabox validate-config
node_exec systemd-run --unit "$service" --property=KillMode=control-group --property=Delegate=yes "$rke2" server

registered() {
  node=$(k get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) && [[ -n $node && $node != *' '* ]]
}
timeout 360 bash -c 'while ! sudo -n test -x /var/lib/rancher/rke2/bin/kubectl; do sleep 2; done'
wait_for registered
k wait --for=condition=Ready "node/$node" --timeout=180s
node_exec "$data/bin/containerd" --version
node_exec "$runtime" --version
smoke_lifecycle
