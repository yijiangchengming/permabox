# Contributing to PermaBox

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

## Release an installer image

1. Review the release commit on `main`. Update `version` in [Chart.yaml](deploy/helm/permabox/Chart.yaml) for chart changes and set `appVersion` to the installer image version the chart should default to.
2. Push a new stable tag such as `v0.1.0`. Treat released tags as immutable.
3. Verify that the **Publish installer image** workflow succeeds. It first runs CI, then publishes `linux/amd64` and `linux/arm64` to `ghcr.io/<owner>/<repo>-installer` (lowercase), with `0.1.0` and `sha-<full-commit-sha>` tags for this example. No `latest` tag is published. The workflow uses the repository's `GITHUB_TOKEN` and does not require a registry password secret.
4. Check the package's visibility and verify a pull from the target environment. For a public deployment, make the package public explicitly; otherwise use registry credentials on the cluster. Publish release notes that identify the image tag, digest, and any upgrade steps.

For installation commands, see the [K3s guide](deploy/k3s/README.md).
