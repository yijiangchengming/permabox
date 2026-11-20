#!/usr/bin/env bash
# Run from the repository root on a fresh, disposable Ubuntu systemd VM.
set -Eeuo pipefail

assets=$(realpath "${1:?pass the directory containing the verified release assets}")
version=${2:?pass the Kubernetes and CRI-O release version (vX.Y.Z)}
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { printf 'Invalid release version: %s\n' "$version" >&2; exit 1; }
bundle=$assets/cri-o
socket=unix:///run/crio/crio.sock
ns=permabox-demo
pod=permabox-demo-0
runtime_class_manifest=deploy/crio/runtimeclass.yaml
runtime_class_node_label=permabox.io.node-restriction.kubernetes.io/crio-vfs
bridge_node_label=permabox.io.node-restriction.kubernetes.io/crio-gc
example_overlay=examples/crio
kubeconfig=/etc/kubernetes/admin.conf
node_exec() { sudo -n "$@"; }
k() { node_exec "$assets/kubectl" --kubeconfig "$kubeconfig" --request-timeout=10s "$@"; }
app() { k -n "$ns" "$@"; }
# shellcheck source=deploy/kubernetes/smoke_lifecycle.sh
source deploy/kubernetes/smoke_lifecycle.sh

[[ $(ps -p 1 -o comm=) == systemd ]] || { printf 'Requires a disposable systemd VM\n' >&2; exit 1; }
for path in /etc/kubernetes /etc/crio /etc/permabox /opt/permabox /run/permabox /var/lib/permabox /var/lib/kubelet /var/lib/etcd /var/lib/containers/permabox-crio-vfs /usr/local/bin/{permabox,crio,pinns,kubelet} /etc/cni/net.d/10-permabox.conflist; do
  if node_exec test -e "$path" || node_exec test -L "$path"; then
    printf 'Refusing to modify a VM with existing path %s\n' "$path" >&2
    exit 1
  fi
done
for unit in crio.service kubelet.service; do
  if systemctl cat "$unit" >/dev/null 2>&1; then
    printf 'Refusing to replace existing unit %s\n' "$unit" >&2
    exit 1
  fi
done

diagnostics() {
  local status=$?
  if (( status != 0 )); then
    node_exec journalctl -u crio -u kubelet --no-pager -n 250 || true
    node_exec tail -n 120 /var/log/permabox.log || true
    if node_exec test -s "$kubeconfig"; then
      k get pods -A -o wide || true
      app describe pod "$pod" || true
      k get events -A --sort-by=.lastTimestamp || true
    fi
  fi
}
trap diagnostics EXIT
trap 'printf "Smoke test failed at %s:%s: %s\n" "$0" "$LINENO" "$BASH_COMMAND" >&2' ERR

node_exec swapoff -a
node_exec modprobe overlay
node_exec modprobe br_netfilter
node_exec sysctl -w net.ipv4.ip_forward=1 net.bridge.bridge-nf-call-iptables=1
node_exec install -Dm0755 bin/permabox /usr/local/bin/permabox
node_exec install -Dm0755 "$assets/kubelet" /usr/local/bin/kubelet
node_exec install -Dm0755 "$bundle/bin/crio" /usr/local/bin/crio
node_exec install -Dm0755 "$bundle/bin/pinns" /usr/local/bin/pinns
node_exec install -Dm0755 -t /usr/libexec/crio "$bundle/bin/"{conmon,runc}
node_exec install -Dm0755 -t /opt/cni/bin "$bundle/cni-plugins/"{bridge,host-local,loopback,portmap}
node_exec install -Dm0644 "$bundle/contrib/policy.json" /etc/crio/policy.json
node_exec install -Dm0644 "$bundle/contrib/crio.service" /etc/systemd/system/crio.service
node_exec install -Dm0644 deploy/crio/99-permabox-vfs.conf /etc/crio/crio.conf.d/99-permabox-vfs.conf

