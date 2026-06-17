# Build the manager binary
FROM golang:1.26.2 AS builder
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
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o manager cmd/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o onDefineDomain cmd/sidecar/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o coordinator cmd/coordinator/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o vncbridge cmd/vncbridge/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o aileron-ui cmd/aileron-ui/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o grader cmd/grader/main.go

# Aileron runtime
FROM gcr.io/distroless/static:nonroot AS manager
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/data/*.fd /data/
USER 65532:65532
ENTRYPOINT ["/manager"]

# Grader worker — aileron core (runs in per-VM grading Jobs scheduled by the
# GradeRequest reconciler; connects to the VM's KubeVirt serial console).
FROM gcr.io/distroless/static:nonroot AS grader
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/grader /grader
USER 65532:65532
ENTRYPOINT ["/grader"]

# VNC bridge — aileron core (TCP <-> KubeVirt VNC WebSocket tunnels for guacd)
FROM gcr.io/distroless/static:nonroot AS vncbridge
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/vncbridge /vncbridge
USER 65532:65532
ENTRYPOINT ["/vncbridge"]

# aileron-ui — basic web interface (builds/clones submission, status, consoles).
# Static frontend assets are embedded in the binary via go:embed.
FROM gcr.io/distroless/static:nonroot AS aileron-ui
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/aileron-ui /aileron-ui
USER 65532:65532
ENTRYPOINT ["/aileron-ui"]

# VNC gateway — aileron core (guacamole-lite front for guacd; session sharing).
# Authentication is NOT here: it lives in an external authenticated VNC proxy.
FROM node:22-alpine AS vncgateway
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
WORKDIR /app
COPY vncgateway/package.json vncgateway/package-lock.json ./
RUN npm ci
COPY vncgateway/ ./
RUN npm test && npm prune --omit=dev
USER node
ENTRYPOINT ["node", "server.js"]

# Coordinator (boot commands + provisioning)
FROM gcr.io/distroless/static:nonroot AS coordinator
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/coordinator /coordinator
USER 65532:65532
ENTRYPOINT ["/coordinator"]

# Egress bridge helper (replaces nicolaka/netshoot)
FROM alpine:3.21 AS egress-bridge
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache iproute2 iptables
ENTRYPOINT ["/bin/sh"]

# Build helper (disk image creation, etc.)
FROM alpine:3.21 AS helper
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
RUN apk add --no-cache mtools dosfstools cdrkit
ENTRYPOINT ["/bin/sh"]

# KubeVirt sidecar hook (EFI firmware + floppy device injection)
FROM quay.io/kubevirt/sidecar-shim:v1.4.0 AS sidecar
LABEL org.opencontainers.image.source="https://github.com/ruddervirt/aileron"
COPY --from=builder /workspace/onDefineDomain /usr/bin/onDefineDomain
