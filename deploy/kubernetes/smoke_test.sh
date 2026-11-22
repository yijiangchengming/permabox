#!/usr/bin/env bash
# Runs only against the disposable kind cluster created by this script.
set -euo pipefail

image=${1:?pass a digest-pinned kind node image}
config_version=${2:-2}
runtime=${3:-runc}
cluster=permabox-ci
node=${cluster}-control-plane
ns=permabox-demo
pod=permabox-demo-0
[[ $config_version == 2 || $config_version == 3 ]]
[[ $runtime == runc || $runtime == crun ]]
# Avoid deleting or altering somebody else's cluster on a local run.
if kind get clusters | grep -Fxq "$cluster"; then
  printf 'Cluster %s already exists\n' "$cluster" >&2
  exit 1
fi
work=$(mktemp -d)
export KUBECONFIG=$work/kubeconfig
node_exec() { docker exec "$node" "$@"; }
k() { kubectl --request-timeout=10s "$@"; }
app() { k -n "$ns" "$@"; }
# shellcheck source=deploy/kubernetes/smoke_lifecycle.sh
source deploy/kubernetes/smoke_lifecycle.sh

cleanup() {
  local status=$?
  if (( status != 0 )); then
    k get pods -A -o wide || true
    app describe pod "$pod" || true
    node_exec tail -n 100 /var/log/permabox.log || true
    node_exec journalctl -u containerd --no-pager -n 120 || true
  fi
  kind delete cluster --name "$cluster" || status=1
  # The test node can leave root-owned directories in the bind mount.
  if [[ $(id -u) == 0 ]]; then
    rm -rf -- "$work" || status=1
  else
    sudo -n rm -rf -- "$work" || status=1
  fi
  exit "$status"
}

trap cleanup EXIT

sudo install -d -o root -g root -m 0755 "$work/storage"
{
  printf '%s\n' 'kind: Cluster' 'apiVersion: kind.x-k8s.io/v1alpha4' 'nodes:' '  - role: control-plane' '    extraMounts:'
  printf '      - hostPath: %s\n        containerPath: /var/lib/permabox\n' "$work/storage"
  printf '%s\n' 'containerdConfigPatches:' '  - |-'
  sed '1,/^version = 2$/d; /^$/d; s/^/    /' deploy/containerd-1.x-v2.toml
} > "$work/kind.yaml"
kind create cluster --name "$cluster" --image "$image" --config "$work/kind.yaml" --wait 180s
docker cp bin/permabox "$node:/usr/local/bin/permabox"
if [[ $config_version == 3 ]]; then
  node_exec sh -ec 'containerd config migrate > /etc/containerd/config.toml.tmp; mv /etc/containerd/config.toml.tmp /etc/containerd/config.toml'
  node_exec systemctl restart containerd
fi
node_exec containerd --version
if [[ $runtime == crun ]]; then
  node_exec apt-get update -qq
  node_exec apt-get install -y -qq --no-install-recommends crun dbus
  node_exec systemctl start dbus.socket
fi
node_exec install -d -o root -g root -m 0755 /opt
node_exec install -d -o root -g root -m 0700 /opt/permabox
node_exec install -d /etc/permabox
# shellcheck disable=SC2016
runtime_bin=$(node_exec sh -c 'command -v "$1"' sh "$runtime")
node_exec "$runtime_bin" --version
docker exec -i "$node" tee /etc/permabox/config.yaml >/dev/null <<EOF
underlying_runtime: $runtime_bin
trusted_namespaces: ["$ns"]
gc_on_workload_delete: true
gc_on_scale_to_zero: true
gc_workload_annotations: true
workload_socket: /opt/permabox/workload.sock
EOF
node_exec /usr/local/bin/permabox validate-config
smoke_lifecycle
