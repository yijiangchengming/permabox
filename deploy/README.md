# Deploy PermaBox

English | [简体中文](README-cn.md)

Use the [K3s Helm guide](k3s/README.md) if your cluster uses K3s with embedded containerd. This guide covers manual installation and a StatefulSet test that verifies persistence.

The example runs a single StatefulSet replica on one Linux node. When the Pod is recreated, it keeps the same namespace and name, so PermaBox can mount its existing local writable layer. A headless Service provides a stable network identity; the example container does not listen on a port.

## 1. Install on the target node

Choose a schedulable Linux node that is `Ready`, runs containerd, and supports OverlayFS. Use local storage. The `upper` and `work` directories must be on the same filesystem, which must support the extended attributes and valid `d_type` required by OverlayFS. Local ext4 is a good starting point. NFS and other shared filesystems are unsupported for the writable layer.

On the target node, run the following from the project root. Put a PermaBox binary built for that node's architecture at `./permabox` and confirm the path to the underlying runtime:

```sh
sudo install -o root -g root -m 0755 ./permabox /usr/local/bin/permabox
sudo install -d -o root -g root -m 0755 /etc/permabox
sudo install -o root -g root -m 0644 deploy/config.yaml /etc/permabox/config.yaml
sudo install -d -o root -g root -m 0700 /var/lib/permabox/storage
sudo install -d -o root -g root -m 0700 /opt/permabox
sudo test -x /usr/bin/runc
```

See [config.yaml](config.yaml) for the defaults. If the node's `runc` is elsewhere, set `underlying_runtime` to its absolute path. It must not point back to PermaBox. Test any other underlying runtime on the target node before using it. PermaBox tracks mounts and cleanup in `/run/permabox` and keeps persistent data separately in `<storage_root>/<namespace>/<pod-name>/upper`. The host log defaults to `/var/log/permabox.log`.

Set `trusted_namespaces: [permabox-demo]` in the installed config to run the example. The default empty list denies persistence; regular runtime passthrough still works. Only list namespaces whose Pod creators you trust with every writable layer in that namespace. StatefulSet UID verification does not isolate users who can create Pods in the same namespace. Set the allowlist explicitly when upgrading.

| Environment variable | Purpose |
| --- | --- |
| `PERMABOX_CONFIG` | Config file path; defaults to `/etc/permabox/config.yaml` |
| `PERMABOX_STORAGE_ROOT` | Overrides `storage_root` |
| `PERMABOX_UNDERLYING_RUNTIME` | Overrides `underlying_runtime` |
| `PERMABOX_LOG_FILE` | Overrides `log_file` |
| `PERMABOX_PERSIST_ANNOTATION` | Overrides `persist_annotation` |
| `PERMABOX_GC_IDLE_TTL` | Overrides `gc_idle_ttl` |

The runtime process on the host must inherit these variables. Setting them in a workload container's `env` has no effect. Prefer the node config file; OCI commands do not need extra PermaBox arguments.

Define optional storage and runtime aliases in the node config. The values must be absolute host paths:

```yaml
storage_roots:
  fast: /mnt/local-ssd/permabox
underlying_runtimes:
  gpu: /usr/bin/nvidia-container-runtime
```

On the Pod, or under `spec.template.metadata.annotations` in a StatefulSet, set `permabox.io/storage-root: fast` to use that storage root for the container selected for persistence. Set `permabox.io/underlying-runtime: gpu` to choose a runtime for the Pod's workload containers, even when persistence is disabled. The sandbox always uses the default runtime. When the annotations are absent, PermaBox uses `storage_root` and `underlying_runtime`. Changing the storage alias does not migrate existing data. On each target node, ensure the storage is local and the configured runtime binaries are installed and executable.

## 2. Register the containerd handler

Run `containerd --version` and identify the containerd instance and config file used by kubelet (usually `/etc/containerd/config.toml`). Back up the file, then merge the `permabox` table and its `options` subtable from the matching snippet. Keep the existing CRI, image, snapshotter, and default runtime settings, with a single top-level `version` key.

