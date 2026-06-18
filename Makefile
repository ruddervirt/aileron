IMAGE_REPO ?= ghcr.io/ruddervirt/aileron
GIT_SHA    := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
GIT_DIRTY  := $(shell git diff --quiet 2>/dev/null || echo -dirty)
TAG        ?= $(GIT_SHA)$(GIT_DIRTY)

# Whether bake also moves the floating ":latest" tag (main-branch CI sets true;
# release builds leave it false). See docker-bake.hcl.
MOVE_LATEST ?= false

# OCI registry the versioned Helm chart is published to (see `helm-publish`).
# Zones consume from $(HELM_OCI_REPO)/aileron --version <X.Y.Z>.
HELM_OCI_REPO ?= oci://ghcr.io/ruddervirt/charts

CONTROLLER_GEN ?= bin/controller-gen
ENVTEST        ?= $(shell pwd)/bin/setup-envtest
GOLANGCI_LINT  ?= bin/golangci-lint
KIND           ?= bin/kind
GOLANGCI_LINT_VERSION ?= v2.11.4
ENVTEST_K8S_VERSION   ?= 1.36.0
KIND_CLUSTER_NAME ?= aileron-test

# Optional bake overlay (CI passes BAKE_FILES="-f docker-bake.ci.hcl" for GHA cache).
BAKE_FILES ?=
DOCKER_BAKE := IMAGE_BASE=$(IMAGE_REPO) SHA_TAG=$(TAG) MOVE_LATEST=$(MOVE_LATEST) docker buildx bake $(BAKE_FILES)

.PHONY: build push helm-publish generate sync-chart-crds verify-crds lint test test-e2e check

# Authoritative CRD sources. Anything added here is copied into the chart by
# sync-chart-crds and checked by verify-crds, so chart/templates/crds/ never
# drifts from source of truth.
CHART_CRD_DIR   = chart/aileron/templates/crds
CRD_SOURCE_DIRS = config/crd/bases

generate: $(CONTROLLER_GEN)
	# Scope object generation to this module's source roots. Using "./..." walks
	# the untracked OLDMODULES/ dir (which carries its own go.mod files) and
	# breaks controller-gen's package loader — and thus push/install. Only
	# ./api/ and ./internal/ carry +kubebuilder:object markers.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..." paths="./internal/..."
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook \
		paths="./api/..." paths="./internal/..." \
		output:crd:artifacts:config=config/crd/bases
	$(MAKE) sync-chart-crds

# sync-chart-crds mirrors every authoritative CRD into chart/templates/crds/.
# Uses cp -f (not rm + cp) because some dev filesystems have the chart dir
# sharing inodes with config/crd/bases; a blanket rm there wiped the source.
# Stale chart CRDs (for removed CRDs) must be cleaned up by hand — verify-crds
# catches that drift on CI.
sync-chart-crds:
	@mkdir -p $(CHART_CRD_DIR)
	@for dir in $(CRD_SOURCE_DIRS); do \
		for f in $$dir/*.yaml; do \
			dest=$(CHART_CRD_DIR)/$$(basename $$f); \
			if [ ! -e $$dest ] || ! cmp -s $$f $$dest; then \
				cp -f $$f $$dest && echo "synced $$f → $$dest"; \
			fi; \
		done; \
	done

# verify-crds regenerates CRDs into a throwaway tree and diffs against what's
# committed. CI runs this on every PR; a non-zero exit means someone edited
# an API type without running `make generate`, or hand-tweaked a chart CRD.
verify-crds: $(CONTROLLER_GEN)
	@tmp=$$(mktemp -d); trap "rm -rf $$tmp" EXIT; \
	mkdir -p $$tmp/config/crd/bases $$tmp/chart; \
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook \
		paths="./api/..." paths="./internal/..." \
		output:crd:artifacts:config=$$tmp/config/crd/bases; \
	cp $$tmp/config/crd/bases/*.yaml $$tmp/chart/; \
	diff -r $$tmp/config/crd/bases config/crd/bases || { echo "config/crd/bases drift — run 'make generate'"; exit 1; }; \
	diff -r $$tmp/chart $(CHART_CRD_DIR) || { echo "$(CHART_CRD_DIR) drift — run 'make generate'"; exit 1; }

# Build all images in one bake call so the shared `builder` stage compiles its
# Go binaries once, not once per image. `--load` puts them in the local docker
# daemon for kind/local testing.
build: generate
	$(DOCKER_BAKE) --load

# Same bake graph, pushed straight to the registry — used by CI. Deployment
# zones consume the published images + chart; this repo never deploys them.
push: generate
	$(DOCKER_BAKE) --push

# Package the chart at a release version and push it to the OCI registry.
# Used by the release workflow after `make push`. --version/--app-version stamp
# the packaged tarball without editing tracked files; AppVersion becomes the
# resolved image tag (see chart/aileron/templates/_helpers.tpl "aileron.imageTag").
# CHART_VERSION must be plain semver (1.2.3); APP_VERSION carries the image tag
# (v1.2.3) so the chart's images resolve to the release build.
CHART_VERSION ?= $(error CHART_VERSION required, e.g. CHART_VERSION=1.2.3)
APP_VERSION   ?= $(CHART_VERSION)
helm-publish:
	rm -rf dist && mkdir -p dist
	helm package chart/aileron --version $(CHART_VERSION) --app-version $(APP_VERSION) -d dist
	helm push dist/aileron-$(CHART_VERSION).tgz $(HELM_OCI_REPO)

# Run the same CI gates as the GitHub Actions workflow
# (lint + test + test-e2e), concurrently. Re-invokes make with -j3 so a
# plain `make check` Just Works without the caller remembering to pass -j.
# Note: test-e2e creates and tears down a local kind cluster named
# $(KIND_CLUSTER_NAME) — don't run this if you're using that name elsewhere.
check:
	@$(MAKE) -j3 --no-print-directory lint test test-e2e

$(CONTROLLER_GEN):
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1

# Install golangci-lint from the upstream pre-built binary (~5s) instead of
# `go install` (~2-3min cold). Pinned via GOLANGCI_LINT_VERSION.
$(GOLANGCI_LINT):
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
		| sh -s -- -b $(shell pwd)/bin $(GOLANGCI_LINT_VERSION)

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

$(ENVTEST):
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

test: $(ENVTEST)
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path --bin-dir $(shell pwd)/bin/k8s)" \
		go test $$(go list ./... | grep -v /test/) -race

$(KIND):
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/kind@latest

test-e2e: $(KIND)
	$(KIND) create cluster --name $(KIND_CLUSTER_NAME)
	kubectl --context kind-$(KIND_CLUSTER_NAME) apply \
		-f config/crd/bases/ruddervirt.io_virtualmachinebuilds.yaml \
		-f config/crd/bases/ruddervirt.io_virtualmachineclones.yaml \
		-f config/crd/bases/ruddervirt.io_virtualmachinenamespaces.yaml
	kubectl --context kind-$(KIND_CLUSTER_NAME) wait --for condition=established --timeout=30s \
		crd/virtualmachinebuilds.ruddervirt.io \
		crd/virtualmachineclones.ruddervirt.io \
		crd/virtualmachinenamespaces.ruddervirt.io
	go test ./test/e2e/... -v -count=1 -timeout 5m; \
	EXIT_CODE=$$?; \
	$(KIND) delete cluster --name $(KIND_CLUSTER_NAME); \
	exit $$EXIT_CODE
