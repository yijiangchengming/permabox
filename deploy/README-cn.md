# 部署 PermaBox

[English](README.md) | 简体中文

使用 K3s 内置 containerd 时，可按 [Helm 自动部署指南](k3s/README-cn.md)安装。这份指南用于手动部署及 StatefulSet 持久化验收。

示例在一台 Linux 节点上运行单副本 StatefulSet。Pod 重建后仍使用同一命名空间和 Pod 名，PermaBox 因此可以重新挂载同一份本地可写层。无头 Service 为 StatefulSet 提供稳定的网络身份；示例容器不监听端口。

## 1. 安装到目标节点

选择一台 `Ready`、可调度、运行 containerd 且支持 OverlayFS 的 Linux 节点。用本地磁盘作为持久化存储；`upper` 和 `work` 必须在同一个文件系统上，且该文件系统须支持 OverlayFS 所需的扩展属性和有效的 `d_type`。建议先用本地 ext4 验证；不要把 NFS 当作可写层，也不要假设其他共享文件系统支持该用途。

在目标节点上，从包含 `deploy/` 的目录运行下列命令。先把与节点架构匹配的已构建 PermaBox 二进制放在当前目录的 `./permabox`，并确认底层运行时的实际路径：

```sh
sudo install -o root -g root -m 0755 ./permabox /usr/local/bin/permabox
sudo install -d -o root -g root -m 0755 /etc/permabox
sudo install -o root -g root -m 0644 deploy/config.yaml /etc/permabox/config.yaml
sudo install -d -o root -g root -m 0700 /var/lib/permabox/storage
sudo install -d -o root -g root -m 0700 /opt/permabox
sudo test -x /usr/bin/runc
```

默认值见 [config.yaml](config.yaml)。若节点上的 `runc` 不在 `/usr/bin/runc`，修改 `underlying_runtime` 为其绝对路径，不能指回 PermaBox。其他底层运行时应先在目标节点验证兼容性。运行时状态固定在 `/run/permabox`，用于跟踪和清理挂载；持久化数据保存在 `<storage_root>/<namespace>/<pod-name>/upper`，不在 `/run` 下。调试日志默认写入宿主机 `/var/log/permabox.log`。

运行示例前，在节点配置中设置 `trusted_namespaces: [permabox-demo]`。默认空名单拒绝持久化，普通运行时透传仍可使用。仅当你信任命名空间内所有 Pod 创建者访问该命名空间的全部写层时，才将其加入名单。核验 StatefulSet UID 无法隔离同一命名空间内有权创建 Pod 的用户。升级时请显式配置名单。

在同一节点手动部署多个运行时实例时，为每个实例使用独立配置文件，并将 `state_dir` 设为各自专用、由 root 控制的绝对目录。默认值为 `/run/permabox`。每个实例的 OCI 包装器和 GC 服务须使用同一份配置，各实例也应使用独立的存储根目录。切换状态目录前，请先排空并清理已有容器。K3s Helm 安装器使用默认状态目录。

PermaBox 会在调用运行时前拒绝不支持的 OCI 命令、无法识别的生命周期参数，以及无法读取或无效的创建配置。升级前请检查运行时的 OCI 命令兼容性。状态目录必须是运行时用户拥有的私有目录。请使用受信的上级目录，并避免在状态路径中使用符号链接。

请使用由 CRI 管理的 OCI bundle，并将其置于受信任且不含符号链接的路径下。bundle 目录及普通文件 `config.json` 仅允许其受信任的所有者写入，配置上限为 4 MiB。底层运行时会在 PermaBox 检查后重新读取配置，因此整个 `create` 期间应保持配置不变。

| 环境变量 | 用途 |
| --- | --- |
| `PERMABOX_CONFIG` | 配置文件路径，默认 `/etc/permabox/config.yaml` |
| `PERMABOX_STORAGE_ROOT` | 覆盖 `storage_root` |
| `PERMABOX_UNDERLYING_RUNTIME` | 覆盖 `underlying_runtime` |
| `PERMABOX_LOG_FILE` | 覆盖 `log_file` |
| `PERMABOX_PERSIST_ANNOTATION` | 覆盖 `persist_annotation` |
| `PERMABOX_GC_IDLE_TTL` | 覆盖 `gc_idle_ttl` |

