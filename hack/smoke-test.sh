#!/usr/bin/env bash
set -euo pipefail

# smoke-test.sh — Run integration builds against a live cluster and report results.
#
# Applies each test YAML, waits for completion, then cleans up.
# Builds run sequentially by default; use -p for parallel.
# Layered builds and clones run after their dependencies succeed.
#
# Usage:
#   hack/smoke-test.sh                     # run all tests sequentially
#   hack/smoke-test.sh -p                  # run independent tests in parallel
#   hack/smoke-test.sh simple-build efi    # run only matching tests
#   hack/smoke-test.sh --skip windows11    # skip slow tests
#   hack/smoke-test.sh --timeout 20m       # override per-build wait timeout

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="$ROOT_DIR/test/integration"
WATCH_SCRIPT="$SCRIPT_DIR/watch-build.sh"

NS="${NAMESPACE:-ruddervirt-system}"
CONTEXT="${KUBE_CONTEXT:-direct}"
KUBECTL="kubectl --context=$CONTEXT"
TIMEOUT="${SMOKE_TIMEOUT:-30m}"
PARALLEL=false
SKIP_PATTERNS=()
FILTER_PATTERNS=()
CLEANUP=true

# --- Argument parsing --------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        -p|--parallel)   PARALLEL=true; shift ;;
        --skip)          SKIP_PATTERNS+=("$2"); shift 2 ;;
        --timeout)       TIMEOUT="$2"; shift 2 ;;
        --no-cleanup)    CLEANUP=false; shift ;;
        -h|--help)
            echo "Usage: $0 [-p] [--skip PATTERN] [--timeout DURATION] [--no-cleanup] [FILTER...]"
            echo ""
            echo "Options:"
            echo "  -p, --parallel    Run independent builds in parallel"
            echo "  --skip PATTERN    Skip tests matching PATTERN (repeatable)"
            echo "  --timeout DUR     Per-build wait timeout (default: 30m)"
            echo "  --no-cleanup      Don't delete builds after completion"
            echo "  FILTER            Only run tests whose filename contains FILTER"
            exit 0
            ;;
        *)               FILTER_PATTERNS+=("$1"); shift ;;
    esac
done

# --- Colors ------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# --- Test discovery ----------------------------------------------------------

# Phase 1: standalone builds (no buildRef dependency)
STANDALONE_BUILDS=()
# Phase 2: layered builds (depend on a phase-1 build)
LAYERED_BUILDS=()
# Phase 3: clones (depend on a succeeded build template)
CLONE_TESTS=()

