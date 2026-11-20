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

If the release image is unavailable or you need a custom build, build and push it to a registry your cluster can pull from. Use a build that supports Pod aliases, `gc`, and `validate-config`. Run this from the project root; no build or file copy is needed on the K3s nodes.

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
| `underlyingRuntimes` | `{}`. Maps runtime aliases to absolute binary paths on the host. |
| `gcIdleTTL` | `"0s"`. A Go duration; by default, GC retains released data and still retries stale cleanup. |
| `gcOnWorkloadDelete` | `false`. Enable to reclaim safely released writable layers when their StatefulSet is deleted. |
| `gcOnScaleToZero` | `false`. Enable to reclaim safely released writable layers when their StatefulSet has `spec.replicas: 0`. |
| `gcWorkloadAnnotations` | `false`. Enable to let Pod template annotations override the two workload GC defaults individually. |
| `gcIntervalSeconds` | `60`. A positive integer giving the wait in seconds before each GC sweep. |
| `persistAnnotation` | `permabox.io/persist`. Update the workload Pod annotations if you change this key. |

Use `permabox.io/storage-root` to set a storage alias for the container selected for persistence. Use `permabox.io/underlying-runtime` to set a runtime alias for the workload containers. The sandbox uses the default runtime from the time it was created. See [storage and runtime aliases](../README.md#1-install-on-the-target-node). When upgrading, use a new release image tag or push your custom build with a new tag. While container records still exist, the installer permits changes to the workload GC switches alone. Stop PermaBox workloads before changing other runtime settings.

The installer runs `permabox validate-config` on the staged configuration before publishing it. The default runtime and any runtime alias must not point to `/opt/permabox-k3s/runtime`, because that would call PermaBox recursively.

The installer requires a healthy metadata bridge and checks existing writable layers for verifiable StatefulSet identity before publishing the upgrade. Resolve any legacy layer errors before retrying. A failed CRI handler transition restores the previous transaction state; allow time for K3s to restart and the previous handler to become ready.

## Workload garbage collection

Workload GC policies apply only to StatefulSets. Enable either policy independently, for example with `--set gcOnWorkloadDelete=true --set gcOnScaleToZero=true`. Released layers are checked by the next periodic sweep (60 seconds by default, configurable with `gcIntervalSeconds`). Both policies can reclaim data without waiting for `gcIdleTTL`. Helm also runs a read-only metadata bridge for StatefulSet UID verification, even when GC policies are disabled. Review the [legacy layer upgrade guidance](../README.md#3-pin-the-example-to-one-node-and-deploy-it) before upgrading.

Layers left by Deployments from earlier versions can still be reclaimed through `gcIdleTTL` once released.

Deleting a Pod by hand, rolling out an update, or scaling to a nonzero replica count does not by itself trigger either policy. Deletion is eligible on the next sweep; scaling to zero requires a second sweep at least one minute after approval. The policies apply only to released writable layers with confirmed ownership. See [periodic garbage collection](../README.md#periodic-garbage-collection) for the safeguards.

### Override GC per workload

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

Remove mode skips GC. It removes the runtime registration and the binary and configuration managed by the installer, while preserving storage roots and persistent data. If a node is offline, wait for it to return and verify cleanup there.

For related deployment patterns, see [Sysbox Kubernetes DaemonSet](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md) and [Kata Deploy](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy). For template behavior, see the [K3s containerd configuration documentation](https://docs.k3s.io/advanced#configuring-containerd).
