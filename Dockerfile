# Build the manager binary
FROM golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS builder
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
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6 AS manager
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/data/*.fd /data/
USER 65532:65532
ENTRYPOINT ["/manager"]

# Grader worker — aileron core (runs in per-VM grading Jobs scheduled by the
# GradeRequest reconciler; connects to the VM's KubeVirt serial console).
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6 AS grader
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/grader /grader
USER 65532:65532
ENTRYPOINT ["/grader"]

# VNC bridge — aileron core (TCP <-> KubeVirt VNC WebSocket tunnels for guacd)
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6 AS vncgateway
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/vncgateway /vncgateway
USER 65532:65532
ENTRYPOINT ["/vncgateway"]

# aileron-ui — basic web interface (builds/clones submission, status, consoles).
# Static frontend assets are embedded in the binary via go:embed.
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6 AS aileron-ui
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/aileron-ui /aileron-ui
USER 65532:65532
ENTRYPOINT ["/aileron-ui"]

# Coordinator (boot commands + provisioning)
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6 AS coordinator
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/coordinator /coordinator
USER 65532:65532
ENTRYPOINT ["/coordinator"]

# Egress bridge helper (replaces nicolaka/netshoot)
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS egress-bridge
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache iproute2 iptables
ENTRYPOINT ["/bin/sh"]

# Build helper (disk image creation, etc.)
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS helper
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache mtools dosfstools cdrkit
ENTRYPOINT ["/bin/sh"]

# KubeVirt sidecar hook (EFI firmware + floppy device injection)
FROM quay.io/kubevirt/sidecar-shim:v1.4.0@sha256:cb4025f7275f8de2891c2195c5faf128ac1b68595cdb3eff776a2e5360b8f034 AS sidecar
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/onDefineDomain /usr/bin/onDefineDomain
