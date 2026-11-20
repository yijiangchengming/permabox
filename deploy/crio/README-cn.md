# 在 CRI-O 上使用 PermaBox

[English](README.md)

支持的组合为 **CRI-O 1.34.0、VFS 存储驱动、conmon 2.1.13 和 runc 1.3.1**。[CRI-O 兼容性工作流](../../.github/workflows/compat-crio.yml) 使用原生 Kubernetes 1.34.0，检查 Pod 重建后文件的创建、修改和删除是否保留，并检查缩容到零、删除 StatefulSet 和注解覆盖的回收行为。请使用独立节点或临时 CRI-O 实例。VFS 是 CRI-O 实例级设置，需要独立的 graph root 和 run root，会复制镜像层并占用更多空间；请勿在已有生产节点上直接切换存储驱动。

CRI-O 默认的 `overlay` 驱动不支持持久化：CRI-O 会在 OCI `delete` 前卸载容器根文件系统，与 PermaBox 的卸载顺序冲突。PermaBox 会拒绝为该布局挂载持久化写层。检查点和恢复、SELinux enforcing、用户命名空间、其他 OCI 运行时及 OpenShift 不在此次测试范围内。

1. 在节点安装 `/usr/local/bin/permabox` 和 [`/etc/permabox/config.yaml`](../config.yaml)。将 `underlying_runtime` 指向实际的 `runc`，写层放在 CRI-O 存储目录之外的本地文件系统。首次验证时保持回收策略关闭。
2. 将 [99-permabox-vfs.conf](99-permabox-vfs.conf) 安装到 `/etc/crio/crio.conf.d/`。确认没有命令行参数、后续配置和 overlay 专用选项覆盖 VFS 设置。检查配置后，在维护窗口重启 CRI-O，并用 `crictl info` 检查 kubelet 实际连接的 CRI 端点。
3. 只在目标节点设置 `permabox.io.node-restriction.kubernetes.io/crio-vfs=true`，应用 [RuntimeClass](runtimeclass.yaml)。它与 containerd 的示例使用相同名称；请在独立测试集群中验证，避免覆盖正在使用的对象。
4. 在 `permabox-demo` 命名空间应用 [`deploy/statefulset.yaml`](../statefulset.yaml)，按[根文件系统持久化验证步骤](../README-cn.md)重建 Pod 并检查文件。

## 启用 StatefulSet 回收

在选中的 CRI-O 节点创建 `/opt/permabox`，所有者为 `root`，权限为 `0700`，然后部署[工作负载元数据桥接](../kubernetes/workload-bridge.yaml)。桥接清单使用 `permabox.io.node-restriction.kubernetes.io/pinned=true` 选择节点；请为该节点添加此标签，或在混用多种运行时的集群中调整清单的节点选择器。等待桥接 DaemonSet 就绪后，在 PermaBox 配置中设置 `workload_socket: /opt/permabox/workload.sock`，按需启用 `gc_on_workload_delete`、`gc_on_scale_to_zero` 或 `gc_workload_annotations`。按[通用部署指南](../README-cn.md)设置定期执行 `permabox gc`，以便临时无法清理或获取元数据时重试。仅删除 Pod 且 StatefulSet 仍需要该副本时，写层会保留。
