# PermaBox on CRI-O

This compatibility slice supports **CRI-O 1.34.0 with the VFS storage driver, conmon and runc**. Use a dedicated Linux node or disposable CRI-O instance. VFS is a CRI-O-wide setting, needs a separate graph and run root, and uses more disk space because it copies layers. Do not switch an existing production node to VFS in place. Checkpoint/restore, alternate OCI runtimes, user namespaces and enforcing SELinux were not validated by this slice.

CRI-O's default `overlay` driver is unsupported. `StopContainer` asks `containers/storage` to unmount the rootfs before CRI-O invokes OCI `delete`. The overlay driver's lazy unmount detaches PermaBox's top mount and leaves CRI-O's original mount in place, so replacing the mountpoint fails with `EBUSY`. VFS returns a directory and its stop-time `Put` is a no-op. OCI `delete` can therefore clean up PermaBox's overlay before storage removes that directory. PermaBox rejects selected CRI-O workloads whose OCI rootfs does not use the VFS layout.

## Install on the target node

1. Install the built binary at `/usr/local/bin/permabox` and install [the PermaBox config](../config.yaml) at `/etc/permabox/config.yaml`, owned by root. Ensure `underlying_runtime` points to the node's actual `runc` binary. Keep `storage_root` on compatible local storage, outside CRI-O's graph and run roots. Leave GC disabled for the first test. See the [shared deployment guide](../README.md) for storage and security requirements.
2. Install [99-permabox-vfs.conf](99-permabox-vfs.conf) under `/etc/crio/crio.conf.d/`. Check for CLI flags or later drop-ins overriding the VFS settings, and remove any inherited overlay-specific `storage_option` settings from this dedicated instance. Point `monitor_path` at conmon if it is not on the service's `PATH`. Leave the default runtime and cgroup manager appropriate for the node. Run `sudo crio config` to inspect the merged configuration, restart CRI-O, then run `sudo crictl --runtime-endpoint unix:///run/crio/crio.sock info` (adjust for the socket kubelet actually uses).
3. Label exactly one eligible node and apply [runtimeclass.yaml](runtimeclass.yaml). This RuntimeClass uses the same name as the shared demo; use it in a cluster or test setup where that name is not already assigned to the containerd example.

```sh
(
  set -eu
  node=replace-with-crio-node-name
  label=permabox.io.node-restriction.kubernetes.io/crio-vfs
  kubectl label node "$node" "$label=true" --overwrite
  test "$(kubectl get nodes -l "$label=true" -o name)" = "node/$node"
  kubectl apply -f deploy/crio/runtimeclass.yaml
  kubectl create namespace permabox-demo
  kubectl -n permabox-demo apply -f deploy/statefulset.yaml
  kubectl -n permabox-demo rollout status statefulset/permabox-demo --timeout=180s
)
```

Use the [shared rootfs verification steps](../README.md#4-verify-that-file-changes-survive-pod-recreation). They check that a replacement Pod on the same node and image retains a newly created file, an edited image file, and deletion of an image file. If the Pod fails, inspect its events, the CRI-O service log, and `/var/log/permabox.log` on the selected node. Use only trusted workloads and a clean namespace/Pod identity for the first test.

## Upstream behavior checked

- [CRI-O sandbox annotations](https://github.com/cri-o/cri-o/blob/v1.34.0/server/sandbox_run_linux.go) and [workload annotation generation](https://github.com/cri-o/cri-o/blob/v1.34.0/internal/factory/container/container.go): CRI-O marks sandbox and workload containers using `io.kubernetes.cri-o.ContainerType`, and copies sandbox annotations and the [kubelet's workload labels](https://github.com/kubernetes/kubernetes/blob/v1.34.0/pkg/kubelet/kuberuntime/labels.go) into the workload OCI spec. `permabox.io/*` is outside CRI-O's recognized experimental annotation set; adding those keys to `allowed_annotations` is [rejected](https://github.com/cri-o/cri-o/blob/v1.34.0/pkg/config/config.go).
- [CRI-O OCI runtime](https://github.com/cri-o/cri-o/blob/v1.34.0/internal/oci/runtime_oci.go) and [conmon argv assembly](https://github.com/containers/conmon/blob/v2.1.13/src/runtime_args.c): conmon invokes `create` with `--bundle` and `--pid-file`; CRI-O invokes `start` and `delete --force` directly with the runtime root. PermaBox's existing runc CLI parser already handles those forms.
- [Stop order](https://github.com/cri-o/cri-o/blob/v1.34.0/server/container_stop.go), [remove order](https://github.com/cri-o/cri-o/blob/v1.34.0/server/container_remove.go), and the [overlay](https://github.com/containers/storage/blob/v1.58.0/drivers/overlay/overlay.go) and [VFS](https://github.com/containers/storage/blob/v1.58.0/drivers/vfs/driver.go) drivers explain why this slice requires VFS.
