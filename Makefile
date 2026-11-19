GO ?= go
GOFMT ?= gofmt
HELM ?= helm

.PHONY: build check fmt-check test vet lint-installer lint-chart test-kernel

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/permabox ./cmd/permabox

check: fmt-check test vet lint-installer lint-chart

fmt-check:
	@files=$$($(GOFMT) -l cmd internal) || exit; \
		test -z "$$files" || { printf 'Run gofmt on:\n%s\n' "$$files"; exit 1; }

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint-installer:
	shellcheck -x deploy/k3s/installer.sh deploy/k3s/installer_test.sh
	bash deploy/k3s/installer_test.sh

lint-chart:
	$(HELM) lint --strict deploy/helm/permabox --set-string image.repository=example.invalid/permabox-installer --set-string 'nodeSelector.kubernetes\.io/hostname=test-node'
	$(HELM) lint --strict deploy/helm/permabox --set-string image.repository=example.invalid/permabox-installer --set-string 'nodeSelector.kubernetes\.io/hostname=test-node' --set gcWorkloadAnnotations=true

test-kernel:
	@go_bin=$$(command -v $(GO)) || exit; \
		if [ "$$(id -u)" -ne 0 ]; then set -- sudo; fi; \
		"$$@" unshare --mount --propagation private env PERMABOX_KERNEL_TEST=1 \
		"$$go_bin" test -count=1 -v ./internal/mount -run '^TestKernelPersistence$$'