| containerd / config format | Snippet | CRI runtime plugin |
| --- | --- | --- |
| 1.x / `version = 2` | [containerd-1.x-v2.toml](containerd-1.x-v2.toml) | `io.containerd.grpc.v1.cri` |
| 2.x / `version = 3` | [containerd-2.x-v3.toml](containerd-2.x-v3.toml) | `io.containerd.cri.v1.runtime` |

If containerd 2.x still uses a v2 config, generate a migrated config with `containerd --config /etc/containerd/config.toml config migrate`. Review the main config and its `imports` before merging the v3 snippet; the command does not migrate imported files individually. Do not just change the `version` number. Match `SystemdCgroup` to the node's runtime and kubelet settings; use `true` for the `systemd` driver. Point `BinaryName` at the wrapper and keep the runc v2 shim as `runtime_type`.

`pod_annotations` lists the keys containerd forwards from the Pod to the OCI spec. If you change `persist_annotation`, update both this list and the StatefulSet Pod template. The value that enables persistence is still the string `"true"`. PermaBox mounts persistent layers only for eligible workload containers and skips the CRI sandbox. For a Pod with several containers, select one with `permabox.io/container: demo`, replacing `demo` with the container name. Containers in the same Pod should not share a writable layer keyed by namespace and Pod name.

During a maintenance window, check the config, restart containerd, and confirm CRI is available. This example assumes systemd and the default config path. Point `crictl` at the same CRI endpoint as kubelet.

```sh
sudo containerd --config /etc/containerd/config.toml config dump
sudo systemctl restart containerd
sudo systemctl is-active containerd
sudo crictl info
```

## 3. Pin the example to one node and deploy it

From a terminal with `kubectl` access and the project checkout, set `NODE` to the node where you installed and verified PermaBox. Label only that node. If the label check lists more than one node, confirm that the others have no affected workloads before removing the label from them. The `RuntimeClass` adds both Linux and the node label as scheduling constraints. If the target node is unavailable, wait for it to recover. Moving the label to another node does not move the data.

```sh
(
  set -eu
  NODE='replace-with-node-name'
  LABEL=permabox.io.node-restriction.kubernetes.io/pinned
  kubectl label node "$NODE" "$LABEL=true" --overwrite
  kubectl get nodes -l "$LABEL=true" -o wide
  test "$(kubectl get nodes -l "$LABEL=true" -o name)" = "node/$NODE"

  kubectl apply -f deploy/kubernetes/workload-bridge.yaml
  kubectl -n permabox-system rollout status daemonset/permabox-workload-bridge --timeout=180s
  kubectl create namespace permabox-demo
  kubectl apply -f deploy/runtimeclass.yaml
  kubectl -n permabox-demo apply -f deploy/statefulset.yaml
  kubectl -n permabox-demo rollout status statefulset/permabox-demo --timeout=180s
  test "$(kubectl -n permabox-demo get pod permabox-demo-0 -o jsonpath='{.spec.nodeName}')" = "$NODE"
)
```

PermaBox reuses a layer only for the same StatefulSet UID. A same-name replacement is blocked while its predecessor's layer exists. On upgrade, PermaBox adopts a layer with a matching previous owner marker. For older layers without verified ownership, stop the workload, confirm the layer is unmounted, and back up and archive its slot. Start a clean layer and restore application data through the application. A metadata outage temporarily blocks new persistent container creation and leaves existing layers intact.

## 4. Verify that file changes survive Pod recreation

Start with a clean example data directory and keep the image unchanged throughout the test. The commands create a file, modify the image's `/etc/profile`, and delete its `/etc/inittab`, then delete the Pod normally. Once the StatefulSet recreates the Pod, the script checks that it has a new UID, is on the same node, uses the same image ID, and retained all three changes. All test paths are on the root filesystem, outside mounted volumes.

