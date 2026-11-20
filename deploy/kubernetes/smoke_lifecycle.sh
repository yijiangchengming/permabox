#!/usr/bin/env bash
# Shared by disposable smoke clusters. The caller supplies k, app, node_exec,
# and the selected node, namespace and Pod names.
# shellcheck disable=SC2154

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

smoke_lifecycle() {
  local slot=/var/lib/permabox/storage/$ns/$pod original
  k label node "$node" permabox.io.node-restriction.kubernetes.io/pinned=true
  if [[ -n ${runtime_class_node_label:-} ]]; then
    k label node "$node" "$runtime_class_node_label=true"
  fi
  k apply -f "${runtime_class_manifest:-deploy/runtimeclass.yaml}"
  k create namespace "$ns"
  k apply -f deploy/kubernetes/workload-bridge.yaml
  k -n permabox-system rollout status daemonset/permabox-workload-bridge --timeout=120s

  app apply -f deploy/statefulset.yaml
  app patch statefulset permabox-demo --type=merge -p '{"spec":{"template":{"spec":{"terminationGracePeriodSeconds":1}}}}'
  app rollout status statefulset/permabox-demo --timeout=120s
  wait_for new_pod ''
  ready
  original=$(uid)
  app exec "$pod" -c demo -- sh -ec 'test -f /etc/inittab; rm /etc/inittab; echo permabox >> /etc/alpine-release; echo persisted > /permabox-marker'
  node_exec test -f "$slot/upper/permabox-marker"
  app delete pod "$pod" --wait=false
  wait_for new_pod "$original"
  ready
  # shellcheck disable=SC2016
  app exec "$pod" -c demo -- sh -ec 'test ! -e /etc/inittab; grep -Fxq permabox /etc/alpine-release; test "$(cat /permabox-marker)" = persisted'

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
  app exec "$pod" -c demo -- sh -ec 'test -f /etc/inittab; ! grep -Fxq permabox /etc/alpine-release; test ! -e /permabox-marker'
  node_exec test -d "$slot/upper"
  app delete statefulset permabox-demo --cascade=foreground --wait=false
  wait_for stopped
  wait_for gc_removed
}
