# Deploy PermaBox on K3s with Helm

English | [简体中文](README-cn.md)

This chart supports Linux K3s nodes managed by systemd and running embedded containerd. It installs a privileged DaemonSet and `RuntimeClass/permabox`. On each selected node, the installer copies the runtime binary, updates the active containerd config template, and restarts `k3s.service` or `k3s-agent.service` if needed. It checks the live CRI configuration before reporting ready. You do not need to log in to the nodes, and workload Pods using the RuntimeClass do not need privileged mode.

The cluster must allow the installer to run a privileged Pod with `hostPID` and a host filesystem mount in a dedicated namespace. By default, select one node by hostname. Setting `allowParallelInstall=true` permits other node selectors, which can cause several nodes to restart K3s during initial installation. Plan a maintenance window and avoid restarting several control plane nodes at once. The chart does not support rootless K3s, K3s running in a container without a corresponding systemd unit, or K3s using external containerd.

## Choose an installer image

The release workflow publishes an installer image from each stable `vMAJOR.MINOR.PATCH` tag to `ghcr.io/yijiangchengming/permabox-installer`. The image is tagged with the version without the `v` prefix and with `sha-<full-commit-sha>`. GitHub package visibility is managed separately; anonymous pulls require a public package.

Once the release image is available, substitute your repository and version below. The image version has no `v` prefix.

```sh
IMAGE_REPO=ghcr.io/yijiangchengming/permabox-installer
IMAGE_TAG='<MAJOR.MINOR.PATCH>'
```

If the release image is unavailable or you need a custom build, build and push the installer image from the same revision as the chart to a registry your cluster can pull from. Run this from the project root. The K3s nodes pull the image directly.

```sh
IMAGE_REPO=registry.example.com/infra/permabox-installer
IMAGE_TAG=0.1.0
docker buildx build --platform linux/amd64,linux/arm64 \
  -f deploy/k3s/Dockerfile -t "$IMAGE_REPO:$IMAGE_TAG" --push .
```

## Install

Run `kubectl get nodes -L kubernetes.io/hostname` to find the hostname label value for the target node. Set `NODE_HOSTNAME` to that value and run the following from the project root:

```sh
NODE_HOSTNAME=your-k3s-node
helm upgrade --install permabox ./deploy/helm/permabox \
  --namespace permabox-system --create-namespace \
  --set-string image.repository="$IMAGE_REPO" \
  --set-string image.tag="$IMAGE_TAG" \
  --set-string 'nodeSelector.kubernetes\.io/hostname'="$NODE_HOSTNAME" \
  --set 'trustedNamespaces={permabox-demo}' \
  --wait --timeout 10m

kubectl -n permabox-system rollout status daemonset/permabox-installer --timeout=5m
kubectl -n permabox-system rollout status daemonset/permabox-bridge --timeout=5m
kubectl -n permabox-system get daemonset permabox-installer
```