节点配置须为不超过 1 MiB 的普通文件。配置文件、上级目录及路径中的符号链接均应由 root 控制。运行时和 GC 都要求 `state_dir` 由运行时用户拥有且仅该用户可访问，上级目录须可信，路径中不能有符号链接。

环境变量须由宿主机运行时进程继承，写入业务容器的 `env` 无效。建议使用节点配置文件；无需给 OCI 调用添加 PermaBox 专用命令行参数。

可在节点配置文件中定义存储和运行时别名，值必须是宿主机绝对路径。例如：

```yaml
storage_roots:
  fast: /mnt/local-ssd/permabox
underlying_runtimes:
  gpu: /usr/bin/nvidia-container-runtime
```

在 Pod（或 StatefulSet 的 `spec.template.metadata.annotations`）上设置 `permabox.io/storage-root: fast`，为选中持久化的业务容器使用 `fast` 对应的写层根目录。设置 `permabox.io/underlying-runtime: gpu`，为 Pod 的业务容器选择 `gpu` 运行时，即使业务容器未开启持久化；sandbox 始终使用默认运行时。省略注解时使用 `storage_root` 和 `underlying_runtime`。别名只是路径选择，更改存储根目录不会迁移旧数据。请确保每个目标节点上的存储为本地文件系统、运行时二进制已安装且可以执行。

默认运行时和所有运行时别名都应位于 root 控制的路径上，只有 root 可以修改可执行文件或变更其路径。PermaBox 会检查符号链接经过的路径组件及最终目标。升级期间也应保护这些路径，并以原子方式替换可执行文件或版本链接。

各本地存储根目录、命名空间目录和 Pod 写层目录应由 root 控制。PermaBox 拒绝这些目录中的符号链接及所属组或其他用户的写权限。每个写层的 OverlayFS `work` 目录必须由运行时用户拥有，并限制为仅该用户可访问。GC 会跳过不安全的存储路径。升级前请检查现有路径和标记文件，迁移链接路径前先排空受影响的工作负载。

创建容器前，PermaBox 会按设备号和 inode 检查写层是否已被占用，包括绑定挂载以不同路径指向同一写层的情况。新挂载记录会保存设备号和 inode。如果其他挂载命名空间的旧记录阻止创建，请先在原命名空间完成清理。使用独立状态目录的运行时实例仍须使用独立存储。`container.json` 的读写上限统一为 24 MiB，可覆盖有效 OCI 配置中容器名的 JSON 转义。

## 2. 注册 containerd handler

挂载记录包含存储目录身份时，PermaBox 会在提交归属或标记写层已释放前核对目录身份。发现不匹配时会保留记录以便排查。即使目录通过其他路径可见，GC 也会保护记录指向的目录。回收审批只适用于同一存储目录和释放标记。最终删除会校验设备号和 inode，并在遇到挂载边界时停止。旧凭据缺少设备号的回收目录会保留，供管理员核查。

用 `containerd --version` 确认版本，并确认 kubelet 使用的 containerd 实例及其实际配置文件（通常是 `/etc/containerd/config.toml`）。备份该文件，按实际配置格式合并下面一个片段中的 `permabox` 表及 `options` 子表；保留已有的 CRI、镜像、快照器及默认运行时设置，顶层 `version` 只写一次。

| containerd / 配置格式 | 片段 | CRI runtime 插件 |
| --- | --- | --- |
| 1.x / `version = 2` | [containerd-1.x-v2.toml](containerd-1.x-v2.toml) | `io.containerd.grpc.v1.cri` |
| 2.x / `version = 3` | [containerd-2.x-v3.toml](containerd-2.x-v3.toml) | `io.containerd.cri.v1.runtime` |

若 2.x 实例仍使用 v2 配置，先用 `containerd --config /etc/containerd/config.toml config migrate` 生成迁移结果，审查主配置及其 `imports`（该命令不会逐个迁移子配置），再合并 v3 片段；不要只改 `version` 数字。`SystemdCgroup = true` 适用于使用 `systemd` 驱动的节点；按现有运行时及 kubelet 的配置设置该值。`BinaryName` 指向包装器，`runtime_type` 仍使用 runc v2 shim。

