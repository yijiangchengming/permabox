# 参与 PermaBox 开发

[English](CONTRIBUTING.md)

## 分支与 PR 流程

采用「工作分支 → `dev` → `main`」：`dev` 集成已审查的改动，`main` 保留可发布的提交。

1. 重要功能和兼容性变更先在 GitHub Issue 中讨论。小改动可直接开 PR。未公开的安全漏洞按 [SECURITY.md](SECURITY.md) 私下报告。
2. 从上游最新的 `dev` 创建短期分支，例如 `fix/k3s-cleanup`、`feat/runtime-alias` 或 `docs/deployment-guide`。没有写权限时，推送到自己的 Fork，再向上游发起 PR。
3. 一个分支和 PR 聚焦一项改动。提交信息用简短动词说明变更，避免夹带无关重构和生成产物。
4. 向 `dev` 开 PR，未完成时使用 Draft。说明问题、变更后的行为、验证结果，以及兼容性、升级或数据保留影响。仅在合并即可解决 Issue 时使用 `Fixes #123`。
5. 标记为可审查后，解决审查意见，取得至少一名独立审查者批准，并等待当前 PR 版本的必需 CI 检查通过。同步上游后重新检查差异和相关测试。不要强推共享分支；确需重写自己的 PR 分支时，使用 `--force-with-lease`。
6. 维护者通常使用 Squash and merge 合并，保留清晰的提交标题，并删除已合并的短期分支。回退通过新的 PR 完成，不改写 `dev` 历史。

直接克隆上游仓库时：

```sh
git fetch origin dev
git switch -c fix/k3s-cleanup origin/dev
# 完成修改、验证和提交。
git push -u origin fix/k3s-cleanup
```

使用 Fork 时，从 `upstream/dev` 创建分支，推送到自己的 `origin`。

`main` 只接受来自本仓库 `dev` 的 PR。审查和必需检查通过后，使用 **Create a merge commit** 保留 `dev` 的提交历史，并保留两个长期分支。需要将 `main` 独有的改动同步回 `dev` 时，从 `dev` 创建同步分支，将 `main` 合入同步分支，再通过 PR 以 merge commit 合回 `dev`。

维护者按 [GitHub 仓库配置清单](.github/maintainer-settings.md) 启用分支保护。

## 本地验证

使用 Linux、Go 1.23 或更新版本，以及 GNU Make、Bash、jq、ShellCheck 和 Helm。部署行为或示例变更时，同步更新中英文指南。

```sh
make check
make build
```

挂载生命周期有改动时，在支持 OverlayFS 且有权限创建挂载命名空间的 Linux 主机上运行 `make test-kernel`。安装器镜像有改动时，从仓库根目录构建并验证：

```sh
docker build -f deploy/k3s/Dockerfile -t permabox-installer:dev .
docker run --rm --entrypoint /usr/local/lib/permabox/permabox \
  permabox-installer:dev validate-config
```

PR 中记录实际运行的命令、结果和未执行的相关检查。不要上传 kubeconfig、节点数据、令牌或含凭据的日志。GitHub CI 会在单节点 K3s 集群中验证 Helm 安装、Pod 重建后的持久化、缩零或删除 StatefulSet 后的回收，以及运行时移除。

## 发布安装器镜像

1. 通过面向 `dev` 的 PR 准备发布。Chart 有改动时更新 [Chart.yaml](deploy/helm/permabox/Chart.yaml) 的 `version`，并将 `appVersion` 指向默认安装器镜像版本。再通过审查过的 PR 将 `dev` 合入 `main`。
2. 在 `main` 上已审查的提交创建并推送稳定标签，例如 `v0.1.0`。已发布的标签不可移动。
3. 确认 **Publish installer image** 工作流通过。CI 成功后，工作流把 `linux/amd64` 和 `linux/arm64` 镜像发布到 `ghcr.io/<owner>/<repo>-installer`，例如生成 `0.1.0` 和 `sha-<完整提交 SHA>` 标签，不发布 `latest`。
4. 检查镜像包可见性并在目标环境拉取验证。公共部署需要显式公开镜像包；私有镜像需要集群配置仓库凭据。发布说明应列出镜像标签、摘要和升级步骤。

安装命令见 [K3s 部署指南](deploy/k3s/README-cn.md)。
