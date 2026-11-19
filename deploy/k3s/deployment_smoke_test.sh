#!/usr/bin/env bash
# Run before Helm removal on the single-node K3s smoke host. The permabox
# release must enable deploymentLocalReuse and both workload GC policies,
# allow workload annotation overrides, and leave the idle TTL disabled.
set -euo pipefail
export LC_ALL=C

storage=${1:?pass storage root}
marker=permabox-deployment-smoke-data

k() { k3s kubectl --request-timeout=10s "$@"; }
app() { k -n "$namespace" "$@"; }

fail() { printf '%s\n' "$*" >&2; exit 1; }

wait_for() {
  local deadline=$((SECONDS + 180))
  until "$@"; do
    (( SECONDS < deadline )) || fail "Timed out waiting for $*"
    sleep 2
  done
}

create_deployment() {
  local name=$1 scale_gc=${2:-} scale_annotation=
  if [[ -n $scale_gc ]]; then
    printf -v scale_annotation '        permabox.io/gc-on-scale-to-zero: "%s"' "$scale_gc"
  fi
  app create -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
spec:
  replicas: 3
  selector:
    matchLabels:
      app: $name
  template:
    metadata:
      labels:
        app: $name
      annotations:
        permabox.io/persist: "true"
        permabox.io/container: demo
        permabox.io/reuse-policy: deployment-local
$scale_annotation
    spec:
      runtimeClassName: permabox
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 1
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: demo
          image: docker.io/library/alpine:3.22
          command: ["/bin/sleep", "86400"]
          securityContext:
            runAsUser: 0
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF
}

ready_pods() {
  pods=$(app get pods -l "app=$1" -o json) && jq -e --argjson count "$2" '
    .items | length == $count and all(.[];
      .metadata.deletionTimestamp == null and
      any(.status.conditions[]?; .type == "Ready" and .status == "True"))
  ' <<< "$pods" >/dev/null
}

replacement_ready() {
  ready_pods reuse 3 && jq -e --arg uid "$1" 'all(.items[]; .metadata.uid != $uid)' <<< "$pods" >/dev/null
}

# Wait for the node-side container removal as well as the Pod API deletion
# before trying to reuse slots. Check exited containers too.
containers_removed() {
  sudo k3s crictl --runtime-endpoint unix:///run/k3s/containerd/containerd.sock \
    ps -a -o json | jq -e --arg ns "$namespace" '
      all(.containers[]?; .labels["io.kubernetes.pod.namespace"] != $ns)
    ' >/dev/null
}

pod_rows() {
  jq -r '.items | sort_by(.metadata.uid)[] | [.metadata.name, .metadata.uid] | @tsv' <<< "$pods"
}

pod_exec() {
  local name=$1
  shift
  app exec "$name" -c demo -- sh -ec "$@"
}

write_markers() {
  local mode=$1 name uid
  while read -r name uid; do
    # shellcheck disable=SC2016
    pod_exec "$name" '
      if [ "$3" = fresh ]; then test ! -e "$1"; fi
      printf "%s\n" "$2" > "$1"
    ' sh "/$marker" "$uid" "$mode"
  done < <(pod_rows)
}

# Output sorted rows of Pod UID and its marker, using '-' for an empty layer.
read_markers() {
  local name uid value
  while read -r name uid; do
    # shellcheck disable=SC2016
    value=$(pod_exec "$name" 'if test -e "$1"; then cat "$1"; else printf -- "-\n"; fi' sh "/$marker") || return
    printf '%s %s\n' "$uid" "$value"
  done < <(pod_rows)
}

assert_own_markers() {
  awk 'NF != 2 || $1 != $2 { bad = 1 } END { exit (bad || NR != 3) }' <<< "$1" ||
    fail "Each of the three Pods must read back its own marker: $1"
}

host_markers() {
  # Discover files under this test namespace; the slot layout can change.
  sudo bash -ec 'if [[ -d $1 ]]; then find "$1" -xdev -type f -name "$2" -print; fi' \
    bash "$storage/$namespace" "$marker"
}

markers_gone() {
  local paths
  paths=$(host_markers) && [[ -z $paths ]]
}

assert_host_markers() {
  local paths
  paths=$(host_markers)
  [[ -n $paths && $(wc -l <<< "$paths") -eq $1 ]] ||
    fail "Expected $1 marker files in $storage/$namespace; found: $paths"
}

