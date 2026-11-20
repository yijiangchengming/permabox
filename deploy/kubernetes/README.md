# Kubernetes compatibility

[简体中文](README-cn.md)

PermaBox uses Kubernetes `RuntimeClass` (`node.k8s.io/v1`) to select a node's CRI handler. Writable-layer persistence also requires the CRI to forward the workload annotations and invoke PermaBox with a runc-compatible OCI bundle. Run workloads on Linux nodes with a local OverlayFS-compatible filesystem and pin any workload that must recover data to its original node.

| Environment | Installation | Verification |
| --- | --- | --- |
| Upstream Kubernetes with containerd | [Manual node installation](../README.md) | The [CI workflow](../../.github/workflows/ci.yml) tests Kubernetes 1.35.8, 1.36.4, and 1.37.0 on kubeadm-based kind nodes. |
| systemd K3s with embedded containerd | [Helm installer](../k3s/README.md) | Single-node K3s `v1.34.5+k3s1` in CI. |
| RKE2 with embedded containerd | Distribution-specific registration below | Single-node `v1.37.2+rke2r1` with runc in [CI](../../.github/workflows/compat-rke2.yml). |
| k0s with embedded containerd | Distribution-specific registration below | [CI](../../.github/workflows/compat-k0s.yml) targets single-node `v1.36.4+k0s.1` with runc. |
| MicroK8s with containerd | Distribution-specific registration below | Manual integration; no end-to-end test. |
| CRI-O with VFS | [Dedicated node setup](../crio/README.md) | [Compatibility workflow](../../.github/workflows/compat-crio.yml): Kubernetes 1.34.0, CRI-O 1.34.0, conmon 2.1.13 and runc 1.3.1. The default overlay driver is unsupported. |
| Managed Kubernetes | Manual registration on Linux node pools that permit custom CRI handlers and node configuration | Validate against the exact node image. Restricted and serverless pools are outside the supported installation model. |

Kubernetes versions outside the CI matrix are unverified. A working Kubernetes/runtime combination must also meet the upstream Kubernetes CRI and version requirements. Containerd configuration v2/v3 refers to the TOML format, not the Kubernetes version or the containerd major version.

## Register the handler

Follow the [manual installation guide](../README.md) to install PermaBox and configure its storage and underlying runtime. Apply only the `permabox` runtime tables from the matching TOML snippet, preserving your existing config, default runtime, snapshotter, and cgroup driver. Verify the live CRI handler before applying `RuntimeClass` and the test StatefulSet.

- **RKE2:** Under `/var/lib/rancher/rke2/agent/etc/containerd/`, extend `config-v3.toml.tmpl` for v3 or the active `config.toml.tmpl` for v2. When starting a new template on a release that supports it, extend the distribution's base template with `{{ template "base" . }}`. Restart the applicable `rke2-server` or `rke2-agent` service in a maintenance window. See the [RKE2 containerd guide](https://docs.rke2.io/advanced#configuring-containerd).
- **MicroK8s:** Merge the tables into `/var/snap/microk8s/current/args/containerd-template.toml`, using the config format of the active snap. Restart MicroK8s containerd using the snap service controls. Set `underlying_runtime` to the executable used on the node; its path may be inside the snap. See the [MicroK8s service configuration guide](https://canonical.com/microk8s/docs/configuring-services).
- **k0s:** On nodes with k0s-managed containerd, install a `/etc/k0s/containerd.d/permabox.toml` drop-in. For k0s 1.36 and later, use the v3 snippet; use the format expected by the installed version for earlier releases. Match `SystemdCgroup` to the node; use `false` for the stock single-node cgroupfs configuration. For externally managed containerd, use the regular manual guide. See the [k0s runtime guide](https://docs.k0sproject.io/stable/runtime/).

For optional StatefulSet deletion and scale-to-zero GC, [workload-bridge.yaml](workload-bridge.yaml) is a standalone example using the node label in the manual guide. It assumes a PermaBox binary at `/usr/local/bin/permabox`, a root-owned private directory at `/opt/permabox` with mode `0700`, and `workload_socket: /opt/permabox/workload.sock` in the node config. The bridge only reads Pods and StatefulSets. Follow the manual guide to configure the node's separate periodic sweep.

`runc` is the default underlying OCI runtime. The upstream CI matrix also exercises `crun` behind containerd. A different OCI runtime may parse a similar CLI but use a different root filesystem or cleanup lifecycle. Kata, gVisor and Windows are outside the verified matrix.
