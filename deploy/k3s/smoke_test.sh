#!/usr/bin/env bash
# Run on a disposable single-node K3s host with the CI installer image imported.
set -euo pipefail

chart=deploy/helm/permabox
installer_ns=permabox-system
demo_ns=permabox-demo
storage=/var/lib/permabox/ci-storage
pod=permabox-demo-0
slot=$storage/$demo_ns/$pod

k() { k3s kubectl "$@"; }
demo() { k -n "$demo_ns" "$@"; }
pod_field() { demo get pod "$pod" -o "jsonpath=$1"; }
cri_info() { sudo k3s crictl --runtime-endpoint unix:///run/k3s/containerd/containerd.sock info; }
wait_pod() { demo wait --for=condition=Ready "pod/$pod" --timeout=120s; }

wait_for() {
  local deadline=$((SECONDS + 180))
  until "$@"; do
    if (( SECONDS >= deadline )); then
      printf 'Timed out waiting for %s\n' "$*" >&2
      return 1
    fi
    sleep 2
  done
}

installer_ready() {
  # A chart change can restart K3s and briefly interrupt the API connection.
  k --request-timeout=10s -n "$installer_ns" get daemonset permabox-installer -o json | jq -e '
    .status.observedGeneration == .metadata.generation and
    .status.desiredNumberScheduled == 1 and
    .status.updatedNumberScheduled == 1 and .status.numberReady == 1' >/dev/null
}

new_pod() {
  local uid
  uid=$(pod_field '{.metadata.uid}' 2>/dev/null) && [[ -n $uid && $uid != "$1" ]]
}

workload_stopped() {
  local pods
  pods=$(demo get pods -l app.kubernetes.io/name=permabox-demo -o name) && [[ -z $pods ]]
}

node=$(k get nodes -o json | jq -er '.items | select(length == 1) | .[0].metadata.labels["kubernetes.io/hostname"]')
echo 'Installing PermaBox and checking the live CRI handler'
helm upgrade --install permabox "$chart" -n "$installer_ns" --create-namespace \
  --set-string "nodeSelector.kubernetes\\.io/hostname=$node" \
  --set-string image.repository=docker.io/library/permabox-installer --set-string image.tag=ci \
  --set-string image.pullPolicy=Never --set-string storageRoot="$storage" \
  --set gcOnScaleToZero=true --set gcOnWorkloadDelete=true --set gcIntervalSeconds=5 \
  --set deploymentLocalReuse=true --set gcWorkloadAnnotations=true \
  --timeout 2m
wait_for installer_ready
k get runtimeclass permabox -o json | jq -e '.handler == "permabox"'
cri_info | jq -e '.config.containerd.runtimes.permabox.options.BinaryName == "/opt/permabox-k3s/runtime"'

echo 'Checking that root filesystem changes survive Pod recreation'
k create namespace "$demo_ns"
demo apply -f deploy/statefulset.yaml
wait_for new_pod ''
wait_pod
uid=$(pod_field '{.metadata.uid}')
node_name=$(pod_field '{.spec.nodeName}')
image=$(pod_field '{.status.containerStatuses[0].imageID}')
test -n "$image"
test -n "$node_name"
demo exec "$pod" -c demo -- sh -ec '
  test ! -e /permabox-created
  test -f /etc/inittab
  test -s /etc/profile
  printf "%s\n" persisted > /permabox-created
  printf "%s\n" "# modified-by-permabox" > /etc/profile
  rm /etc/inittab
'
sudo test -f "$slot/upper/permabox-created"
demo delete pod "$pod" --wait=false
wait_for new_pod "$uid"
wait_pod
test "$(pod_field '{.spec.nodeName}')" = "$node_name"
test "$(pod_field '{.status.containerStatuses[0].imageID}')" = "$image"
# shellcheck disable=SC2016
demo exec "$pod" -c demo -- sh -ec '
  test "$(cat /permabox-created)" = persisted
  test "$(cat /etc/profile)" = "# modified-by-permabox"
  test ! -e /etc/inittab
'

echo 'Checking that scaling to zero reclaims the writable layer'
uid=$(pod_field '{.metadata.uid}')
demo scale statefulset permabox-demo --replicas=0
wait_for workload_stopped
wait_for sudo test ! -e "$slot"
demo scale statefulset permabox-demo --replicas=1
wait_for new_pod "$uid"
wait_pod
demo exec "$pod" -c demo -- sh -ec 'test ! -e /permabox-created; test -f /etc/inittab'

echo 'Checking that deleting the StatefulSet reclaims its new writable layer'
sudo test -d "$slot/upper"
demo delete statefulset permabox-demo --cascade=foreground --wait=false
wait_for workload_stopped
wait_for sudo test ! -e "$slot"

bash deploy/k3s/deployment_smoke_test.sh "$storage"

echo 'Removing the runtime and uninstalling the chart'
helm upgrade permabox "$chart" -n "$installer_ns" --reuse-values --set mode=remove --timeout 2m
wait_for installer_ready
cri_info | jq -e '.config.containerd.runtimes | type == "object" and (has("permabox") | not)'
sudo test ! -e /opt/permabox-k3s/runtime
helm uninstall permabox -n "$installer_ns" --wait --timeout 2m
runtime_class=$(k get runtimeclass permabox --ignore-not-found -o name)
test -z "$runtime_class"
