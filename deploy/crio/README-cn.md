# 在 CRI-O 上使用 PermaBox

[English](README.md)

[CRI-O 兼容性工作流](../../.github/workflows/compat-crio.yml) 在 Ubuntu 24.04（amd64）上运行原生 Kubernetes，使用 VFS 和 CRI-O 发布包自带的 conmon、runc：

| Kubernetes | CRI-O | 存储驱动 | OCI 运行时 |
| --- | --- | --- | --- |
| 1.34.0 | 1.34.0 | VFS | runc |
| 1.35.8 | 1.35.8 | VFS | runc |
| 1.36.4 | 1.36.4 | VFS | runc |
| 1.37.0 | 1.37.0 | VFS | runc |

每组都检查 Pod 重建后文件的创建、修改和删除是否保留，以及多副本隔离、部分缩容、缩容到零、删除 StatefulSet 和注解覆盖的行为。表中列出的是 CI 的精确配对，未覆盖其他补丁版本或混搭版本。

请使用独立节点或临时 CRI-O 实例。VFS 是 CRI-O 实例级设置，需要独立的 graph root 和 run root，会复制镜像层并占用更多空间；请勿在已有生产节点上直接切换存储驱动。

CRI-O 默认的 `overlay` 驱动不支持持久化：CRI-O 会在 OCI `delete` 前卸载容器根文件系统，与 PermaBox 的卸载顺序冲突。PermaBox 会拒绝为该布局挂载持久化写层。检查点和恢复、SELinux enforcing、用户命名空间、其他 OCI 运行时及 OpenShift 不在此次测试范围内。

1. 在节点安装 `/usr/local/bin/permabox` 和 [`/etc/permabox/config.yaml`](../config.yaml)。将 `underlying_runtime` 指向实际的 `runc`，写层放在 CRI-O 存储目录之外的本地文件系统。首次验证时保持回收策略关闭。
2. 将 [99-permabox-vfs.conf](99-permabox-vfs.conf) 安装到 `/etc/crio/crio.conf.d/`。确认没有命令行参数、后续配置和 overlay 专用选项覆盖 VFS 设置。检查配置后，在维护窗口重启 CRI-O，并用 `crictl info` 检查 kubelet 实际连接的 CRI 端点。
3. 只在目标节点设置 `permabox.io.node-restriction.kubernetes.io/crio-vfs=true`，应用 [RuntimeClass](runtimeclass.yaml)。CRI-O 工作负载使用 `runtimeClassName: permabox-crio`，底层 handler 仍叫 `permabox`。CRI-O 和 containerd 的 RuntimeClass 可以共存，并选择不同的节点。
4. 创建 `permabox-demo` 命名空间，运行 `kubectl -n permabox-demo apply -k examples/crio`，按[根文件系统持久化验证步骤](../README-cn.md)重建 Pod 并检查文件。

从旧版的 CRI-O `permabox` RuntimeClass 升级时，先创建 `permabox-crio`，再更新 StatefulSet 的 Pod 模板。保留旧 RuntimeClass 直到相关 Pod 滚动完成，确认没有其他用户后再删除或改作他用。保持节点、命名空间和 StatefulSet 名称不变，即可复用各副本原有的写层。

## 启用 StatefulSet 回收

在选中的 CRI-O 节点创建 `/opt/permabox`，所有者为 `root`，权限为 `0700`。为节点加上 `permabox.io.node-restriction.kubernetes.io/crio-gc=true`，再部署[工作负载元数据桥接](../kubernetes/workload-bridge.yaml)。如果此前只为桥接设置了通用 `pinned` 标签，设置新标签后即可移除它；`pinned` 也用于 containerd RuntimeClass 的调度。等待桥接 DaemonSet 就绪后，在 PermaBox 配置中设置 `workload_socket: /opt/permabox/workload.sock`，按需启用 `gc_on_workload_delete`、`gc_on_scale_to_zero` 或 `gc_workload_annotations`。按[通用部署指南](../README-cn.md)设置定期执行 `permabox gc`，以便临时无法清理或获取元数据时重试。

StatefulSet 的每个序号都有独立的写层。从两个副本缩到一个时，被缩掉的副本保留写层；该序号在同一节点恢复后可以复用。启用 `gc_on_scale_to_zero` 后，将整个 StatefulSet 缩到零会回收它已释放的所有写层。