namespace=$(k create -f - -o jsonpath='{.metadata.name}' <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  generateName: permabox-deployment-smoke-
EOF
)
[[ -n $namespace ]] || fail 'No namespace returned by K3s'
echo "Checking Deployment local reuse in $namespace"
create_deployment reuse false
wait_for ready_pods reuse 3
write_markers fresh
initial=$(read_markers)
assert_own_markers "$initial"
assert_host_markers 3

echo 'Checking controlled replacement of all three replicas'
app scale deployment reuse --replicas=0
wait_for ready_pods reuse 0
wait_for containers_removed
assert_host_markers 3
app scale deployment reuse --replicas=3
wait_for ready_pods reuse 3
reused=$(read_markers)
original_uids=$(cut -d ' ' -f 1 <<< "$initial")
[[ -z $(comm -12 <(printf '%s\n' "$original_uids") <(cut -d ' ' -f 1 <<< "$reused")) ]] ||
  fail "Expected three new Pod UIDs: $reused"
[[ $(cut -d ' ' -f 2 <<< "$reused" | sort) == "$original_uids" ]] ||
  fail "Replacement Pods did not read the original three distinct markers: $reused"
write_markers overwrite
at_three=$(read_markers)
assert_own_markers "$at_three"

echo 'Checking nonzero scaling and explicit Pod deletion'
app scale deployment reuse --replicas=4
wait_for ready_pods reuse 4
at_four=$(read_markers)
[[ $(comm -12 <(printf '%s\n' "$at_three") <(printf '%s\n' "$at_four")) == "$at_three" ]] ||
  fail "Scale-up changed an existing replica: $at_four"
[[ $(comm -13 <(printf '%s\n' "$at_three") <(printf '%s\n' "$at_four") | cut -d ' ' -f 2) == - ]] ||
  fail "The fourth replica did not start with a fresh layer: $at_four"
app scale deployment reuse --replicas=3
wait_for ready_pods reuse 3
before_delete=$(read_markers)
[[ -z $(comm -23 <(printf '%s\n' "$before_delete") <(printf '%s\n' "$at_four")) ]] ||
  fail "Scale-down changed a surviving replica: $before_delete"
assert_host_markers 3

read -r victim victim_uid <<< "$(pod_rows)"
survivors=$(awk -v uid="$victim_uid" '$1 != uid' <<< "$before_delete")
app delete pod "$victim" --wait=false
wait_for replacement_ready "$victim_uid"
after_delete=$(read_markers)
[[ $(comm -12 <(printf '%s\n' "$before_delete") <(printf '%s\n' "$after_delete")) == "$survivors" ]] ||
  fail "Pod deletion changed another replica: $after_delete"
replacement=$(comm -13 <(printf '%s\n' "$survivors") <(printf '%s\n' "$after_delete"))
read -r replacement_uid replacement_value <<< "$replacement"
[[ -n $replacement_uid && $replacement_uid != "$victim_uid" ]] ||
  fail "Expected one replacement for Pod $victim: $after_delete"
# The ReplicaSet can create the Pod before the old layer is released. Either
# a fresh layer or a released layer is valid, but neither survivor may share it.
if [[ $replacement_value != - ]] && grep -Fxq -- "$replacement_value" < <(cut -d ' ' -f 2 <<< "$survivors"); then
  fail "The replacement shares a survivor's marker: $after_delete"
fi

echo 'Checking workload deletion GC'
assert_host_markers 3
app delete deployment reuse --cascade=foreground --wait=false
wait_for ready_pods reuse 0
wait_for markers_gone

echo 'Checking scale-to-zero GC with the default policy'
create_deployment zero-gc
wait_for ready_pods zero-gc 3
write_markers fresh
fresh=$(read_markers)
assert_own_markers "$fresh"
assert_host_markers 3
app scale deployment zero-gc --replicas=0
wait_for ready_pods zero-gc 0
wait_for markers_gone
app scale deployment zero-gc --replicas=3
wait_for ready_pods zero-gc 3
fresh=$(read_markers)
awk 'NF != 2 || $2 != "-" { bad = 1 } END { exit (bad || NR != 3) }' <<< "$fresh" ||
  fail "Scaling back up recovered previously reclaimed markers: $fresh"
app delete deployment zero-gc --cascade=foreground --wait=false
wait_for ready_pods zero-gc 0
wait_for containers_removed
k delete namespace "$namespace" --wait=false
echo 'Deployment reuse and workload GC checks passed'
