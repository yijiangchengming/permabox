# PermaBox

[English](README.md) · [MIT 许可证](LICENSE)

PermaBox 让 Kubernetes Pod 在同一台 Linux 节点上重建后，仍能读取容器根文件系统中此前写入的数据。它包装兼容 runc 的 OCI 运行时，在节点上挂载本地 OverlayFS 可写层。业务容器无需以特权模式运行。

PermaBox 适用于 Pod 名称稳定的工作负载，例如 StatefulSet。可写层按存储根目录、命名空间和 Pod 名称查找，每个 Pod 支持一个持久化容器。它不会跨节点迁移数据，也不管理独立挂载的卷或提供备份。应用数据需要由 Kubernetes 单独管理时，请使用 PersistentVolume。

Deployment 副本的 Pod 名称不稳定，因此不支持 Deployment 持久化。需要让各副本恢复各自的写层时，请使用 StatefulSet。

## 功能

- **根文件系统持久化：** 同名 Pod 重建后保留文件新建、修改和删除结果，包括 OverlayFS whiteout。
- **运行时和存储选择：** 部署时配置默认路径和别名，Pod 可按需选择；后续 OCI 命令沿用容器创建时选定的运行时。
- **K3s 自动安装：** Helm 部署节点安装器，为 K3s 内置 containerd 注册运行时。安装器需要宿主机权限，业务 Pod 无需特权模式。
- **数据回收：** 默认保留已释放的写层，可配置空闲 TTL，也可在 StatefulSet 删除或缩到零副本时回收。两项工作负载回收策略都支持 Pod 模板注解覆盖。

## 部署

| 环境 | 指南 |
| --- | --- |
| 由 systemd 管理、使用内置 containerd 的 K3s | [使用 Helm 安装](deploy/k3s/README-cn.md) |

其他发行版的注册方式及已验证版本见 [Kubernetes 兼容矩阵](deploy/kubernetes/README-cn.md)。
| Linux 上独立运行的 containerd | [手动部署与验收](deploy/README-cn.md) |

安装 `permabox` RuntimeClass 后，在 Pod 中设置 `spec.runtimeClassName: permabox` 和注解 `permabox.io/persist: "true"`。多容器 Pod 使用 `permabox.io/container` 选择需要持久化根文件系统的容器。可参考 [StatefulSet 示例](deploy/statefulset.yaml)。

启用持久化前，把命名空间加入 Helm 的 `trustedNamespaces` 或节点配置的 `trusted_namespaces`。只允许命名空间内所有有权创建 Pod 的用户都可信的命名空间。

请将工作负载固定到保存其数据的节点，使用兼容的本地文件系统。可写层不支持 NFS 或共享存储。生产部署前应在目标节点验证内核、containerd 快照器和底层运行时的组合；更换镜像前请参阅[安全与镜像升级说明](deploy/README-cn.md)。

## 开发

在 Linux 上使用 Go 1.23 或更高版本：

```sh
make build
make check
```

二进制输出到 `bin/permabox`。项目检查还需要 Helm、Bash、jq 和 ShellCheck。可在支持 OverlayFS 且有权限创建挂载命名空间的 Linux 主机上运行 `make test-kernel`，验证实际挂载与回收。

| 路径 | 职责 |
| --- | --- |
| [`cmd/permabox`](cmd/permabox) | OCI 包装器与命令转发 |
| [`internal`](internal) | 配置、挂载生命周期和 Kubernetes 归属检查 |
| [`deploy`](deploy) | Helm Chart、K3s 安装器和 containerd 示例 |

开发流程和 GHCR 镜像发布方式见 [贡献与发布指南](CONTRIBUTING.md)。
