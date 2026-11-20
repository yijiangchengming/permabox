# 在 CRI-O 上使用 PermaBox

[English](README.md)

已通过 CRI 接口验证的组合为 CRI-O 1.34.0、VFS 存储驱动、conmon 2.1.13 和 runc 1.3.1。验证覆盖写入、修改和删除文件后重建 Pod sandbox；尚无完整 Kubernetes 集群验收。建议使用独立测试节点。VFS 是 CRI-O 实例级设置，会复制镜像层并占用更多空间；请勿在已有生产节点上直接切换存储驱动。

CRI-O 默认的 `overlay` 驱动不支持持久化：CRI-O 会在 OCI `delete` 前卸载容器根文件系统，与 PermaBox 的卸载顺序冲突。PermaBox 会拒绝为该布局挂载持久化写层。SELinux enforcing、用户命名空间和其他 OCI 运行时尚未验证。

1. 在节点安装 `/usr/local/bin/permabox` 和 [`/etc/permabox/config.yaml`](../config.yaml)。将 `underlying_runtime` 指向实际的 `runc`，写层放在 CRI-O 存储目录之外的本地文件系统。首次验证时保持回收策略关闭。
2. 将 [99-permabox-vfs.conf](99-permabox-vfs.conf) 安装到 `/etc/crio/crio.conf.d/`。确认没有命令行参数、后续配置和 overlay 专用选项覆盖 VFS 设置。检查配置后，在维护窗口重启 CRI-O，并用 `crictl info` 检查 kubelet 实际连接的 CRI 端点。
3. 只在目标节点设置 `permabox.io.node-restriction.kubernetes.io/crio-vfs=true`，应用 [RuntimeClass](runtimeclass.yaml)。它与 containerd 的示例使用相同名称；请在独立测试集群中验证，避免覆盖正在使用的对象。
4. 在 `permabox-demo` 命名空间应用 [`deploy/statefulset.yaml`](../statefulset.yaml)，按[根文件系统持久化验证步骤](../README-cn.md)重建 Pod 并检查文件。
