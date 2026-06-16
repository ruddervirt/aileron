#!/usr/bin/env bash
set -euo pipefail

# watch-build.sh — Watch a VirtualMachineBuild until it succeeds or fails.
#
# Accepts either a VirtualMachineBuild name or a virtualMachineNamespace
# (vm-*, ns-*) and resolves the build from it.
#
# Usage:
#   watch-build.sh <build-name|vm-namespace>  [namespace]
#   watch-build.sh test-base-linux
#   watch-build.sh vm-a3f8b2c1

IDENTIFIER="${1:?Usage: watch-build.sh <build-name|vm-namespace> [namespace]}"
NS="${2:-ruddervirt-system}"
POLL_INTERVAL=3

# --- Resolve build name ----------------------------------------------------
resolve_build() {
    # Try direct name first
    if kubectl get virtualmachinebuilds.ruddervirt.io "$IDENTIFIER" -n "$NS" &>/dev/null; then
        echo "$IDENTIFIER"
        return
    fi

    # Try matching by status.virtualMachineNamespace
    local match
    match=$(kubectl get virtualmachinebuilds.ruddervirt.io -n "$NS" -o json | \
        jq -r --arg vmns "$IDENTIFIER" \
        '.items[] | select(.status.virtualMachineNamespace == $vmns) | .metadata.name' | head -1)

    if [[ -n "$match" ]]; then
        echo "$match"
        return
    fi

    echo ""
}

echo "Resolving ${IDENTIFIER}..."
BUILD_NAME=$(resolve_build)
if [[ -z "$BUILD_NAME" ]]; then
    echo "ERROR: Could not find a VirtualMachineBuild matching '${IDENTIFIER}' in namespace '${NS}'" >&2
    exit 1
fi

if [[ "$BUILD_NAME" != "$IDENTIFIER" ]]; then
    echo "Resolved to build: ${BUILD_NAME}"
fi

# --- Cleanup ---------------------------------------------------------------
LOGS_PID=""
cleanup() {
    [[ -n "$LOGS_PID" ]] && kill "$LOGS_PID" 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

# --- Stream controller logs in background ----------------------------------
echo "--- aileron controller logs ---"
kubectl logs -l control-plane=controller-manager -n "$NS" -c manager \
    --tail=1 -f 2>/dev/null | \
    sed -u 's/^/[log] /' &
LOGS_PID=$!

echo ""
echo "Watching build: ${BUILD_NAME} in ${NS}"
echo ""

# --- Poll loop -------------------------------------------------------------
PREV_STATUS=""

while true; do
    # Fetch build JSON
    JSON=$(kubectl get virtualmachinebuilds.ruddervirt.io "$BUILD_NAME" -n "$NS" -o json 2>&1) || {
        echo -e "${RED}Failed to get build: ${JSON}${NC}"
        sleep "$POLL_INTERVAL"
        continue
    }

    # Parse status fields
    PHASE=$(echo "$JSON" | jq -r '.status.phase // "Pending"')
    MESSAGE=$(echo "$JSON" | jq -r '.status.message // ""')
    BUILD_ID=$(echo "$JSON" | jq -r '.status.buildID // ""')
    VM_NS=$(echo "$JSON" | jq -r '.status.virtualMachineNamespace // ""')

    # Per-VM status
    VM_LINES=$(echo "$JSON" | jq -r '
        (.status.vmStatuses // []) | .[] |
        "  \(.name): \(.phase // "Pending")\(if .message != "" and .message != null then " — \(.message)" else "" end)"
    ')

    # Provisioner status
    PROV_LINES=$(echo "$JSON" | jq -r '
        (.status.vmStatuses // []) | .[] as $vm |
        ($vm.provisionerResults // []) | .[] |
        "    [\(.status // "?")] \(if .name != "" and .name != null then .name else "step-\(.index)" end)\(if .duration != null then " (\(.duration))" else "" end)\(if .message != "" and .message != null then " — \(.message)" else "" end)"
    ')

    # Change detection — only print when something changes
    CURR_STATUS="${PHASE}|${MESSAGE}|${VM_LINES}|${PROV_LINES}"

    if [[ "$CURR_STATUS" != "$PREV_STATUS" ]]; then
        PREV_STATUS="$CURR_STATUS"

        echo "[$(date +%H:%M:%S)] Phase: ${PHASE}"
        [[ -n "$BUILD_ID" ]]  && echo "  build-id:  ${BUILD_ID}"
        [[ -n "$VM_NS" ]]     && echo "  namespace: ${VM_NS}"
        [[ -n "$MESSAGE" ]]   && echo "  message:   ${MESSAGE}"

        if [[ -n "$VM_LINES" ]]; then
            echo "  VMs:"
            echo "$VM_LINES"
        fi

        if [[ -n "$PROV_LINES" ]]; then
            echo "  Provisioners:"
            echo "$PROV_LINES"
        fi
        echo ""
    fi

    # Exit on terminal phases
    if [[ "$PHASE" == "Succeeded" ]]; then
        COMPLETION=$(echo "$JSON" | jq -r '.status.completionTime // ""')
        START=$(echo "$JSON" | jq -r '.status.startTime // ""')
        if [[ -n "$START" && -n "$COMPLETION" ]]; then
            START_EPOCH=$(date -d "$START" +%s 2>/dev/null || echo "")
            END_EPOCH=$(date -d "$COMPLETION" +%s 2>/dev/null || echo "")
            if [[ -n "$START_EPOCH" && -n "$END_EPOCH" ]]; then
                ELAPSED=$(( END_EPOCH - START_EPOCH ))
                MINS=$(( ELAPSED / 60 ))
                SECS=$(( ELAPSED % 60 ))
                echo "BUILD SUCCEEDED in ${MINS}m${SECS}s"
            else
                echo "BUILD SUCCEEDED"
            fi
        else
            echo "BUILD SUCCEEDED"
        fi
        exit 0
    elif [[ "$PHASE" == "Failed" ]]; then
        echo "BUILD FAILED"
        [[ -n "$MESSAGE" ]] && echo "  ${MESSAGE}"
        exit 1
    fi

    sleep "$POLL_INTERVAL"
done
