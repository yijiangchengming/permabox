# 用 Helm 在 K3s 上部署 PermaBox

[English](README.md) | 简体中文

适用于由 systemd 管理、使用内置 containerd 的 Linux K3s 节点。Chart 安装特权 DaemonSet 和 `RuntimeClass/permabox`。DaemonSet 在选定节点安装二进制，更新 K3s 当前使用的 containerd 配置模板，并在需要时重启 `k3s.service` 或 `k3s-agent.service`。安装器读取已启动的 CRI 验证注册后才就绪。整个流程无需登录节点；使用该 RuntimeClass 的业务 Pod 无需特权模式。

集群须允许安装器在专用命名空间使用特权 Pod、hostPID 和宿主机挂载。默认要求按 hostname 选定单个节点。设置 `allowParallelInstall=true` 后可以使用其他节点标签，首次安装时多个匹配节点可能同时重启 K3s；请安排维护窗口，避免同时重启多个控制平面节点。该 Chart 不适用于 rootless K3s、容器内运行且没有相应 systemd 单元的 K3s，或外接 containerd 的配置。

如需让指定目录不跨 Pod 重建保留，请在 Pod 模板上设置 `permabox.io/persist-exclude`，值为以逗号分隔的容器内规范绝对路径，例如 `/tmp,/var/cache`。每个路径必须对应镜像中已存在的目录，并且在可写 rootfs 中也是实际目录。拒绝 `/`、逗号、`.` 或 `..` 路径段以及互相重叠的路径。PermaBox 会在每个路径上挂载临时 OverlayFS，镜像内容（包括特殊文件）仍可见，运行期间的写入则进入临时层。同一容器内的排除目录共用一个上限为 4096 MiB 和 4096 个 inode 的 tmpfs。容器设置 `readOnlyRootFilesystem: true` 时，PermaBox 会以只读方式挂载排除目录。设置注解前已持久化的旧目录内容会被隐藏；移除注解后可能重新出现。

## 准备安装器镜像

发布流程会在推送稳定的 `vMAJOR.MINOR.PATCH` 标签时，将安装器镜像发布到 `ghcr.io/yijiangchengming/permabox-installer`。镜像使用不带 `v` 前缀的版本标签和 `sha-<完整提交 SHA>` 标签。GitHub 包的可见性需要单独设置；公开包才支持匿名拉取。

对应镜像发布后，将下面的 `IMAGE_REPO` 和 `IMAGE_TAG` 替换为实际值（版本号不带 `v`），然后继续安装：

```sh
IMAGE_REPO=ghcr.io/yijiangchengming/permabox-installer
IMAGE_TAG='<MAJOR.MINOR.PATCH>'
```

如果发布镜像尚不可用，或需要部署自定义构建，请从与 Chart 相同的版本构建安装器镜像，并推送到集群可访问的仓库。在项目根目录运行以下命令，K3s 节点会直接拉取镜像：

```sh
IMAGE_REPO=registry.example.com/infra/permabox-installer
IMAGE_TAG=0.1.0
docker buildx build --platform linux/amd64,linux/arm64 \
  -f deploy/k3s/Dockerfile -t "$IMAGE_REPO:$IMAGE_TAG" --push .
```

## 安装

先通过 `kubectl get nodes -L kubernetes.io/hostname` 取得目标节点的 hostname 标签值。设置 `NODE_HOSTNAME`，然后运行：

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