node_exec tee /etc/crio/crio.conf.d/10-ci.conf >/dev/null <<'EOF'
[crio.image]
signature_policy = "/etc/crio/policy.json"
[crio.runtime]
default_runtime = "runc"
cgroup_manager = "systemd"
pinns_path = "/usr/local/bin/pinns"
[crio.runtime.runtimes.runc]
runtime_path = "/usr/libexec/crio/runc"
monitor_path = "/usr/libexec/crio/conmon"
[crio.runtime.runtimes.permabox]
monitor_path = "/usr/libexec/crio/conmon"
EOF
node_exec install -d -m0755 /etc/cni/net.d /etc/permabox
node_exec install -d -m0700 /opt/permabox
# This bridge serves only the disposable single-node cluster.
node_exec tee /etc/cni/net.d/10-permabox.conflist >/dev/null <<'EOF'
{
  "cniVersion": "1.0.0",
  "name": "permabox-ci",
  "plugins": [
    {"type": "bridge", "bridge": "permabox0", "isGateway": true, "ipMasq": true,
     "ipam": {"type": "host-local", "ranges": [[{"subnet": "10.244.0.0/24"}]],
              "routes": [{"dst": "0.0.0.0/0"}]}},
    {"type": "portmap", "capabilities": {"portMappings": true}}
  ]
}
EOF
node_exec tee /etc/permabox/config.yaml >/dev/null <<'EOF'
underlying_runtime: /usr/libexec/crio/runc
gc_on_workload_delete: true
gc_on_scale_to_zero: true
gc_workload_annotations: true
workload_socket: /opt/permabox/workload.sock
EOF
node_exec /usr/local/bin/permabox validate-config
node_exec tee /etc/systemd/system/kubelet.service >/dev/null <<'EOF'
[Unit]
After=crio.service
Requires=crio.service

[Service]
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
ExecStart=/usr/local/bin/kubelet --bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf --config=/var/lib/kubelet/config.yaml $KUBELET_KUBEADM_ARGS
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
node_exec systemctl daemon-reload
node_exec systemctl start crio
node_exec systemctl enable kubelet

cri() { node_exec "$bundle/bin/crictl" --runtime-endpoint "$socket" --image-endpoint "$socket" "$@"; }
cri version
node_exec /usr/local/bin/crio status info | grep -Fx 'storage driver: vfs'
node_exec /usr/libexec/crio/conmon --version
node_exec /usr/libexec/crio/runc --version
node_exec "$assets/kubeadm" init --kubernetes-version="$version" --cri-socket="$socket" \
  --pod-network-cidr=10.244.0.0/24 --node-name=permabox-ci
node=permabox-ci
k taint node "$node" node-role.kubernetes.io/control-plane-
k wait --for=condition=Ready "node/$node" --timeout=180s
k get node "$node" -o json | jq -e --arg kubelet "$version" --arg runtime "cri-o://${version#v}" '
  .status.nodeInfo | {kubeletVersion, containerRuntimeVersion}
  | if .kubeletVersion == $kubelet and .containerRuntimeVersion == $runtime then .
    else error("unexpected node versions: \(.)") end
'
# Both RuntimeClasses must be able to exist without sharing a node selector.
k apply -f deploy/runtimeclass.yaml
smoke_lifecycle
[[ $(k get runtimeclass permabox -o json | jq -r '.scheduling.nodeSelector["permabox.io.node-restriction.kubernetes.io/pinned"]') == true ]]

replica_slot() { printf '/var/lib/permabox/storage/%s/permabox-demo-%s\n' "$ns" "$1"; }
expect_marker() {
  app exec "permabox-demo-$1" -c demo -- grep -Fx "replica-$1" /etc/permabox-marker
}
app apply -k "$example_overlay"
app patch statefulset permabox-demo --type=merge -p '{"spec":{"replicas":2,"template":{"spec":{"terminationGracePeriodSeconds":1}}}}'
app rollout status statefulset/permabox-demo --timeout=180s
for ordinal in 0 1; do
  # shellcheck disable=SC2016
  app exec "permabox-demo-$ordinal" -c demo -- sh -ec '
    test ! -e /etc/permabox-marker
    printf "%s\n" "$1" >/etc/permabox-marker
  ' sh "replica-$ordinal"
  expect_marker "$ordinal"
done

app scale statefulset permabox-demo --replicas=1
pod=permabox-demo-1
slot=$(replica_slot 1)
wait_for stopped
wait_for node_exec test -f "$slot/.permabox-released.json"
gc
node_exec grep -Fx replica-1 "$slot/upper/etc/permabox-marker"
expect_marker 0

app scale statefulset permabox-demo --replicas=2
wait_for new_pod ''
ready
expect_marker 1
expect_marker 0

app scale statefulset permabox-demo --replicas=0
for ordinal in 1 0; do
  pod=permabox-demo-$ordinal
  slot=$(replica_slot "$ordinal")
  wait_for stopped
  wait_for gc_removed
done