```sh
(
  set -eu
  ns=permabox-demo
  pod=permabox-demo-0
  k() { kubectl -n "$ns" "$@"; }
  pod_field() { k get pod "$pod" -o "jsonpath=$1"; }

  old_uid=$(pod_field '{.metadata.uid}')
  old_node=$(pod_field '{.spec.nodeName}')
  old_image=$(pod_field '{.status.containerStatuses[0].imageID}')
  test -n "$old_uid"
  test -n "$old_node"
  test -n "$old_image"

  k exec "$pod" -c demo -- /bin/sh -ec '
    test ! -e /etc/permabox-created
    test -f /etc/profile
    test "$(cat /etc/profile)" != "# modified-by-permabox"
    test -f /etc/inittab
    printf "%s\n" created-by-permabox > /etc/permabox-created
    printf "%s\n" "# modified-by-permabox" > /etc/profile
    rm /etc/inittab
  '

  k delete pod "$pod" --wait=true --timeout=180s
  retries=0
  until new_uid=$(pod_field '{.metadata.uid}' 2>/dev/null) &&
        [ -n "$new_uid" ] && [ "$new_uid" != "$old_uid" ]; do
    retries=$((retries + 1))
    if [ "$retries" -ge 90 ]; then printf '%s\n' 'Timed out waiting for the replacement Pod' >&2; exit 1; fi
    sleep 2
  done
  k wait --for=condition=Ready "pod/$pod" --timeout=180s
  test "$(pod_field '{.metadata.uid}')" = "$new_uid"
  test "$(pod_field '{.spec.nodeName}')" = "$old_node"
  test "$(pod_field '{.status.containerStatuses[0].imageID}')" = "$old_image"

  k exec "$pod" -c demo -- /bin/sh -ec '
    test "$(cat /etc/permabox-created)" = created-by-permabox
    test "$(cat /etc/profile)" = "# modified-by-permabox"
    test ! -e /etc/inittab
  '
  printf '%s\n' 'Verified: file creation, modification, and deletion survived Pod recreation'
)
```

If verification fails, first run `kubectl -n permabox-demo describe pod permabox-demo-0` to check scheduling and creation events. On the node, check `journalctl -u containerd`, the PermaBox log, and the storage directory. By default, deleting the example StatefulSet and Service leaves its `upper` directory on the node. For a clean retest, use a different namespace, or stop the workload, confirm the mounts are gone, and back up and archive the old data. Do not force-delete a Pod that may still be running.

<a name="定期回收gc"></a>

## Periodic garbage collection

The default `gc_idle_ttl: "0s"` retains released data. Periodic GC still retries stale cleanup when it can confirm that doing so is safe. With a positive Go duration such as `"72h"`, the idle period starts when the writable layer was last confirmed released. After the period elapses, a subsequent eligible GC sweep can delete the data. Before expiry, a replacement on the same node with the same storage root, namespace, and Pod name can restore the layer. GC keeps legacy data without a lifecycle marker until a later successful use and release. It skips active, potentially mounted, or uncertain data and records. Shared storage is unsupported.

Reclaiming data when a StatefulSet is deleted or scaled to zero is off by default. Deletion can reclaim a released layer on the next eligible sweep. Scale-to-zero requires a later sweep with fresh confirmation, at least one minute after the first approval; reuse cancels the pending reclamation. The final API check and the node's disk operation cannot be atomic. Idle TTL is a separate policy. For manual deployment, the local metadata bridge is required even with both workload policies off. Set `workload_socket` to its absolute host path. Its ServiceAccount needs only `get` access to Pods and StatefulSets, and its socket directory must be private (`0700`). The [K3s Helm installer](k3s/README.md) sets it up automatically.

After a node reboot, GC can release a layer whose latest start was confirmed if no journal or mount references it. Loss of the journal alone does not prove release. GC keeps layers whose latest start was not confirmed and retries interrupted deletions using a receipt outside the deleted directory.