for f in "$TEST_DIR"/*.yaml "$TEST_DIR"/*.yml; do
    [[ -f "$f" ]] || continue
    basename="$(basename "$f")"

    # Apply filters
    if [[ ${#FILTER_PATTERNS[@]} -gt 0 ]]; then
        match=false
        for pat in "${FILTER_PATTERNS[@]}"; do
            if [[ "$basename" == *"$pat"* ]]; then match=true; break; fi
        done
        $match || continue
    fi

    # Apply skips
    skip=false
    for pat in "${SKIP_PATTERNS[@]}"; do
        if [[ "$basename" == *"$pat"* ]]; then skip=true; break; fi
    done
    $skip && continue

    kind=$(grep -m1 "^kind:" "$f" | awk '{print $2}')

    if [[ "$kind" == "VirtualMachineClone" ]]; then
        CLONE_TESTS+=("$f")
    elif grep -q "buildRef:" "$f"; then
        LAYERED_BUILDS+=("$f")
    elif [[ "$kind" == "VirtualMachineBuild" ]]; then
        STANDALONE_BUILDS+=("$f")
    fi
done

TOTAL=$(( ${#STANDALONE_BUILDS[@]} + ${#LAYERED_BUILDS[@]} + ${#CLONE_TESTS[@]} ))
echo -e "${BOLD}Aileron Smoke Test${NC}"
echo -e "  Standalone builds: ${#STANDALONE_BUILDS[@]}"
echo -e "  Layered builds:    ${#LAYERED_BUILDS[@]}"
echo -e "  Clone tests:       ${#CLONE_TESTS[@]}"
echo -e "  Total:             ${TOTAL}"
echo -e "  Namespace:         ${NS}"
echo -e "  Timeout:           ${TIMEOUT}"
echo ""

if [[ $TOTAL -eq 0 ]]; then
    echo "No tests to run."
    exit 0
fi

# --- Helpers -----------------------------------------------------------------
PASSED=0
FAILED=0
SKIPPED=0
RESULTS=()

apply_and_get_name() {
    local file="$1"
    # Apply and capture the created resource name (handles generateName)
    local output
    output=$($KUBECTL apply -f "$file" -n "$NS" 2>&1)
    # Extract "resourcetype/name" from kubectl output
    echo "$output" | grep -oP '(virtualmachinebuild|virtualmachineclone)[./]\K[^ ]+' | head -1
}

wait_for_build() {
    local name="$1"
    local label="$2"

    echo -e "  ${CYAN}Waiting for ${name}...${NC}"
    local start_epoch
    start_epoch=$(date +%s)

    # Convert timeout to seconds
    local timeout_secs
    timeout_secs=$(echo "$TIMEOUT" | sed -E 's/([0-9]+)h/\1*3600+/g; s/([0-9]+)m/\1*60+/g; s/([0-9]+)s/\1+/g; s/\+$//' | bc)

    while true; do
        local now
        now=$(date +%s)
        local elapsed=$(( now - start_epoch ))

        if [[ $elapsed -ge $timeout_secs ]]; then
            echo -e "  ${RED}TIMEOUT after ${TIMEOUT}${NC}"
            RESULTS+=("TIMEOUT  $label")
            FAILED=$(( FAILED + 1 ))
            return 1
        fi

        local phase
        phase=$($KUBECTL get virtualmachinebuilds.ruddervirt.io "$name" -n "$NS" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")

        if [[ "$phase" == "Succeeded" ]]; then
            local mins=$(( elapsed / 60 ))
            local secs=$(( elapsed % 60 ))
            echo -e "  ${GREEN}PASSED${NC} (${mins}m${secs}s)"
            RESULTS+=("PASSED   $label  (${mins}m${secs}s)")
            PASSED=$(( PASSED + 1 ))
            return 0
        elif [[ "$phase" == "Failed" ]]; then
            local msg
            msg=$($KUBECTL get virtualmachinebuilds.ruddervirt.io "$name" -n "$NS" \
                -o jsonpath='{.status.message}' 2>/dev/null || echo "unknown")
            echo -e "  ${RED}FAILED${NC}: $msg"
            RESULTS+=("FAILED   $label  $msg")
            FAILED=$(( FAILED + 1 ))
            return 1
        fi

        sleep 10
    done
}

wait_for_clone() {
    local name="$1"
    local label="$2"

    echo -e "  ${CYAN}Waiting for clone ${name}...${NC}"
    local start_epoch
    start_epoch=$(date +%s)
    local timeout_secs
    timeout_secs=$(echo "$TIMEOUT" | sed -E 's/([0-9]+)h/\1*3600+/g; s/([0-9]+)m/\1*60+/g; s/([0-9]+)s/\1+/g; s/\+$//' | bc)

    while true; do
        local now
        now=$(date +%s)
        local elapsed=$(( now - start_epoch ))

        if [[ $elapsed -ge $timeout_secs ]]; then
            echo -e "  ${RED}TIMEOUT after ${TIMEOUT}${NC}"
            RESULTS+=("TIMEOUT  $label")
            FAILED=$(( FAILED + 1 ))
            return 1
        fi

        local phase
        phase=$($KUBECTL get virtualmachineclones.ruddervirt.io "$name" -n "$NS" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "")

        if [[ "$phase" == "Ready" || "$phase" == "Succeeded" ]]; then
            local mins=$(( elapsed / 60 ))
            local secs=$(( elapsed % 60 ))
            echo -e "  ${GREEN}PASSED${NC} (${mins}m${secs}s)"
            RESULTS+=("PASSED   $label  (${mins}m${secs}s)")
            PASSED=$(( PASSED + 1 ))
            return 0
        elif [[ "$phase" == "Failed" ]]; then
            local msg
            msg=$($KUBECTL get virtualmachineclones.ruddervirt.io "$name" -n "$NS" \
                -o jsonpath='{.status.message}' 2>/dev/null || echo "unknown")
            echo -e "  ${RED}FAILED${NC}: $msg"
            RESULTS+=("FAILED   $label  $msg")
            FAILED=$(( FAILED + 1 ))
            return 1
        fi

        sleep 10
    done
}

cleanup_resource() {
    local kind="$1"
    local name="$2"
    $CLEANUP && $KUBECTL delete "$kind" "$name" -n "$NS" --ignore-not-found &>/dev/null &
}

run_build() {
    local file="$1"
    local label
    label="$(basename "$file")"

    echo -e "${BOLD}[$label]${NC}"

    local name
    name=$(apply_and_get_name "$file")
    if [[ -z "$name" ]]; then
        echo -e "  ${RED}FAILED to apply${NC}"
        RESULTS+=("FAILED   $label  (apply error)")
        FAILED=$(( FAILED + 1 ))
        return 1
    fi
    echo -e "  Applied: ${name}"

    wait_for_build "$name" "$label"
    local rc=$?

    cleanup_resource "virtualmachinebuilds.ruddervirt.io" "$name"
    return $rc
}

run_clone() {
    local file="$1"
    local label
    label="$(basename "$file")"

    echo -e "${BOLD}[$label]${NC}"

    local name
    name=$($KUBECTL apply -f "$file" -n "$NS" 2>&1 | grep -oP '(virtualmachineclone)[./]\K[^ ]+' | head -1)
    if [[ -z "$name" ]]; then
        echo -e "  ${RED}FAILED to apply${NC}"
        RESULTS+=("FAILED   $label  (apply error)")
        FAILED=$(( FAILED + 1 ))
        return 1
    fi
    echo -e "  Applied: ${name}"

    wait_for_clone "$name" "$label"
    local rc=$?

    cleanup_resource "virtualmachineclones.ruddervirt.io" "$name"
    return $rc
}

# --- Phase 1: Standalone builds ----------------------------------------------
if [[ ${#STANDALONE_BUILDS[@]} -gt 0 ]]; then
    echo -e "${BOLD}=== Phase 1: Standalone Builds ===${NC}"
    echo ""

    if $PARALLEL; then
        PIDS=()
        for f in "${STANDALONE_BUILDS[@]}"; do
            run_build "$f" &
            PIDS+=($!)
        done
        for pid in "${PIDS[@]}"; do
            wait "$pid" || true
        done
    else
        for f in "${STANDALONE_BUILDS[@]}"; do
            run_build "$f" || true
            echo ""
        done
    fi
fi

# --- Phase 2: Layered builds ------------------------------------------------
if [[ ${#LAYERED_BUILDS[@]} -gt 0 ]]; then
    echo -e "${BOLD}=== Phase 2: Layered Builds ===${NC}"
    echo ""

    for f in "${LAYERED_BUILDS[@]}"; do
        run_build "$f" || true
        echo ""
    done
fi

# --- Phase 3: Clones --------------------------------------------------------
if [[ ${#CLONE_TESTS[@]} -gt 0 ]]; then
    echo -e "${BOLD}=== Phase 3: Clone Tests ===${NC}"
    echo ""

    for f in "${CLONE_TESTS[@]}"; do
        run_clone "$f" || true
        echo ""
    done
fi

# --- Summary -----------------------------------------------------------------
echo ""
echo -e "${BOLD}=== Results ===${NC}"
echo ""
for r in "${RESULTS[@]}"; do
    case "$r" in
        PASSED*)  echo -e "  ${GREEN}$r${NC}" ;;
        FAILED*)  echo -e "  ${RED}$r${NC}" ;;
        TIMEOUT*) echo -e "  ${YELLOW}$r${NC}" ;;
        *)        echo "  $r" ;;
    esac
done
echo ""
echo -e "${BOLD}Total: ${TOTAL}  Passed: ${GREEN}${PASSED}${NC}  Failed: ${RED}${FAILED}${NC}${NC}"

if [[ $FAILED -gt 0 ]]; then
    exit 1
fi