Check that `DESIRED` is nonzero and equals `READY`. On workload Pods, set `spec.runtimeClassName: permabox` and add the `permabox.io/persist: "true"` annotation. In a Pod with several containers, use `permabox.io/container: <container-name>` to select one. To try the [example StatefulSet](../statefulset.yaml), create the `permabox-demo` namespace and apply the manifest there. Then follow the [persistence verification steps](../README.md#4-verify-that-file-changes-survive-pod-recreation). Helm has already registered the runtime and created the RuntimeClass.

The chart applies the same `nodeSelector` and `tolerations` to the installer and RuntimeClass. The writable layer stays on the node, so workloads that depend on existing data must stay pinned to their original node.

## Configure the chart

| Helm value | Default or behavior |
| --- | --- |
| `k3s.dataDir` | `/var/lib/rancher/k3s`. Set this if K3s uses a custom `--data-dir`. |
| `k3s.criEndpoint` | `unix:///run/k3s/containerd/containerd.sock` |
| `k3s.service` | `auto` detects the active server or agent service. For a custom unit, specify its name, such as `my-k3s.service`. |
| `k3s.bin` | `/usr/local/bin/k3s` |
| `underlyingRuntime` | Uses the `runc` bundled with K3s at `<dataDir>/data/current/bin/runc`. |
| `storageRoot` | `/var/lib/permabox/storage` |
| `storageRoots` | `{}`. Maps storage aliases to absolute paths on the node. |
| `trustedNamespaces` | `[]`. Explicit list of namespaces allowed to use persistence. Add your workload namespaces, including when upgrading. |
| `bridgeImage` | `{}`. Inherits the installer image. Set `bridgeImage.tag` and, if needed, `bridgeImage.repository` and `bridgeImage.pullPolicy` to pin a known-good bridge separately. |
| `allowLegacyWrapperMigration` | `false`. Set once in install mode after draining all PermaBox workloads from nodes with an ungated wrapper. |
| `underlyingRuntimes` | `{}`. Maps runtime aliases to absolute binary paths on the host. |
| `gcIdleTTL` | `"0s"`. A Go duration; by default, GC retains released data and still retries stale cleanup. |
| `gcOnWorkloadDelete` | `false`. Enable to reclaim safely released writable layers when their StatefulSet is deleted. |
| `gcOnScaleToZero` | `false`. Enable to reclaim released writable layers when their StatefulSet has `spec.replicas: 0`. Leave disabled and avoid enabling the workload override when data must survive scale-to-zero. |
| `gcWorkloadAnnotations` | `false`. Enable to let Pod template annotations override the two workload GC defaults individually. |
| `gcIntervalSeconds` | `60`. A positive integer giving the wait in seconds before each GC sweep. |
| `persistAnnotation` | `permabox.io/persist`. Update the workload Pod annotations if you change this key. |

Use `permabox.io/storage-root` to set a storage alias for the container selected for persistence. Workload containers can select a runtime alias with `permabox.io/underlying-runtime` when their namespace is listed in `trustedNamespaces`. The sandbox uses the default runtime from the time it was created. See [storage and runtime aliases](../README.md#1-install-on-the-target-node). When upgrading, use a new release image tag or push your custom build with a new tag. While container records exist, the installer permits changes to workload GC switches and migration from the old default socket to the dedicated bridge socket. Stop PermaBox workloads before changing other runtime settings.

Before publishing a generation, the installer validates the staged config and takes the create admission lock. It checks the default runtime, each configured alias, and each runtime still pinned under `/run/permabox` using the same path and executable checks as OCI dispatch. K3s's `data/current` link is accepted when its path and target are controlled by root. A path that resolves to the PermaBox wrapper or binary is rejected. Check custom runtime and storage paths on every selected node before upgrading. Storage roots and their namespace and slot directories must be free of symlinks and writable only by root.

The installer checks the K3s executable on the host before every CRI query, including during recovery. It resolves trusted symlinks within the host filesystem. Before reusing a cached generation, it verifies the cache directories, the binary, and the config, and confirms that their contents match the staged digest. A cache validation failure aborts activation.

The installer requires a healthy metadata bridge and checks existing writable layers for verifiable StatefulSet identity before publishing the upgrade. Resolve any legacy layer errors before retrying. A failed CRI handler transition restores the previous transaction state; allow time for K3s to restart and the previous handler to become ready.

Keep `/opt/permabox-k3s` and its parent directories under root control. Before taking locks or recovering a transaction, the installer verifies the state directory and existing lock files. It sets a trusted legacy directory to mode `0700` and rejects directories writable by other users, unsafe locks, and linked transaction directories.

The bridge DaemonSet mounts only `/opt/permabox-k3s-bridge` on the host. Both the bridge and the installer check root ownership and prepare the directory with mode `0700`, so the two DaemonSets can roll in either order. The installer sets `workload_socket` to `/opt/permabox-k3s-bridge/workload.sock`. Upgrade the chart and installer image together. The bridge keeps its in-container mount path, so a compatible dedicated `bridgeImage` can stay pinned across routine installer image updates.

During the upgrade from `/opt/permabox-k3s/workload.sock`, the bridge can move before the installer switches the active generation. New persistent creates and workload-based GC may pause on an affected node. A failed host transaction restores the previous generation and its socket path. If the old bridge has exited, those operations resume after you resolve the activation error and the installer successfully retries with the isolated bridge. Drain PermaBox workloads before a Helm downgrade across this socket change and use the matching older chart and installer image together.

When upgrading from an ungated wrapper, stop all PermaBox Pods and prevent their controllers from creating replacements. Finish a verified install-mode upgrade with `--set allowLegacyWrapperMigration=true` before resuming workloads or switching to remove mode. Legacy creates that are already in flight fall outside the new gate. On nodes serving PermaBox workloads, use a chart with the dedicated bridge DaemonSet.

During rollback, the installer checks the recorded K3s service and verifies the cached generation named by the saved version link before restoring node files. An unexpected active main process, missing cached generation, or failed cache verification leaves the transaction pending. A stopped K3s service remains eligible for recovery. Reconciliation and rollback access the containerd configuration directory through PID 1's host root. Keep the directory and its regular config and template files free of symlinks and writable only by root.

## Workload garbage collection

Workload GC policies apply only to StatefulSets. Enable either policy independently, for example with `--set gcOnWorkloadDelete=true --set gcOnScaleToZero=true`. Released layers are checked by the next periodic sweep (60 seconds by default, configurable with `gcIntervalSeconds`). Both policies can reclaim data without waiting for `gcIdleTTL`. Helm also runs a read-only metadata bridge for StatefulSet UID verification, even when GC policies are disabled. Review the [legacy layer upgrade guidance](../README.md#3-pin-the-example-to-one-node-and-deploy-it) before upgrading.

Layers left by Deployments from earlier versions can still be reclaimed through `gcIdleTTL` once released.

Deleting a Pod by hand, rolling out an update, or scaling to a nonzero replica count does not by itself trigger either policy. Deletion is eligible on the next sweep; scaling to zero requires a second sweep at least one minute after approval. The policies apply only to released writable layers with confirmed ownership. See [periodic garbage collection](../README.md#periodic-garbage-collection) for the safeguards.

### Override GC per workload

To stop both workload-based reclaim policies for existing layers, set `gcOnWorkloadDelete=false`, `gcOnScaleToZero=false`, and `gcWorkloadAnnotations=false`. Already retired layers can still be deleted. Set `gcIdleTTL=0s` too if released layers must be retained indefinitely.

Set `--set gcWorkloadAnnotations=true` in Helm, then add the desired annotations to the StatefulSet Pod template. You can enable a policy for an individual workload even when both global defaults are `false`:

```yaml
spec:
  template:
    metadata:
      annotations:
        permabox.io/persist: "true"
        permabox.io/gc-on-workload-delete: "true"
        permabox.io/gc-on-scale-to-zero: "false"
```

Both annotations accept the strings `"true"` and `"false"`. Omit an annotation to inherit its Helm default. Invalid values are treated as `"false"`. PermaBox reads these annotations when creating the container and stores them with the writable layer. Template changes apply to newly created containers; released layers that have not been reused keep their recorded settings. The annotations override only their matching workload policies. `gcIdleTTL` still applies independently.

## Troubleshoot installation

The installer detects whether the generated containerd config uses version 2 or 3 and changes only its managed block in `config.toml.tmpl` or `config-v3.toml.tmpl`. Installation fails if another mechanism has already registered a runtime named `permabox`. Check `kubectl -n permabox-system logs daemonset/permabox-installer -c installer` for errors. On a multi-node deployment, check the installer Pod logs for the affected node.

## Uninstall

First stop all workloads that use `RuntimeClass/permabox`. Run removal with **the same release and namespace, targeting every node where it was installed**. Confirm that the removal DaemonSet is ready and that `DESIRED` covers every node that needs cleanup before uninstalling the release:

```sh
helm upgrade permabox ./deploy/helm/permabox \
  --namespace permabox-system --reuse-values --set mode=remove \
  --wait --timeout 10m
kubectl -n permabox-system rollout status daemonset/permabox-installer --timeout=5m
kubectl -n permabox-system get daemonset permabox-installer
helm uninstall permabox --namespace permabox-system
```

Remove mode closes admission to new runtime creates, waits for in-flight creates, and allows deletes to continue. If records remain or host rollback restores the handler, admission stays closed and the bridge stays available. Finish deleting the workloads and retry remove mode, or run a successful install-mode reconciliation to cancel the drain and reopen admission. After verified removal, Helm uninstall also removes the bridge. Remove mode skips GC and preserves storage roots and persistent data. If a node is offline, wait for it to return and verify cleanup there.

For related deployment patterns, see [Sysbox Kubernetes DaemonSet](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md) and [Kata Deploy](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy). For template behavior, see the [K3s containerd configuration documentation](https://docs.k3s.io/advanced#configuring-containerd).
