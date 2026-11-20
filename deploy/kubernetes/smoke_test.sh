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
work=$(mktemp -d)
export KUBECONFIG=$work/kubeconfig
node_exec() { docker exec "$node" "$@"; }
k() { kubectl --request-timeout=10s "$@"; }
app() { k -n "$ns" "$@"; }

cleanup() {
  local status=$?
  if (( status != 0 )); then
    k get pods -A -o wide || true
    app describe pod "$pod" || true
    node_exec tail -n 100 /var/log/permabox.log || true
    node_exec journalctl -u containerd --no-pager -n 120 || true
  fi
  kind delete cluster --name "$cluster" || true
  rm -rf -- "$work"
}

# Avoid deleting or altering somebody else's cluster on a local run.
if kind get clusters | grep -Fxq "$cluster"; then
  printf 'Cluster %s already exists\n' "$cluster" >&2
  exit 1
fi
trap cleanup EXIT

mkdir -p "$work/storage"
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
  node_exec apt-get install -y -qq --no-install-recommends crun
fi
node_exec install -d -m 0700 /opt/permabox
node_exec install -d /etc/permabox
# shellcheck disable=SC2016
runtime_bin=$(node_exec sh -c 'command -v "$1"' sh "$runtime")
node_exec "$runtime_bin" --version
docker exec -i "$node" tee /etc/permabox/config.yaml >/dev/null <<EOF
underlying_runtime: $runtime_bin
gc_on_workload_delete: true
gc_on_scale_to_zero: true
gc_workload_annotations: true
workload_socket: /opt/permabox/workload.sock
EOF
node_exec /usr/local/bin/permabox validate-config
k label node "$node" permabox.io.node-restriction.kubernetes.io/pinned=true
k apply -f deploy/runtimeclass.yaml
k create namespace "$ns"
k apply -f deploy/kubernetes/workload-bridge.yaml
k -n permabox-system rollout status daemonset/permabox-workload-bridge --timeout=120s

wait_for() {
  local deadline=$((SECONDS + 130))
  until "$@"; do
    if (( SECONDS >= deadline )); then
      printf 'Timed out waiting for %s\n' "$*" >&2
      return 1
    fi
    sleep 2
  done
}
ready() { app wait --for=condition=Ready "pod/$pod" --timeout=120s; }
uid() { app get pod "$pod" --ignore-not-found -o jsonpath='{.metadata.uid}'; }
new_pod() { local current; current=$(uid) && [[ -n $current && $current != "$1" ]]; }
stopped() { local current; current=$(uid) && [[ -z $current ]]; }
removed() { node_exec test ! -e "$slot"; }
gc() { node_exec /usr/local/bin/permabox gc; }
gc_removed() { gc && removed; }

slot=/var/lib/permabox/storage/$ns/$pod
app apply -f deploy/statefulset.yaml
wait_for new_pod ''
ready
original=$(uid)
app exec "$pod" -c demo -- sh -ec 'test -f /etc/inittab; rm /etc/inittab; echo persisted > /permabox-marker'
node_exec test -f "$slot/upper/permabox-marker"
app delete pod "$pod" --wait=false
wait_for new_pod "$original"
ready
# shellcheck disable=SC2016
app exec "$pod" -c demo -- sh -ec 'test ! -e /etc/inittab; test "$(cat /permabox-marker)" = persisted'

app patch statefulset permabox-demo --type=merge -p '{"spec":{"template":{"metadata":{"annotations":{"permabox.io/gc-on-scale-to-zero":"false"}}}}}'
app rollout status statefulset/permabox-demo --timeout=120s
app scale statefulset permabox-demo --replicas=0
wait_for stopped
wait_for node_exec test -f "$slot/.permabox-released.json"
gc
node_exec test -f "$slot/upper/permabox-marker"

app patch statefulset permabox-demo --type=merge -p '{"spec":{"template":{"metadata":{"annotations":{"permabox.io/gc-on-scale-to-zero":null}}}}}'
app scale statefulset permabox-demo --replicas=1
wait_for new_pod ''
ready
node_exec test -f "$slot/upper/permabox-marker"
app scale statefulset permabox-demo --replicas=0
wait_for stopped
wait_for gc_removed

app scale statefulset permabox-demo --replicas=1
wait_for new_pod ''
ready
app exec "$pod" -c demo -- sh -ec 'test -f /etc/inittab; test ! -e /permabox-marker'
node_exec test -d "$slot/upper"
app delete statefulset permabox-demo --cascade=foreground --wait=false
wait_for stopped
wait_for gc_removed
