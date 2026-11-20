# Contributing to PermaBox

[简体中文](CONTRIBUTING-cn.md)

## GitHub Flow

1. Discuss substantial changes and compatibility decisions in a GitHub issue before implementation. Small fixes can start with a pull request. Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).
2. Create a short-lived branch from the current upstream `main`. Use a descriptive name such as `fix/k3s-cleanup`, `feat/runtime-alias`, or `docs/deployment-guide`. Contributors without write access should push to a fork and target the upstream repository.
3. Keep each branch and PR focused on one outcome. Commit coherent changes with short, imperative subjects. Keep unrelated refactors and generated artifacts out of the diff.
4. Open a PR targeting `main`. Use a draft while the work is in progress. Describe the problem, the resulting behavior, and what you ran to verify it. Use `Fixes #123` only when merging the PR resolves the issue. Call out changes to compatibility, upgrades, or data retention.
5. Mark the PR ready when it can be reviewed. Resolve review threads, obtain an independent approval, and wait for the required CI check on the current PR revision. After incorporating newer `main` changes, review the resulting diff and rerun relevant checks. Never force-push a shared branch; when rewriting your own PR branch, use `--force-with-lease`.
6. A maintainer squash-merges the approved PR using a descriptive title, then deletes the merged branch. Revert a bad merge with a new PR instead of rewriting `main`.

For a repository clone with the upstream repository configured as `origin`:

```sh
git fetch origin main
git switch -c fix/k3s-cleanup origin/main
# Make, verify, and commit the change.
git push -u origin fix/k3s-cleanup
```

For a fork, use `upstream/main` as the starting point and push to your fork's `origin`.

Maintainers can use the [GitHub repository settings checklist](.github/maintainer-settings.md) to enforce the merge policy.

## Verify changes

Use Linux with Go 1.23 or newer, GNU Make, Bash, jq, ShellCheck, and Helm. Keep deployment behavior and examples in sync across the English and Chinese guides.

```sh
make check
make build
```

For mount lifecycle changes, also run `make test-kernel` on a host with OverlayFS and `sudo` access. For image changes, build the installer from the repository root:

```sh
docker build -f deploy/k3s/Dockerfile -t permabox-installer:dev .
docker run --rm --entrypoint /usr/local/lib/permabox/permabox \
  permabox-installer:dev validate-config
```

In a pull request, describe what changed, how you verified it, and any change in how nodes, writable layers, or existing Helm installations are handled. Do not include kubeconfigs, node data, tokens, or logs containing credentials.

GitHub Actions imports the installer image built from the current checkout into a single-node K3s cluster. The smoke test covers Helm installation, persistence after StatefulSet Pod replacement, data reclamation after scaling to zero or deleting the StatefulSet, and runtime removal. Image releases run the same CI checks before publishing.

## Release an installer image

1. Submit release preparation through a PR to `main`. Update `version` in [Chart.yaml](deploy/helm/permabox/Chart.yaml) for chart changes and set `appVersion` to the installer image version the chart should default to. Tag the resulting reviewed commit on `main`.
2. Push a new stable tag such as `v0.1.0`. Treat released tags as immutable.
3. Verify that the **Publish installer image** workflow succeeds. It first runs CI, then publishes `linux/amd64` and `linux/arm64` to `ghcr.io/<owner>/<repo>-installer` (lowercase), with `0.1.0` and `sha-<full-commit-sha>` tags for this example. No `latest` tag is published. The workflow uses the repository's `GITHUB_TOKEN` and does not require a registry password secret.
4. Check the package's visibility and verify a pull from the target environment. For a public deployment, make the package public explicitly; otherwise use registry credentials on the cluster. Publish release notes that identify the image tag, digest, and any upgrade steps.

For installation commands, see the [K3s guide](deploy/k3s/README.md).