`pod_annotations` 是从 Pod 转发到 OCI spec 的键白名单。如果更改 `persist_annotation`，同步修改白名单和 StatefulSet 的 Pod 模板注解；启用值仍为字符串 `"true"`。PermaBox 跳过 CRI sandbox，只为符合条件的业务容器挂载持久化层。建议使用示例中的固定注解 `permabox.io/container: demo` 指定单个容器名；如 Pod 内有多个容器，不要让它们共用以命名空间和 Pod 名为键的可写层。

在维护窗口内检查配置，再重启并确认 CRI 可用。以下以 systemd 和默认配置路径为例；`crictl` 需指向 kubelet 使用的同一个 CRI 端点。

```sh
sudo containerd --config /etc/containerd/config.toml config dump
sudo systemctl restart containerd
sudo systemctl is-active containerd
sudo crictl info
```

## 3. 只允许一个节点并部署示例

在有 `kubectl` 权限的管理终端执行。将 `NODE` 改为上面已经安装并验证的节点名。仅给这一台节点加标签；如果检查列出多台节点，先在确认其他节点无相关工作负载后移除它们的同名标签，再继续。`RuntimeClass` 将 Linux 和该标签两个条件都加到调度约束中。目标节点不可用时，让 Pod 等待该节点恢复，不能通过给其他节点加标签来迁移本地数据。

```sh
(
  set -eu
  NODE='replace-with-node-name'
  LABEL=permabox.io.node-restriction.kubernetes.io/pinned
  kubectl label node "$NODE" "$LABEL=true" --overwrite
  kubectl get nodes -l "$LABEL=true" -o wide
  test "$(kubectl get nodes -l "$LABEL=true" -o name)" = "node/$NODE"

  kubectl apply -f deploy/kubernetes/workload-bridge.yaml
  kubectl -n permabox-system rollout status daemonset/permabox-workload-bridge --timeout=180s
  kubectl create namespace permabox-demo
  kubectl apply -f deploy/runtimeclass.yaml
  kubectl -n permabox-demo apply -f deploy/statefulset.yaml
  kubectl -n permabox-demo rollout status statefulset/permabox-demo --timeout=180s
  test "$(kubectl -n permabox-demo get pod permabox-demo-0 -o jsonpath='{.spec.nodeName}')" = "$NODE"
)
```

只有相同 StatefulSet UID 才能复用写层。旧写层仍在时，同名的新 StatefulSet 不会接管它。升级时，可继续使用归属标记能匹配当前 UID 的旧写层。若历史写层没有可核实的归属，先停止工作负载，确认卸载并备份、归档旧目录，再创建干净写层并通过应用恢复数据。元数据服务暂时不可用时，新的持久化容器创建会等待重试，现有数据不受影响。

## 4. 验证写入、修改和删除能跨 Pod 重建保留

使用干净的示例数据目录，并在整个验证过程中保持镜像不变。下列命令在容器的 `/etc` 中新建文件，改写镜像自带的 `/etc/profile`，删除镜像自带的 `/etc/inittab`，再正常删除 Pod。StatefulSet 重建同名 Pod 后，依次核对新 UID、同一节点、同一镜像 ID 和三个文件结果。测试路径都在根文件系统上，不使用挂载卷。

