# SPDX-License-Identifier: GPL-3.0-only

# Build the manager binary
FROM golang:1.26.10@sha256:cfa27959837144bf4649802ba38e59daffd0dd2a7994aa83daaf113285b0f551 AS builder
WORKDIR /workspace

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build — copy only what the aileron build needs so changes under unrelated
# dirs don't bust this stage's cache.
ARG TARGETOS=linux
ARG TARGETARCH
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
COPY data/ data/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o manager cmd/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o onDefineDomain cmd/sidecar/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o coordinator cmd/coordinator/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o vncgateway cmd/vncgateway/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o aileron-ui cmd/aileron-ui/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o grader cmd/grader/main.go

# Aileron runtime
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS manager
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/data/*.fd /data/
USER 65532:65532
ENTRYPOINT ["/manager"]

# Grader worker — aileron core (runs in per-VM grading Jobs scheduled by the
# GradeRequest reconciler; connects to the VM's KubeVirt serial console).
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS grader
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/grader /grader
USER 65532:65532
ENTRYPOINT ["/grader"]

# VNC bridge — aileron core (TCP <-> KubeVirt VNC WebSocket tunnels for guacd)
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS vncgateway
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/vncgateway /vncgateway
USER 65532:65532
ENTRYPOINT ["/vncgateway"]

# aileron-ui — basic web interface (builds/clones submission, status, consoles).
# Static frontend assets are embedded in the binary via go:embed.
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS aileron-ui
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/aileron-ui /aileron-ui
USER 65532:65532
ENTRYPOINT ["/aileron-ui"]

# Coordinator (boot commands + provisioning)
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS coordinator
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/coordinator /coordinator
USER 65532:65532
ENTRYPOINT ["/coordinator"]

# Egress bridge helper
FROM alpine:3.21@sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab AS egress-bridge
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache iproute2 iptables wireguard-tools
ENTRYPOINT ["/bin/sh"]

# Build helper (disk image creation, etc.)
FROM alpine:3.21@sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab AS helper
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache mtools dosfstools cdrkit
ENTRYPOINT ["/bin/sh"]

# KubeVirt sidecar hook (EFI firmware + floppy device injection)
FROM quay.io/kubevirt/sidecar-shim:v1.10.0@sha256:93d211a7de2a1e47a45da63ec8ea93ef8e84ae2fb6bf2de07f310e7093fa6891 AS sidecar
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/onDefineDomain /usr/bin/onDefineDomain
