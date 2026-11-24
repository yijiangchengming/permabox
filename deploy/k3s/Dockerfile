# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
ENV GOTOOLCHAIN=local
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -o /permabox ./cmd/permabox

FROM scratch AS installer-binary
COPY --from=build /permabox /permabox

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends jq util-linux \
    && rm -rf /var/lib/apt/lists/*
COPY --from=installer-binary /permabox /usr/local/lib/permabox/permabox
COPY --chmod=0755 deploy/k3s/installer.sh /usr/local/bin/permabox-k3s
ENTRYPOINT ["/usr/local/bin/permabox-k3s"]