确认 `DESIRED` 非零且等于 `READY`，然后在业务 Pod 中设置 `spec.runtimeClassName: permabox`，并添加 `permabox.io/persist: "true"` 注解。多容器 Pod 应使用 `permabox.io/container: <容器名>` 选择一个容器。可直接部署 [StatefulSet 示例](../statefulset.yaml) 并按[文件持久化验收步骤](../README-cn.md#4-验证写入修改和删除能跨-pod-重建保留)检查结果；跳过手动注册和手动创建 RuntimeClass 的步骤。

Chart 将同一套 `nodeSelector` 和 `tolerations` 应用于安装器和 RuntimeClass。写层仍保存在节点本地；依赖已有数据的工作负载须固定到原节点。

| Helm 值 | 默认行为 |
| --- | --- |
| `k3s.dataDir` | `/var/lib/rancher/k3s`；覆盖 K3s 使用的 `--data-dir` 时同步设置此值 |
| `k3s.criEndpoint` | `unix:///run/k3s/containerd/containerd.sock` |
| `k3s.service` | `auto`，检测启用的 server 或 agent 服务；自定义 unit 需传如 `my-k3s.service` |
| `k3s.bin` | `/usr/local/bin/k3s` |
| `underlyingRuntime` | 使用 K3s 自带的 `<dataDir>/data/current/bin/runc` |
| `storageRoot` | `/var/lib/permabox/storage` |
| `storageRoots` | `{}`；存储别名到节点本地绝对路径的映射 |
| `trustedNamespaces` | `[]`；显式允许使用持久化的命名空间，升级时也需要配置 |
| `bridgeImage` | `{}`；默认沿用安装器镜像。设置 `bridgeImage.tag`，并按需设置 `bridgeImage.repository` 和 `bridgeImage.pullPolicy`，可独立固定已验证的桥接镜像 |
| `allowLegacyWrapperMigration` | `false`；使用旧包装器的节点须先停止所有 PermaBox 工作负载，再在安装模式下一次性启用此项 |
| `underlyingRuntimes` | `{}`；运行时别名到宿主机绝对二进制路径的映射 |
| `gcIdleTTL` | `"0s"`；Go 时长，默认保留已释放的持久化数据，GC 仍重试残留清理 |
| `gcOnWorkloadDelete` | `false`；设为 `true` 后，删除 StatefulSet 时回收其已安全释放的写层 |
| `gcOnScaleToZero` | 默认 `false`。开启后，StatefulSet 的 `spec.replicas` 为 `0` 时回收其已释放的写层。若需保留缩到零后的数据，请保持关闭，并避免通过工作负载注解启用。 |
| `gcWorkloadAnnotations` | `false`；设为 `true` 后，允许 Pod 模板注解单独覆盖上面两项默认值 |
| `gcIntervalSeconds` | `60`；每次 GC 前等待的秒数（正整数） |
| `persistAnnotation` | `permabox.io/persist`；更改后须同步修改业务 Pod |

在 Pod 上用 `permabox.io/storage-root` 为选中的持久化容器指定存储别名。命名空间列入 `trustedNamespaces` 的 Pod 可以用 `permabox.io/underlying-runtime` 为业务容器指定运行时别名。sandbox 使用创建时的默认运行时。参见[别名配置](../README-cn.md#1-安装到目标节点)。升级时请使用新的发布镜像标签，或用新标签推送自定义构建。仍有容器记录时，安装器允许调整工作负载回收开关，以及将旧默认套接字迁移到 Bridge 专用目录。修改其他运行时配置前，请先停止 PermaBox 工作负载。

发布运行时版本前，安装器会验证暂存配置，并获取创建准入锁。它使用 OCI 分发的同一套路径和可执行文件检查，验证默认运行时、每个配置的别名，以及 `/run/permabox` 中每条容器路由固定的运行时。K3s 的 `data/current` 链接仍可使用，前提是链接路径和目标均由 root 控制。解析到 PermaBox 包装器或二进制的路径会被拒绝。升级前，请在每个选定节点检查自定义运行时和存储路径。存储根目录及其中的命名空间和写层目录须为普通目录，且只有 root 可以写入。

安装器在每次查询 CRI 前检查宿主机上的 K3s 可执行文件，回滚时也会执行这项检查。受信的符号链接按宿主机根路径解析。复用缓存版本前，安装器会验证目录、二进制和配置，并核对内容与暂存摘要是否一致。校验失败会中止启用。

安装器会先确认元数据桥接可用，并检查现有写层是否有可核验的 StatefulSet 身份，再发布升级。若发现旧写层问题，请处理后重试。CRI handler 切换失败时会恢复事务记录的原状态；请预留时间让 K3s 重启并恢复原 handler。

请让 root 控制 `/opt/permabox-k3s` 及其上级目录。安装器在加锁或恢复事务前会检查状态目录和已有锁文件。受信的旧版目录会收紧为 `0700`。其他用户可写的目录、不安全的锁文件和指向事务目录的符号链接会被拒绝。

元数据 Bridge DaemonSet 只挂载宿主机的 `/opt/permabox-k3s-bridge`。Bridge 和安装器都会检查该目录由 root 所有，并将权限设为 `0700`，因此两套 DaemonSet 可以按任意顺序滚动更新。安装器将 `workload_socket` 设为 `/opt/permabox-k3s-bridge/workload.sock`。请使用配套的 Chart 和安装器镜像一起升级。Bridge 沿用容器内的挂载路径，因此常规更新安装器时可以继续固定兼容的 `bridgeImage`。

从 `/opt/permabox-k3s/workload.sock` 迁移时，Bridge 可能先于安装器切换路径。受影响节点上的新持久化容器创建和基于工作负载的 GC 可能暂时暂停。宿主机事务失败时，安装器会恢复旧版运行时及套接字路径。若旧 Bridge 已退出，请解决启用失败的原因，待安装器通过专用 Bridge 重试成功后恢复上述操作。通过 Helm 跨此次套接字变更降级前，请先排空 PermaBox 工作负载，并搭配使用旧 Chart 和对应的安装器镜像。

若原包装器没有创建准入锁，请停止所有 PermaBox Pod，并阻止控制器创建替代 Pod。以安装模式设置 `--set allowLegacyWrapperMigration=true`，确认升级就绪后再恢复业务或切换到卸载模式。新准入锁只能跟踪升级后通过包装器发起的创建操作。承载 PermaBox 工作负载的节点应使用带独立 Bridge DaemonSet 的 Chart。

回滚时，安装器会在恢复节点文件前核对事务记录中的 K3s 服务，并验证旧版本链接指向的缓存。主进程身份不符、缓存缺失或校验失败时，安装器会保留待处理事务。已停止的 K3s 服务仍可恢复。正常协调和回滚均通过宿主机 PID 1 的根目录访问 containerd 配置目录。该目录及其中的普通配置和模板文件须由 root 控制，且路径不能包含符号链接。

两项工作负载回收策略仅适用于 StatefulSet，可独立开启，例如 Helm 的 `--set gcOnWorkloadDelete=true --set gcOnScaleToZero=true`。已释放的写层由下一轮周期任务检查，默认间隔 60 秒，可通过 `gcIntervalSeconds` 调整。两项策略都无需等待 `gcIdleTTL`。即使关闭回收，Helm 也会启动只读元数据桥接以核验 StatefulSet UID。升级前请参考[旧写层迁移说明](../README-cn.md)。

旧版本中由 Deployment 留下的写层，在释放后仍可通过 `gcIdleTTL` 回收。

手动删除 Pod、滚动更新和缩容到非零副本数不会单独触发这两项策略。删除 StatefulSet 可在下一次符合条件的扫描回收；缩到 0 则需至少等待首次批准一分钟，并由后续扫描再次确认。两项策略只处理已释放且归属已确认的写层。回收保护规则见[定期回收](../README-cn.md#定期回收gc)。

### 按工作负载覆盖回收策略

如需对现有写层关闭两项工作负载回收策略，请将 `gcOnWorkloadDelete=false`、`gcOnScaleToZero=false` 和 `gcWorkloadAnnotations=false` 一并设置。已经移入回收目录的写层仍可能被删除。如需长期保留已释放写层，还应设置 `gcIdleTTL=0s`。

通过 Helm 传入 `--set gcWorkloadAnnotations=true` 后，在 StatefulSet 的 Pod 模板中按需设置注解。即使两个全局默认值均为 `false`，也可以只为指定工作负载开启：

```yaml
spec:
  template:
    metadata:
      annotations:
        permabox.io/persist: "true"
        permabox.io/gc-on-workload-delete: "true"
        permabox.io/gc-on-scale-to-zero: "false"
```

两项注解都接受字符串 `"true"` 或 `"false"`，省略任一项就继承对应的 Helm 默认值。无效值按 `"false"` 处理。注解在容器创建时读取并随写层记录保存；修改模板后，由新建容器采用新设置，已释放且未被重新使用的写层保留原记录。注解仅覆盖对应的工作负载回收策略，`gcIdleTTL` 仍独立生效。

从曾丢弃这些覆盖值的运行时版本升级后，受影响的写层会沿用节点默认策略，直到新建容器使用该写层成功启动并记录工作负载的设置。

安装器识别渲染结果的配置版本 2 或 3，只修改对应的 `config.toml.tmpl` 或 `config-v3.toml.tmpl` 中自己的配置块。同名 runtime 已由其他方式注册时，安装会失败。发生错误时，可通过 `kubectl -n permabox-system logs daemonset/permabox-installer -c installer` 检查；多节点场景请查看对应节点上的安装器 Pod 日志。

## 卸载

先停止所有使用 `RuntimeClass/permabox` 的工作负载。使用**同一个 release、命名空间和之前所有安装节点**执行清理；不要先运行 `helm uninstall`。确认清理 DaemonSet 就绪，且 `DESIRED` 包含所有需要清理的节点后，再卸载 release：

```sh
helm upgrade permabox ./deploy/helm/permabox \
  --namespace permabox-system --reuse-values --set mode=remove \
  --wait --timeout 10m
kubectl -n permabox-system rollout status daemonset/permabox-installer --timeout=5m
kubectl -n permabox-system get daemonset permabox-installer
helm uninstall permabox --namespace permabox-system
```

卸载模式会关闭新建容器的准入，等待已开始的创建操作完成，同时允许继续删除容器。若仍有运行时记录，或主机事务回滚并恢复了 handler，准入继续关闭，桥接继续运行。删除剩余工作负载后重试卸载模式；如需取消清理，则成功执行一次安装模式以重新开放准入。确认主机端清理完成后，再通过 Helm 卸载桥接。卸载模式不执行 GC，保留存储根目录和持久化数据。节点离线时应待其恢复后确认清理结果。

部署模式参考 [Sysbox Kubernetes DaemonSet](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md) 和 [Kata Deploy](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy)。模板行为参见 [K3s containerd 配置文档](https://docs.k3s.io/advanced#configuring-containerd)。
