# 在 CRI-O 上使用 PermaBox

[English](README.md)

兼容性工作流在 Ubuntu 24.04（amd64）上运行原生 Kubernetes，使用 CRI-O 发布包自带的 conmon 和 runc。[CI 工作流](../../.github/workflows/compat-crio.yml)

| Kubernetes | CRI-O | 存储驱动 | OCI 运行时 |
| --- | --- | --- | --- |
| 1.34.0 | 1.34.0 | overlay | runc |
| 1.35.8 | 1.35.8 | overlay | runc |
| 1.36.4 | 1.36.4 | overlay | runc |
| 1.37.0 | 1.37.0 | overlay | runc |

矩阵中的每个组合均已通过验证，覆盖 rootfs 持久化、副本隔离、写层回收，以及同一个可写 `hostPath` 的直接挂载和目录 `subPath` 挂载。测试会在 Pod 重建后检查两个卷路径，并确认 PermaBox 回收 rootfs 写层后节点上的卷文件仍然存在。测试还分别执行 CRI `StopContainer` 和 `RemoveContainer`，检查每一步结束后的挂载状态。PVC 和 CSI 后端、只读挂载、挂载传播、`subPathExpr`、检查点和恢复、其他 OCI 运行时、用户命名空间、SELinux enforcing 和 OpenShift 均在矩阵范围之外。[测试脚本](smoke_test.sh)

使用 CRI-O 原生 `overlay` 驱动时，PermaBox 将容器 rootfs 挂载到 `<state_dir>/<container-id>.rootfs`，并更新运行时 OCI bundle 的 `root.path`。CRI-O 在 `StopContainer` 时可以卸载并替换自己的 `merged` 目录，PermaBox 在 OCI `delete` 后清理独立 rootfs 和 lower 绑定。持久化写层使用 `<storage_root>/<namespace>/<pod-name>/upper`。

创建中断后，让 CRI-O 在原挂载命名空间重试 OCI 删除。如果配置状态或残留挂载使 GC 无法确认可以安全清理，GC 会保留挂载记录。排查时使用该容器记录中的运行时和 bundle 路径。[恢复步骤](../README-cn.md)

1. 在节点安装 `/usr/local/bin/permabox` 和 [`/etc/permabox/config.yaml`](../config.yaml)。将 `underlying_runtime` 指向实际的 `runc`，写层放在 CRI-O 存储目录之外的本地文件系统。首次验证时保持回收策略关闭。
2. 使用原生 overlay 的节点，将 [overlay 运行时配置](99-permabox-overlay.conf) 安装到 `/etc/crio/crio.conf.d/`。该配置只注册 OCI handler。使用原生 overlay 作为 CRI-O 的存储驱动。按需设置 `monitor_path` 指向 conmon，在维护窗口检查合并后的配置并重启 CRI-O，再使用 `crictl info` 检查 kubelet 实际连接的 CRI 端点。
3. 只在目标节点设置 `permabox.io.node-restriction.kubernetes.io/crio-overlay=true`，应用 [RuntimeClass](runtimeclass.yaml)。CRI-O 工作负载使用 `runtimeClassName: permabox-crio`，底层 handler 仍叫 `permabox`。CRI-O 和 containerd 的 RuntimeClass 可以共存，并选择不同的节点。
4. 在节点创建 root 所有、权限为 `0700` 的 `/opt/permabox`，设置节点标签 `permabox.io.node-restriction.kubernetes.io/crio-gc=true`，安装[元数据桥接](../kubernetes/workload-bridge.yaml)并等待 DaemonSet 就绪。示例配置使用 `workload_socket: /opt/permabox/workload.sock`。关闭回收时也需要桥接。
5. 在节点配置中设置 `trusted_namespaces: [permabox-demo]`。创建 `permabox-demo` 命名空间，运行 `kubectl -n permabox-demo apply -k examples/crio`，按[根文件系统持久化验证步骤](../README-cn.md)重建 Pod 并检查文件。

从旧版的 CRI-O `permabox` RuntimeClass 升级时，先创建 `permabox-crio`，再更新 StatefulSet 的 Pod 模板。保留旧 RuntimeClass 直到相关 Pod 滚动完成，确认没有其他用户后再删除或改作他用。写层复用要求节点和 StatefulSet UID 保持不变。没有可核验归属的旧写层请按[通用部署指南](../README-cn.md)处理。

从 VFS 部署升级时，请先在旧版本上停止工作负载并清理挂载，将应用数据备份到写层之外。为 overlay 准备独立的 CRI-O 存储，先设置新的 `crio-overlay` 节点标签，再应用 RuntimeClass。在新建干净写层后通过应用恢复数据。旧版 PermaBox 仍可用于 VFS 工作负载。

## 启用 StatefulSet 回收

元数据桥接就绪后，按需启用 `gc_on_workload_delete`、`gc_on_scale_to_zero` 或 `gc_workload_annotations`。按[通用部署指南](../README-cn.md)设置定期执行 `permabox gc`，清理已释放的写层并重试临时故障。

StatefulSet 的每个序号都有独立的写层。从两个副本缩到一个时，被缩掉的副本保留写层；该序号在同一节点恢复后可以复用。启用 `gc_on_scale_to_zero` 后，将整个 StatefulSet 缩到零会回收它已释放的所有写层。
