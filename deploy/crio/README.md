# PermaBox on CRI-O

[简体中文](README-cn.md)

The [CRI-O compatibility workflow](../../.github/workflows/compat-crio.yml) runs upstream Kubernetes on Ubuntu 24.04 (amd64) with VFS and the conmon and runc binaries bundled by CRI-O:

| Kubernetes | CRI-O | Storage driver | OCI runtime |
| --- | --- | --- | --- |
| 1.34.0 | 1.34.0 | VFS | runc |
| 1.35.8 | 1.35.8 | VFS | runc |
| 1.36.4 | 1.36.4 | VFS | runc |
| 1.37.0 | 1.37.0 | VFS | runc |

Each entry checks file creation, modification and deletion across Pod replacement, per-replica isolation, partial scale-down, scale-to-zero reclamation, StatefulSet deletion and annotation overrides. The table lists exact CI pairs; other patches and mixed Kubernetes/CRI-O versions are not covered. Checkpoint/restore, alternate OCI runtimes, user namespaces, enforcing SELinux and OpenShift are outside this test coverage.

Use a dedicated Linux node or disposable CRI-O instance. VFS is a CRI-O-wide setting, needs a separate graph and run root, and uses more disk space because it copies layers. Do not switch an existing production node to VFS in place.

CRI-O's default `overlay` driver is unsupported. `StopContainer` asks `containers/storage` to unmount the rootfs before CRI-O invokes OCI `delete`. The overlay driver's lazy unmount detaches PermaBox's top mount and leaves CRI-O's original mount in place, so replacing the mountpoint fails with `EBUSY`. VFS returns a directory and its stop-time `Put` is a no-op. OCI `delete` can therefore clean up PermaBox's overlay before storage removes that directory. PermaBox rejects selected CRI-O workloads whose OCI rootfs does not use the VFS layout.

## Install on the target node

1. Install the built binary at `/usr/local/bin/permabox` and install [the PermaBox config](../config.yaml) at `/etc/permabox/config.yaml`, owned by root. Ensure `underlying_runtime` points to the node's actual `runc` binary. Keep `storage_root` on compatible local storage, outside CRI-O's graph and run roots. Leave GC disabled for the first test. See the [shared deployment guide](../README.md) for storage and security requirements.
2. Install [99-permabox-vfs.conf](99-permabox-vfs.conf) under `/etc/crio/crio.conf.d/`. Check for CLI flags or later drop-ins overriding the VFS settings, and remove any inherited overlay-specific `storage_option` settings from this dedicated instance. Point `monitor_path` at conmon if it is not on the service's `PATH`. Leave the default runtime and cgroup manager appropriate for the node. Run `sudo crio config` to inspect the merged configuration, restart CRI-O, then run `sudo crictl --runtime-endpoint unix:///run/crio/crio.sock info` (adjust for the socket kubelet actually uses).
3. Label exactly one eligible node and apply [runtimeclass.yaml](runtimeclass.yaml). CRI-O workloads select `runtimeClassName: permabox-crio`. The OCI handler remains `permabox`; the distinct RuntimeClass name lets CRI-O and containerd use different node selectors in the same cluster.

Before deploying a persistent workload, create `/opt/permabox` with owner `root` and mode `0700` on the node. Label it `permabox.io.node-restriction.kubernetes.io/crio-gc=true`, apply [the metadata bridge](../kubernetes/workload-bridge.yaml), and wait for its DaemonSet to be ready. The socket path in the example config is `/opt/permabox/workload.sock`. This bridge is required even with GC disabled.

```sh
(
  set -eu
  node=replace-with-crio-node-name
  label=permabox.io.node-restriction.kubernetes.io/crio-vfs
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

## Enable StatefulSet reclamation

With the metadata bridge running, enable `gc_on_workload_delete`, `gc_on_scale_to_zero` or `gc_workload_annotations` as needed. Configure the periodic `permabox gc` task described in the [shared deployment guide](../README.md) to reclaim released layers and retry temporary failures.

Each StatefulSet ordinal has an independent write layer. Scaling from two replicas to one retains the removed ordinal's data so it can be reused if that ordinal returns to the same node. With `gc_on_scale_to_zero` enabled, scaling the whole StatefulSet to zero reclaims all released layers for that StatefulSet.

## Upstream behavior checked

- [CRI-O sandbox annotations](https://github.com/cri-o/cri-o/blob/v1.34.0/server/sandbox_run_linux.go) and [workload annotation generation](https://github.com/cri-o/cri-o/blob/v1.34.0/internal/factory/container/container.go): CRI-O marks sandbox and workload containers using `io.kubernetes.cri-o.ContainerType`, and copies sandbox annotations and the [kubelet's workload labels](https://github.com/kubernetes/kubernetes/blob/v1.34.0/pkg/kubelet/kuberuntime/labels.go) into the workload OCI spec. `permabox.io/*` is outside CRI-O's recognized experimental annotation set; adding those keys to `allowed_annotations` is [rejected](https://github.com/cri-o/cri-o/blob/v1.34.0/pkg/config/config.go).
- [CRI-O OCI runtime](https://github.com/cri-o/cri-o/blob/v1.34.0/internal/oci/runtime_oci.go) and [conmon argv assembly](https://github.com/containers/conmon/blob/v2.1.13/src/runtime_args.c): conmon invokes `create` with `--bundle` and `--pid-file`; CRI-O invokes `start` and `delete --force` directly with the runtime root. PermaBox's existing runc CLI parser already handles those forms.
- [Stop order](https://github.com/cri-o/cri-o/blob/v1.34.0/server/container_stop.go), [remove order](https://github.com/cri-o/cri-o/blob/v1.34.0/server/container_remove.go), and the [overlay](https://github.com/containers/storage/blob/v1.58.0/drivers/overlay/overlay.go) and [VFS](https://github.com/containers/storage/blob/v1.58.0/drivers/vfs/driver.go) drivers explain why this slice requires VFS.