The same API bridge also supports overrides from Pod template annotations. Set `gc_workload_annotations: true` on the node and use the [same annotations as the Helm deployment](k3s/README.md#override-gc-per-workload).

On each manually configured node, use these systemd units to run `gc` periodically. The service must run as root and use the same config, environment overrides, and mount namespace as the runtime. The example assumes containerd uses the default config and does not isolate its mount namespace. If the runtime shim uses a separate mount namespace, run GC in that namespace.

`/etc/systemd/system/permabox-gc.service`:

```ini
[Unit]
Description=PermaBox garbage collection

[Service]
Type=oneshot
Environment=PERMABOX_CONFIG=/etc/permabox/config.yaml
ExecStart=/usr/local/bin/permabox gc
```

`/etc/systemd/system/permabox-gc.timer`:

```ini
[Unit]
Description=Run PermaBox garbage collection every minute

[Timer]
OnActiveSec=60s
OnUnitInactiveSec=60s
AccuracySec=1s

[Install]
WantedBy=timers.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now permabox-gc.timer
systemctl list-timers permabox-gc.timer
sudo journalctl -u permabox-gc.service
```

Before uninstalling PermaBox, run `sudo systemctl disable --now permabox-gc.timer`, then `sudo systemctl stop permabox-gc.service`.

## Recover from a failed container create

After PermaBox hands off `create` to the underlying runtime, it cannot catch a later creation failure in the same process. If containerd never calls `delete` and periodic GC cannot confirm that cleanup is safe, the old record can prevent a replacement from mounting the writable layer. During maintenance, stop retries, verify that the old container has stopped, and check that `/run/permabox/<container-id>.json` belongs to that instance. Retry cleanup with the original runtime config, runtime state root, and mount namespace:

```sh
sudo /usr/local/bin/permabox --root "$RUNTIME_STATE_ROOT" delete --force "$OLD_CONTAINER_ID"
```

Set the variables to the values used by the shim and the failed container. Before resuming the workload, confirm that cleanup removed the record and the temporary lower mount. PermaBox refuses cleanup from a different mount namespace. Inspect the original namespace, or verify during node maintenance that it and its mounts are gone. Do not delete state files by hand to bypass a writable layer that is still in use.

## Security and image upgrades

- A node administrator must install and manage the wrapper because it mounts filesystems on the host. The example container is unprivileged, but runs as UID 0 to write to `/` and `/etc`, so it does not satisfy the Restricted Pod Security policy's non-root requirement. Use an authorized test namespace rather than relaxing admission policy for the whole cluster.
- Allow only trusted workloads to use this RuntimeClass and its persistence annotations. You can enforce this with admission policy. With the Node authorizer and NodeRestriction enabled, the example label key prevents kubelets from assigning themselves that label. PermaBox verifies StatefulSet identity before reusing stored data. Restrict access to node data and back up and archive it as needed. Volumes, Secret projections, `/proc`, and other separate mounts have their own lifecycles.
- In production, pin the image digest for the lower layer. OverlayFS prefers the upper layer: file changes usually trigger copy-up, deletions create whiteouts, and some directories can be marked opaque. Reusing an old upper can override or hide files from a new image, including security fixes. Changing the image does not merge the two layers.
- Never edit a mounted lower or upper layer directly. Before upgrading, stop the instance, confirm that its mounts are gone, record the old image digest, and back up the upper layer. Create a clean upper with a new Pod identity or storage location, migrate only the application data you need, and verify it before switching over. A whole-layer backup must preserve ownership, permissions, extended attributes, and special entries such as whiteouts. For rollback, restore the old image and its matching upper together.
- The root filesystem prepared by the containerd snapshotter may already be OverlayFS. Verify the full procedure on the target node, since nested overlays depend on the kernel, snapshotter, and filesystem. If a mount fails, check kernel logs, overlay depth, and filesystem restrictions before considering a tested non-OverlayFS snapshotter. Node-local storage does not provide failover to another node.

Upstream references: [containerd 1.7 CRI configuration](https://github.com/containerd/containerd/blob/release/1.7/docs/cri/config.md), [containerd 2.0 CRI configuration](https://github.com/containerd/containerd/blob/release/2.0/docs/cri/config.md), [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/), [StatefulSet](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/), [node labels and isolation](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/), and [Linux OverlayFS](https://docs.kernel.org/filesystems/overlayfs.html).