```sh
(
  set -eu
  ns=permabox-demo
  pod=permabox-demo-0
  k() { kubectl -n "$ns" "$@"; }
  pod_field() { k get pod "$pod" -o "jsonpath=$1"; }

  old_uid=$(pod_field '{.metadata.uid}')
  old_node=$(pod_field '{.spec.nodeName}')
  old_image=$(pod_field '{.status.containerStatuses[0].imageID}')
  test -n "$old_uid"
  test -n "$old_node"
  test -n "$old_image"

  k exec "$pod" -c demo -- /bin/sh -ec '
    test ! -e /etc/permabox-created
    test -f /etc/profile
    test "$(cat /etc/profile)" != "# modified-by-permabox"
    test -f /etc/inittab
    printf "%s\n" created-by-permabox > /etc/permabox-created
    printf "%s\n" "# modified-by-permabox" > /etc/profile
    rm /etc/inittab
  '

  k delete pod "$pod" --wait=true --timeout=180s
  retries=0
  until new_uid=$(pod_field '{.metadata.uid}' 2>/dev/null) &&
        [ -n "$new_uid" ] && [ "$new_uid" != "$old_uid" ]; do
    retries=$((retries + 1))
    if [ "$retries" -ge 90 ]; then printf '%s\n' '等待新 Pod 超时' >&2; exit 1; fi
    sleep 2
  done
  k wait --for=condition=Ready "pod/$pod" --timeout=180s
  test "$(pod_field '{.metadata.uid}')" = "$new_uid"
  test "$(pod_field '{.spec.nodeName}')" = "$old_node"
  test "$(pod_field '{.status.containerStatuses[0].imageID}')" = "$old_image"

  k exec "$pod" -c demo -- /bin/sh -ec '
    test "$(cat /etc/permabox-created)" = created-by-permabox
    test "$(cat /etc/profile)" = "# modified-by-permabox"
    test ! -e /etc/inittab
  '
  printf '%s\n' '验证通过：新建、修改和删除均跨 Pod 重建保留'
)
```

失败时先用 `kubectl -n permabox-demo describe pod permabox-demo-0` 检查调度和创建事件，再在目标节点查看 `journalctl -u containerd`、PermaBox 日志和存储目录。默认情况下，删除示例 StatefulSet 和 Service 后，已保存的 `upper` 仍留在节点上。需要重新做干净测试时，请换一个新命名空间，或在停止工作负载并确认相关挂载已卸载后备份并归档旧数据。不要强制删除尚可能在运行的 Pod。

## 定期回收（GC）

GC 删除已进入回收目录的写层时会分批读取目录条目，并在进入每个目录前检查挂载身份。

默认 `gc_idle_ttl: "0s"` 保留已释放的持久化数据，周期 GC 仍会重试能够安全确认的残留清理。若设为正的 Go 时长（如 `"72h"`），从最后一次确认写层已释放时开始计算空闲时间；达到时长后，下一次符合条件的 GC 才会删除数据。在到期前，同一节点、同一存储根、同一命名空间和 Pod 名的替换实例仍可恢复原写层。GC 会保留没有生命周期标记的旧数据，直到后续成功使用并释放；对仍活跃、可能仍挂载或状态无法确认的记录和数据会保守跳过。共享存储不受支持。

StatefulSet 删除和缩到 0 的回收默认关闭。开启后，删除可在下一次符合条件的扫描回收已释放写层；缩到 0 则至少要等首次批准一分钟，并在后续扫描中按工作负载逐组复核。批准过期或写层已复用时，本次不会退休该写层。最终 API 检查和节点磁盘操作无法原子执行。空闲 TTL 是独立策略。手动部署即使关闭回收也需要元数据桥接来核验身份。将 `workload_socket` 设为桥接 socket 的宿主机绝对路径。socket 目录须由 root 所有，权限设为 `0700`，路径中不能有符号链接。桥接和运行时须使用相同的有效 UID，客户端会在使用元数据前检查 socket 所有者和服务端 UID。桥接的 ServiceAccount 只需要 Pod 和 StatefulSet 的 `get` 权限。[K3s Helm 安装器](k3s/README-cn.md)会自动部署桥接。

节点重启后，若写层最近一次启动已确认，且无日志或挂载引用，GC 可以恢复释放记录。仅丢失运行时日志不足以认定写层已释放。最近一次启动未确认时，只有从未确认使用、已记录释放且 `upper` 为空的写层才可按 TTL 回收。已有数据或来源不明的写层会保留；中断的删除可用写层目录之外的凭据继续清理。

