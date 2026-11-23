# PermaBox on CRI-O

[简体中文](README-cn.md)

The compatibility workflow pairs upstream Kubernetes and CRI-O on Ubuntu 24.04 (amd64) with the conmon and runc binaries bundled by CRI-O. [CI workflow](../../.github/workflows/compat-crio.yml)

| Kubernetes | CRI-O | Storage driver | OCI runtime |
| --- | --- | --- | --- |
| 1.34.0 | 1.34.0 | overlay | runc |
| 1.35.8 | 1.35.8 | overlay | runc |
| 1.36.4 | 1.36.4 | overlay | runc |
| 1.37.0 | 1.37.0 | overlay | runc |

Each pair passed CI coverage for rootfs persistence, replica isolation, reclamation, and a writable node `hostPath` mounted directly and through a directory `subPath`. The tests check both volume paths after Pod replacement and verify that node files survive PermaBox rootfs reclamation. Each job also runs CRI `StopContainer` and `RemoveContainer` separately and checks which mounts survive each call. PVC and CSI backends, read-only mounts, mount propagation, `subPathExpr`, checkpoint and restore, alternate OCI runtimes, user namespaces, enforcing SELinux, and OpenShift are outside this matrix. The workflow also verifies that writes under excluded directories disappear after Pod replacement and that image files return. [Smoke test](smoke_test.sh)

With CRI-O's native `overlay` driver, PermaBox mounts the container rootfs at `<state_dir>/<container-id>.rootfs` and updates `root.path` in the runtime bundle. CRI-O can unmount and replace its own `merged` directory during `StopContainer`. PermaBox keeps the lower bind and the separate rootfs until OCI `delete`. The persistent write layer uses `<storage_root>/<namespace>/<pod-name>/upper`.

After an interrupted create, let CRI-O retry OCI deletion in the original mount namespace. GC retains the journal when the recorded configuration or surviving mounts prevent it from confirming safe cleanup. Use the runtime and bundle paths recorded for that container to investigate. [Recovery instructions](../README.md)

## Install on the target node

1. Install the built binary at `/usr/local/bin/permabox` and install [the PermaBox config](../config.yaml) at `/etc/permabox/config.yaml`, owned by root. Ensure `underlying_runtime` points to the node's actual `runc` binary. Keep `storage_root` on compatible local storage, outside CRI-O's graph and run roots. Leave GC disabled for the first test. See the [shared deployment guide](../README.md) for storage and security requirements.
2. On a node using the native overlay driver, install the [overlay runtime drop-in](99-permabox-overlay.conf) under `/etc/crio/crio.conf.d/`. It only registers the OCI handler. Use the native overlay driver for CRI-O storage. Point `monitor_path` at conmon if needed. In a maintenance window, check the merged CRI-O config, restart CRI-O, and verify the CRI endpoint used by kubelet with `crictl info`.
3. Label exactly one eligible node and apply [runtimeclass.yaml](runtimeclass.yaml). CRI-O workloads select `runtimeClassName: permabox-crio`. The OCI handler remains `permabox`; the distinct RuntimeClass name lets CRI-O and containerd use different node selectors in the same cluster.

Before deploying a persistent workload, create `/opt/permabox` with owner `root` and mode `0700` on the node. Label it `permabox.io.node-restriction.kubernetes.io/crio-gc=true`, apply [the metadata bridge](../kubernetes/workload-bridge.yaml), and wait for its DaemonSet to be ready. The socket path in the example config is `/opt/permabox/workload.sock`. This bridge is required even with GC disabled.

To run the example, set `trusted_namespaces: [permabox-demo]` in the node config before creating the StatefulSet.

```sh
(
  set -eu
  node=replace-with-crio-node-name
  label=permabox.io.node-restriction.kubernetes.io/crio-overlay
  kubectl label node "$node" "$label=true" --overwrite
  test "$(kubectl get nodes -l "$label=true" -o name)" = "node/$node"
  kubectl apply -f deploy/crio/runtimeclass.yaml
  kubectl create namespace permabox-demo
  kubectl -n permabox-demo apply -k examples/crio
  kubectl -n permabox-demo rollout status statefulset/permabox-demo --timeout=180s
)
```

Use the [shared rootfs verification steps](../README.md#4-verify-that-file-changes-survive-pod-recreation). They check that a replacement Pod on the same node and image retains a newly created file, an edited image file, and deletion of an image file. If the Pod fails, inspect its events, the CRI-O service log, and `/var/log/permabox.log` on the selected node. Use only trusted workloads and a clean namespace/Pod identity for the first test.

When upgrading from the old CRI-O RuntimeClass named `permabox`, create `permabox-crio` first and update each StatefulSet's Pod template to use it. Keep the old class available until the affected Pods have rolled out, then remove or repurpose it only after checking for other users. Keep the same node and StatefulSet UID to reuse each replica's layer. Follow the [shared upgrade guidance](../README.md) for layers without verified ownership.

To migrate from VFS, stop the workloads and let the old PermaBox version clean up the mounts, then back up application data outside the writable layer. Prepare a separate CRI-O overlay store and apply the new `crio-overlay` node label before updating the RuntimeClass. Restore application data into a fresh writable layer. Older PermaBox versions can still run VFS workloads.

## Enable StatefulSet reclamation

With the metadata bridge running, enable `gc_on_workload_delete`, `gc_on_scale_to_zero` or `gc_workload_annotations` as needed. Configure the periodic `permabox gc` task described in the [shared deployment guide](../README.md) to reclaim released layers and retry temporary failures.

Each StatefulSet ordinal has an independent write layer. Scaling from two replicas to one retains the removed ordinal's data so it can be reused if that ordinal returns to the same node. With `gc_on_scale_to_zero` enabled, scaling the whole StatefulSet to zero reclaims all released layers for that StatefulSet.

## Upstream behavior checked

- [CRI-O sandbox annotations](https://github.com/cri-o/cri-o/blob/v1.34.0/server/sandbox_run_linux.go) and [workload annotation generation](https://github.com/cri-o/cri-o/blob/v1.34.0/internal/factory/container/container.go): CRI-O marks sandbox and workload containers using `io.kubernetes.cri-o.ContainerType`, and copies sandbox annotations and the [kubelet's workload labels](https://github.com/kubernetes/kubernetes/blob/v1.34.0/pkg/kubelet/kuberuntime/labels.go) into the workload OCI spec. `permabox.io/*` is outside CRI-O's recognized experimental annotation set; adding those keys to `allowed_annotations` is [rejected](https://github.com/cri-o/cri-o/blob/v1.34.0/pkg/config/config.go).
- [CRI-O OCI runtime](https://github.com/cri-o/cri-o/blob/v1.34.0/internal/oci/runtime_oci.go) and [conmon argv assembly](https://github.com/containers/conmon/blob/v2.1.13/src/runtime_args.c): conmon invokes `create` with `--bundle` and `--pid-file`; CRI-O invokes `start` and `delete --force` directly with the runtime root. PermaBox's existing runc CLI parser already handles those forms.
- [Stop order](https://github.com/cri-o/cri-o/blob/v1.34.0/server/container_stop.go), [remove order](https://github.com/cri-o/cri-o/blob/v1.34.0/server/container_remove.go), and the [overlay driver](https://github.com/containers/storage/blob/v1.58.0/drivers/overlay/overlay.go) define the cleanup sequence.
