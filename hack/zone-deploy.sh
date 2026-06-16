#!/usr/bin/env bash
set -euo pipefail

# zone-deploy.sh — install/upgrade aileron in a deployment zone from the
# versioned OCI Helm chart published by the release workflow.
#
# This replaces the old push-based deploy (CI connecting into each zone over
# Tailscale). Run it inside the zone (manually or from a cron) against the
# zone's own kubeconfig. CI never reaches into the cluster.
#
# Prereqs (once per zone):
#   helm registry login ghcr.io -u <user> -p <ghcr-read-PAT>
#   # ...unless ghcr.io/ruddervirt/charts/aileron is public.
#
# Usage:
#   VERSION=1.2.3 hack/zone-deploy.sh [zone-values.yaml]
#   VERSION=1.2.3 RELEASE=aileron NAMESPACE=ruddervirt-system hack/zone-deploy.sh zone-values.yaml
#
# Env:
#   VERSION    (required) chart version to install, e.g. 1.2.3 (no leading "v")
#   RELEASE    Helm release name        (default: aileron)
#   NAMESPACE  target namespace         (default: ruddervirt-system)
#   CHART_REPO OCI chart repo           (default: oci://ghcr.io/ruddervirt/charts/aileron)
#
# The first positional arg (optional) is a zone-specific values file with any
# overrides (resources, aileronUI.service.nodePort, egressExternal.cidr, ...).
# The chart's bundled values.yaml covers the common case; image tags are pinned
# by the chart version, so no --set image.* is needed.

VERSION="${VERSION:?Set VERSION to the chart version to install, e.g. VERSION=1.2.3}"
RELEASE="${RELEASE:-aileron}"
NAMESPACE="${NAMESPACE:-ruddervirt-system}"
CHART_REPO="${CHART_REPO:-oci://ghcr.io/ruddervirt/charts/aileron}"
VALUES_FILE="${1:-}"

ARGS=(
    upgrade --install "$RELEASE" "$CHART_REPO"
    --version "$VERSION"
    --namespace "$NAMESPACE" --create-namespace
    --take-ownership
    --wait --timeout=120s
)
if [[ -n "$VALUES_FILE" ]]; then
    ARGS+=(-f "$VALUES_FILE")
fi

echo "Deploying $RELEASE from ${CHART_REPO} version ${VERSION} into namespace ${NAMESPACE}..."
helm "${ARGS[@]}"
