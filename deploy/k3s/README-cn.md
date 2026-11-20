# 用 Helm 在 K3s 上部署 PermaBox

[English](README.md) | 简体中文

适用于由 systemd 管理、使用内置 containerd 的 Linux K3s 节点。Chart 安装特权 DaemonSet 和 `RuntimeClass/permabox`。DaemonSet 在选定节点安装二进制，更新 K3s 当前使用的 containerd 配置模板，并在需要时重启 `k3s.service` 或 `k3s-agent.service`。安装器读取已启动的 CRI 验证注册后才就绪。整个流程无需登录节点；使用该 RuntimeClass 的业务 Pod 无需特权模式。

集群须允许安装器在专用命名空间使用特权 Pod、hostPID 和宿主机挂载。默认要求按 hostname 选定单个节点。设置 `allowParallelInstall=true` 后可以使用其他节点标签，首次安装时多个匹配节点可能同时重启 K3s；请安排维护窗口，避免同时重启多个控制平面节点。该 Chart 不适用于 rootless K3s、容器内运行且没有相应 systemd 单元的 K3s，或外接 containerd 的配置。

## 准备安装器镜像

发布流程会在推送稳定的 `vMAJOR.MINOR.PATCH` 标签时，将安装器镜像发布到 `ghcr.io/yijiangchengming/permabox-installer`。镜像使用不带 `v` 前缀的版本标签和 `sha-<完整提交 SHA>` 标签。GitHub 包的可见性需要单独设置；公开包才支持匿名拉取。

对应镜像发布后，将下面的 `IMAGE_REPO` 和 `IMAGE_TAG` 替换为实际值（版本号不带 `v`），然后继续安装：

```sh
IMAGE_REPO=ghcr.io/yijiangchengming/permabox-installer
IMAGE_TAG='<MAJOR.MINOR.PATCH>'
```

如果发布镜像尚不可用，或需要部署自定义构建，请从项目根目录构建并推送到集群可拉取的镜像仓库。安装器镜像应包含 Pod 别名、`gc` 和 `validate-config` 支持。无需在 K3s 节点上构建或复制文件：

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
| `underlyingRuntimes` | `{}`；运行时别名到宿主机绝对二进制路径的映射 |
| `gcIdleTTL` | `"0s"`；Go 时长，默认保留已释放的持久化数据，GC 仍重试残留清理 |
| `gcOnWorkloadDelete` | `false`；设为 `true` 后，删除 StatefulSet 时回收其已安全释放的写层 |
| `gcOnScaleToZero` | `false`；设为 `true` 后，StatefulSet 的 `spec.replicas` 为 `0` 时回收其已安全释放的写层 |
| `gcWorkloadAnnotations` | `false`；设为 `true` 后，允许 Pod 模板注解单独覆盖上面两项默认值 |
| `gcIntervalSeconds` | `60`；每次 GC 前等待的秒数（正整数） |
| `persistAnnotation` | `permabox.io/persist`；更改后须同步修改业务 Pod |

在 Pod 上用 `permabox.io/storage-root` 为选中的持久化容器指定存储别名，用 `permabox.io/underlying-runtime` 为业务容器指定运行时别名；sandbox 使用创建时的默认运行时。参见[别名配置](../README-cn.md#1-安装到目标节点)。升级时请使用新的发布镜像标签，或用新标签推送自定义构建。仍存在容器记录时，安装器允许单独修改工作负载回收开关，修改其他运行时配置前请先停止 PermaBox 工作负载。

安装器通过 `permabox validate-config` 验证暂存的配置，验证成功后才发布。默认运行时和别名均不得指向安装器包装器 `/opt/permabox-k3s/runtime`，否则会递归调用 PermaBox。

安装器会先确认元数据桥接可用，并检查现有写层是否有可核验的 StatefulSet 身份，再发布升级。若发现旧写层问题，请处理后重试。CRI handler 切换失败时会恢复事务记录的原状态；请预留时间让 K3s 重启并恢复原 handler。

两项工作负载回收策略仅适用于 StatefulSet，可独立开启，例如 Helm 的 `--set gcOnWorkloadDelete=true --set gcOnScaleToZero=true`。已释放的写层由下一轮周期任务检查，默认间隔 60 秒，可通过 `gcIntervalSeconds` 调整。两项策略都无需等待 `gcIdleTTL`。即使关闭回收，Helm 也会启动只读元数据桥接以核验 StatefulSet UID。升级前请参考[旧写层迁移说明](../README-cn.md)。

旧版本中由 Deployment 留下的写层，在释放后仍可通过 `gcIdleTTL` 回收。

手动删除 Pod、滚动更新和缩容到非零副本数不会单独触发这两项策略。删除 StatefulSet 可在下一次符合条件的扫描回收；缩到 0 则需至少等待首次批准一分钟，并由后续扫描再次确认。两项策略只处理已释放且归属已确认的写层。回收保护规则见[定期回收](../README-cn.md#定期回收gc)。

### 按工作负载覆盖回收策略

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

卸载模式不执行 GC；它移除运行时注册和安装器管理的二进制、配置，并保留存储根目录和持久化数据。节点离线时应待其恢复后确认清理结果。

部署模式参考 [Sysbox Kubernetes DaemonSet](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md) 和 [Kata Deploy](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy)。模板行为参见 [K3s containerd 配置文档](https://docs.k3s.io/advanced#configuring-containerd)。