同一个 API 桥也支持 Pod 模板注解覆盖，节点设置 `gc_workload_annotations: true` 即可启用；[注解示例](k3s/README-cn.md#按工作负载覆盖回收策略)与 Helm 部署相同。

在每个手动部署的节点上，用下列 systemd 单元定期执行 `gc`。服务须以 root 运行，并与运行时使用相同的配置、环境覆盖和挂载命名空间。示例适用于使用默认配置、未隔离挂载命名空间的 containerd；如果 runtime shim 使用独立的挂载命名空间，请在其所在命名空间运行 GC。

`/etc/systemd/system/permabox-gc.service`：

```ini
[Unit]
Description=PermaBox garbage collection

[Service]
Type=oneshot
Environment=PERMABOX_CONFIG=/etc/permabox/config.yaml
ExecStart=/usr/local/bin/permabox gc
```

`/etc/systemd/system/permabox-gc.timer`：

```ini
[Unit]
Description=Run PermaBox garbage collection every minute

[Timer]
OnActiveSec=60s
OnUnitInactiveSec=60s
AccuracySec=1s

[Install]
WantedBy=timers.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now permabox-gc.timer
systemctl list-timers permabox-gc.timer
sudo journalctl -u permabox-gc.service
```

卸载 PermaBox 前运行 `sudo systemctl disable --now permabox-gc.timer`，再运行 `sudo systemctl stop permabox-gc.service`。

## 从异常创建恢复

PermaBox 将 `create` 交给底层运行时后，无法在同一进程里捕获后续创建失败。若 containerd 未再调用 `delete`，且周期 GC 无法确认可以安全清理，旧记录可能阻止替换实例挂载该写层。维护时先停止重试，核实旧容器已经停止，然后从 `/run/permabox/<container-id>.json` 确认记录与故障实例对应。使用原来的底层运行时配置、运行时状态根目录和挂载命名空间重试清理：

```sh
sudo /usr/local/bin/permabox --root "$RUNTIME_STATE_ROOT" delete --force "$OLD_CONTAINER_ID"
```

先将变量设置为该 shim 实际使用的值。清理成功后，确认记录和临时 lower 挂载均已移除，再恢复工作负载。跨挂载命名空间的清理会拒绝执行；应从原命名空间检查，或在节点维护期间核实原命名空间与相关挂载已经释放。不要通过手动删除状态文件绕过仍在使用的写层。

## 安全范围与镜像升级

- 包装器在宿主机执行挂载，须由节点管理员安装和管理。示例容器为非特权容器；为了在 `/` 和 `/etc` 进行验证，示例以 UID 0 运行，因此不满足要求非 root 的 Restricted Pod Security 策略。只在授权的测试命名空间使用，不要放宽整个集群的准入策略。
- 仅授权受信任的工作负载使用该 RuntimeClass 和持久化注解，可用准入策略限制其使用范围。启用 Node authorizer 与 NodeRestriction 时，示例的标签键可防止 kubelet 自行给节点设置该标签。PermaBox 在复用写层前核验 StatefulSet 身份；仍应限制节点数据访问，并备份和归档。根文件系统之外的卷、Secret 投影和 `/proc` 等挂载由各自的机制管理。
- 生产环境使用镜像摘要固定 lower。OverlayFS 读取时优先使用 upper：被修改的文件通常经过 copy-up，删除镜像文件会产生 whiteout，部分目录还可能带有 opaque 标记。重用旧 upper 可能覆盖或隐藏新版镜像的文件，甚至掩盖安全修复；不能把换镜像当作自动合并。
- 不要直接修改仍在挂载中的 lower 或 upper。升级前停止实例并确认卸载，记录旧镜像摘要并备份可写层；用新 Pod 身份或新的存储位置建立干净的 upper，只迁移确认需要的应用数据，验证后再切换。备份整个层时须保留属主、权限、扩展属性和 whiteout 等特殊条目。回滚时保留旧镜像与对应的 upper 成对恢复。
- containerd 准备的根文件系统可能本身由 OverlayFS 快照器提供，PermaBox 会在其上再叠一层。嵌套挂载是否可用取决于内核、快照器及文件系统组合；应在目标节点实测上面的完整流程。遇到挂载失败时先检查内核日志、层数与文件系统限制，再评估是否改用经验证的非 OverlayFS 快照器。节点本地存储不提供跨节点故障转移。

上游参考：[containerd 1.7 CRI 配置](https://github.com/containerd/containerd/blob/release/1.7/docs/cri/config.md)、[containerd 2.0 CRI 配置](https://github.com/containerd/containerd/blob/release/2.0/docs/cri/config.md)、[Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)、[StatefulSet](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)、[节点标签与隔离](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)、[Linux OverlayFS](https://docs.kernel.org/filesystems/overlayfs.html)。
