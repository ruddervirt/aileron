# Build the manager binary
FROM golang:1.26.2@sha256:b54cbf583d390341599d7bcbc062425c081105cc5ef6d170ced98ef9d047c716 AS builder
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
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240 AS manager
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/data/*.fd /data/
USER 65532:65532
ENTRYPOINT ["/manager"]

# Grader worker — aileron core (runs in per-VM grading Jobs scheduled by the
# GradeRequest reconciler; connects to the VM's KubeVirt serial console).
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240 AS grader
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/grader /grader
USER 65532:65532
ENTRYPOINT ["/grader"]

# VNC bridge — aileron core (TCP <-> KubeVirt VNC WebSocket tunnels for guacd)
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240 AS vncgateway
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/vncgateway /vncgateway
USER 65532:65532
ENTRYPOINT ["/vncgateway"]

# aileron-ui — basic web interface (builds/clones submission, status, consoles).
# Static frontend assets are embedded in the binary via go:embed.
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240 AS aileron-ui
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/aileron-ui /aileron-ui
USER 65532:65532
ENTRYPOINT ["/aileron-ui"]

# Coordinator (boot commands + provisioning)
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240 AS coordinator
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/coordinator /coordinator
USER 65532:65532
ENTRYPOINT ["/coordinator"]

# Egress bridge helper (replaces nicolaka/netshoot)
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS egress-bridge
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache iproute2 iptables
ENTRYPOINT ["/bin/sh"]

# Build helper (disk image creation, etc.)
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS helper
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache mtools dosfstools cdrkit
ENTRYPOINT ["/bin/sh"]

# KubeVirt sidecar hook (EFI firmware + floppy device injection)
FROM quay.io/kubevirt/sidecar-shim:v1.4.0@sha256:cb4025f7275f8de2891c2195c5faf128ac1b68595cdb3eff776a2e5360b8f034 AS sidecar
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/onDefineDomain /usr/bin/onDefineDomain
