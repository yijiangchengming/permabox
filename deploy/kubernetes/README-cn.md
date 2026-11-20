# Kubernetes 兼容性

[English](README.md)

PermaBox 通过 Kubernetes `RuntimeClass`（`node.k8s.io/v1`）选择节点上的 CRI 处理器。根文件系统持久化还要求 CRI 转发工作负载注解，并以兼容 runc 的方式调用 PermaBox。使用支持 OverlayFS 的 Linux 本地文件系统；需要恢复数据的工作负载应固定在原节点。

| 环境 | 安装方式 | 验证范围 |
| --- | --- | --- |
| 原生 Kubernetes + containerd | [手动配置目标节点](../README-cn.md) | [CI](../../.github/workflows/ci.yml) 使用基于 kubeadm 的 kind 节点验证 Kubernetes 1.35.8、1.36.4、1.37.0。 |
| systemd 管理、内置 containerd 的 K3s | [Helm 自动安装](../k3s/README-cn.md) | CI 覆盖单节点 K3s `v1.34.5+k3s1`。 |
| 使用内置 containerd 的 RKE2 | 按下文在节点注册 | [CI](../../.github/workflows/compat-rke2.yml) 已验证单节点 `v1.37.2+rke2r1` + runc。 |
| 使用内置 containerd 的 k0s | 按下文在节点注册 | [CI](../../.github/workflows/compat-k0s.yml) 覆盖单节点 `v1.36.4+k0s.1` + runc。 |
| 使用 containerd 的 MicroK8s | 按下文在节点注册 | 提供手动接入方式，暂无端到端测试。 |
| 使用 VFS 的 CRI-O | [在独立节点手动安装](../crio/README-cn.md) | [兼容性工作流](../../.github/workflows/compat-crio.yml) 使用 Kubernetes 1.34.0、CRI-O 1.34.0、conmon 2.1.13 和 runc 1.3.1。不支持默认 overlay 驱动。 |
| 托管 Kubernetes | 仅在允许修改节点配置和注册自定义 CRI 处理器的 Linux 节点池手动接入 | 需用实际节点镜像验证；受限及无服务器节点池不在当前安装范围内。 |

CI 矩阵之外的 Kubernetes 版本尚未验证。节点还需满足 Kubernetes 上游的 CRI 与版本要求。containerd 配置 v2 和 v3 指的是 TOML 格式，与 Kubernetes 版本或 containerd 大版本不是同一个概念。

## 注册运行时

按[手动安装指南](../README-cn.md)安装 PermaBox，配置写层位置和底层运行时。仅合入相应 TOML 示例中的 `permabox` 运行时配置，保留原来的默认运行时、snapshotter 和 cgroup 驱动。确认 CRI 已加载处理器后，再安装 `RuntimeClass` 并验证 StatefulSet。

- **RKE2：** 在 `/var/lib/rancher/rke2/agent/etc/containerd/` 目录扩展与当前配置格式相符的模板：v3 用 `config-v3.toml.tmpl`，v2 用 `config.toml.tmpl`。如果当前版本支持，从空白创建模板时使用 `{{ template "base" . }}` 继承发行版基础配置。在维护窗口重启对应的 `rke2-server` 或 `rke2-agent` 服务。参见 [RKE2 containerd 文档](https://docs.rke2.io/advanced#configuring-containerd)。
- **MicroK8s：** 将运行时配置合入 `/var/snap/microk8s/current/args/containerd-template.toml`，使用当前 snap 的配置格式，然后通过 snap 服务管理重启 MicroK8s 的 containerd。`underlying_runtime` 使用节点上的实际可执行文件路径，可能位于 snap 内。参见 [MicroK8s 服务配置文档](https://canonical.com/microk8s/docs/configuring-services)。
- **k0s：** 对由 k0s 管理 containerd 的节点，安装 `/etc/k0s/containerd.d/permabox.toml`。k0s 1.36 及以上使用 v3 示例，较早版本按实际配置格式选择。`SystemdCgroup` 须匹配节点；默认单节点配置使用 cgroupfs，应设为 `false`。外部管理的 containerd 使用常规手动安装方式。参见 [k0s 运行时文档](https://docs.k0sproject.io/stable/runtime/)。

要启用 StatefulSet 删除和缩到零回收，可参考独立的 [workload-bridge.yaml](workload-bridge.yaml)。它使用手动安装指南中的节点标签，要求节点上存在 `/usr/local/bin/permabox`、由 root 拥有且权限为 `0700` 的 `/opt/permabox`，并设置 `workload_socket: /opt/permabox/workload.sock`。桥接只读取 Pod 和 StatefulSet，节点上的定期回收仍需按手动指南分别配置。

`runc` 是默认 OCI 运行时；原生 Kubernetes CI 也覆盖 containerd 调用 `crun`。其他运行时即使命令行相似，也可能采用不同的根文件系统和卸载流程。Kata、gVisor 和 Windows 不在已验证矩阵内。
