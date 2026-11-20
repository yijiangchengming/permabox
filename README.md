# PermaBox

[简体中文](README-cn.md) · [MIT License](LICENSE)

PermaBox preserves changes to a container's root filesystem across Kubernetes Pod recreation on the same Linux node. It wraps a runc-compatible OCI runtime and uses a node-local OverlayFS writable layer. The application container does not need to run in privileged mode.

It is intended for workloads with stable Pod names, such as StatefulSets. Persistence is keyed by storage root, namespace, and Pod name, with one persistent container per Pod. PermaBox does not migrate data between nodes, manage mounted volumes, or provide a backup. Use a PersistentVolume when you need Kubernetes to manage application storage explicitly.

Deployment replicas do not have stable Pod names and are unsupported for persistence. Use a StatefulSet when each replica needs to recover its own writable layer.

## Features

- **Root filesystem persistence:** Preserve additions, changes, and deletions across same-name Pod recreation, including OverlayFS whiteouts.
- **Runtime and storage selection:** Configure node defaults and named aliases, then select an alias per Pod. PermaBox remembers the underlying runtime for later OCI commands.
- **K3s installation:** A Helm-managed node installer registers the runtime with embedded containerd. The installer needs host privileges; application Pods do not.
- **Lifecycle cleanup:** Keep released layers by default, expire them after an optional idle TTL, or reclaim them when a StatefulSet is deleted or scaled to zero. The two workload policies can be overridden with Pod template annotations.

## Deploy

| Environment | Guide |
| --- | --- |
| Systemd-managed K3s with embedded containerd | [Install with Helm](deploy/k3s/README.md) |
| Standalone containerd on Linux | [Manual installation and verification](deploy/README.md) |

See the [Kubernetes compatibility matrix](deploy/kubernetes/README.md) for tested versions and instructions for other distributions.

After installing the `permabox` RuntimeClass, set `spec.runtimeClassName: permabox` and `permabox.io/persist: "true"` on the Pod. In a multi-container Pod, use `permabox.io/container` to select the container whose root filesystem should persist. See the [StatefulSet example](deploy/statefulset.yaml).

Keep the workload on the node holding its data and use a compatible local filesystem. NFS and shared writable layers are unsupported. Test your kernel, containerd snapshotter, and underlying runtime together before production deployment. Review the [security and upgrade guidance](deploy/README.md) before reusing a writable layer with a different image.

## Develop

Build on Linux with Go 1.23 or newer:

```sh
make build
make check
```

The build produces `bin/permabox`. The checks require Helm, Bash, jq and ShellCheck. Run `make test-kernel` on a Linux host with OverlayFS and permission to create a mount namespace to exercise real mounting and cleanup.

| Path | Purpose |
| --- | --- |
| [`cmd/permabox`](cmd/permabox) | OCI wrapper and command dispatch |
| [`internal`](internal) | Configuration, mount lifecycle, and Kubernetes ownership checks |
| [`deploy`](deploy) | Helm chart, K3s installer, and containerd examples |

See [contributing and releasing](CONTRIBUTING.md) for the development workflow and tagged GHCR publishing.
