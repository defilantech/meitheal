# Image URLs to use for the building/pushing image targets
MEITHEAL_OPERATOR_IMG ?= ghcr.io/defilantech/meitheal-operator:dev
MEITHEAL_AGENT_IMG ?= ghcr.io/defilantech/meitheal-agent:dev
# Coder-Job image: the meitheal-agent binary plus go/make/git/helm/
# golangci-lint/controller-gen on a golang base pinned to go.mod's version,
# so the in-Job coder and the in-cluster agent's fast gate can run
# `make fmt vet lint test`. Referenced by Agent.spec.execution.image.
MEITHEAL_AGENT_CODER_IMG ?= ghcr.io/defilantech/meitheal-agent-coder:dev

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'.

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: chart-crds
chart-crds: manifests ## Generate CRDs and sync them to the Helm chart templates.
	@./scripts/sync-crds.sh

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# A fresh seed per run keeps spec-order randomization (it catches test
# pollution); echoing it into the log makes any failure replayable:
# `make test GINKGO_SEED=<n>` reruns the exact order.
#
# `?=` is recursive, so `$(shell date +%s)` re-runs at every reference and the
# echoed seed can differ from the exported one. `ifndef` forces one evaluation,
# and is false for a command-line seed, so an explicit `GINKGO_SEED=<n>` wins.
ifndef GINKGO_SEED
GINKGO_SEED := $(shell date +%s)
endif

# go test kills a package binary after 10 minutes by default, so the test
# targets set the limit explicitly. GO_TEST_FLAGS appends flags to go test.
GO_TEST_TIMEOUT ?= 20m
GO_TEST_FLAGS ?=

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	@echo "ginkgo seed: $(GINKGO_SEED) (replay: make test GINKGO_SEED=$(GINKGO_SEED))"
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" GINKGO_SEED=$(GINKGO_SEED) go test -timeout $(GO_TEST_TIMEOUT) $(GO_TEST_FLAGS) ./... -coverprofile cover.out

# The envtest suites only, so CI can run a second fixed seed over just the
# specs whose ordering matters. The gate stays single-pass, and `make test`
# above is what it runs.
.PHONY: test-envtest
test-envtest: manifests generate fmt vet setup-envtest ## Run the envtest suites only (CI second seed pass).
	@echo "ginkgo seed: $(GINKGO_SEED) (replay: make test-envtest GINKGO_SEED=$(GINKGO_SEED))"
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" GINKGO_SEED=$(GINKGO_SEED) go test -timeout $(GO_TEST_TIMEOUT) $(GO_TEST_FLAGS) ./internal/controller/ -count=1

.PHONY: test-scripts
test-scripts: ## Run the shell regression tests (finalize, image scan, macOS signing and notarization, pinned release tools, Go module download retries).
	@bash scripts/finalize_verify_test.sh
	@bash scripts/scan-images_test.sh
	@bash scripts/macos-sign_test.sh
	@bash scripts/macos-notarize_test.sh
	@bash scripts/install-rcodesign_test.sh
	@bash scripts/install-pinned-tool_test.sh
	@bash scripts/go-mod-download_test.sh

.PHONY: test-chart
test-chart: ## Lint and unit-test the Helm chart (requires helm + helm-unittest plugin).
	helm lint charts/meitheal
	helm unittest charts/meitheal

.PHONY: check-reviewer-prompts
check-reviewer-prompts: ## Verify the reviewer Agent manifests carry config/system-prompts/reviewer.md.
	go run ./cmd/sync-reviewer-prompts --check

.PHONY: sync-reviewer-prompts
sync-reviewer-prompts: ## Write config/system-prompts/reviewer.md into the reviewer Agent manifests.
	go run ./cmd/sync-reviewer-prompts

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-all
lint-all: golangci-lint ## Lint under all supported GOOS settings (slower; run before pushing).
	@echo "==> Lint GOOS=darwin"
	GOOS=darwin $(GOLANGCI_LINT) run ./...
	@echo "==> Lint GOOS=linux"
	GOOS=linux $(GOLANGCI_LINT) run ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the meitheal CLI, operator and agent binaries into bin/.
	go build -o bin/meitheal ./cmd/meitheal
	go build -o bin/meitheal-operator ./cmd/meitheal-operator
	go build -o bin/meitheal-agent ./cmd/meitheal-agent

# Generated at build time and never committed: the tree goes stale with every
# dependency bump. .goreleaser.yaml ships it in each archive and image.
THIRD_PARTY_LICENSES_DIR ?= third_party_licenses
# The binaries .goreleaser.yaml builds; one tree covers their union.
THIRD_PARTY_LICENSES_PKGS ?= ./cmd/meitheal ./cmd/meitheal-operator ./cmd/meitheal-agent
# The windows cli links a module the linux and darwin builds do not (cobra's
# mousetrap), so every shipped GOOS gets a pass and the trees are merged.
THIRD_PARTY_LICENSES_GOOS ?= linux darwin windows

.PHONY: third-party-licenses
third-party-licenses: go-licenses ## Generate the third-party license tree the release archives and images ship.
	@staging="$(THIRD_PARTY_LICENSES_DIR).tmp"; rm -rf "$$staging"; \
		trap 'rm -rf "$$staging" $(THIRD_PARTY_LICENSES_DIR).tmp.*' EXIT; \
		for os in $(THIRD_PARTY_LICENSES_GOOS); do \
			out="$(THIRD_PARTY_LICENSES_DIR).tmp.$$os"; \
			rm -rf "$$out"; \
			echo "Collecting third-party licenses (GOOS=$$os)"; \
			GOOS=$$os $(GO_LICENSES) save $(THIRD_PARTY_LICENSES_PKGS) \
				--save_path "$$out" --ignore $$(go list -m) || exit 1; \
			mkdir -p "$$staging"; \
			cp -Rf "$$out/." "$$staging/" || exit 1; \
		done; \
		rm -rf "$(THIRD_PARTY_LICENSES_DIR)"; \
		mv "$$staging" "$(THIRD_PARTY_LICENSES_DIR)"
	@echo "$(THIRD_PARTY_LICENSES_DIR)/ ready"

# Built per-arch via goreleaser at release time; these targets exist for
# local development against a kind cluster and for the e2e workflow.
.PHONY: docker-build
docker-build: docker-build-operator docker-build-agent ## Build the operator and agent images.

.PHONY: docker-build-operator
docker-build-operator: ## Build docker image for the meitheal-operator.
	$(CONTAINER_TOOL) build -f Dockerfile.operator -t ${MEITHEAL_OPERATOR_IMG} .

.PHONY: docker-build-agent
docker-build-agent: ## Build docker image for the meitheal-agent.
	$(CONTAINER_TOOL) build -f Dockerfile.agent -t ${MEITHEAL_AGENT_IMG} .

.PHONY: docker-build-agent-coder
docker-build-agent-coder: ## Build the coder-Job image (meitheal-agent + go/make/git/helm/golangci-lint/controller-gen).
	$(CONTAINER_TOOL) build -f Dockerfile.agent-builder -t ${MEITHEAL_AGENT_CODER_IMG} .

.PHONY: docker-push-operator
docker-push-operator: ## Push the meitheal-operator image.
	$(CONTAINER_TOOL) push ${MEITHEAL_OPERATOR_IMG}

.PHONY: docker-push-agent
docker-push-agent: ## Push the meitheal-agent image.
	$(CONTAINER_TOOL) push ${MEITHEAL_AGENT_IMG}

.PHONY: docker-push-agent-coder
docker-push-agent-coder: ## Push the coder-Job image.
	$(CONTAINER_TOOL) push ${MEITHEAL_AGENT_CODER_IMG}

##@ Agent (macOS install)

# MEITHEAL_AGENT_LABEL is the launchd service label for meitheal-agent. It is
# also the bundle identifier the release binary is signed with.
MEITHEAL_AGENT_LABEL ?= dev.meitheal.agent

# MEITHEAL_AGENT_VERSION is stamped into the versioned install layout and the
# ldflags Version variable.
MEITHEAL_AGENT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build-agent-versioned
build-agent-versioned: fmt vet ## Build meitheal-agent with ldflags version info.
	go build \
		-ldflags "-X main.Version=$(MEITHEAL_AGENT_VERSION) -X main.GitCommit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) -X main.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)" \
		-o bin/meitheal-agent \
		./cmd/meitheal-agent

# MEITHEAL_INSTALL_ROOT is the user-owned managed layout root for
# meitheal-agent. On macOS the default matches
# ResolveInstallRoot("meitheal-agent"). Do NOT point it at a root-owned path:
# self-update writes here without sudo.
MEITHEAL_INSTALL_ROOT ?= $(HOME)/Library/Application Support/meitheal/meitheal-agent

# meitheal-agent runtime configuration rendered into the launchd plist by
# install-agent. Override on the command line, e.g.:
#   make install-agent \
#     MEITHEAL_INSTALLED_MODELS=<model> \
#     MEITHEAL_NODE_NAME=<fleet node name> \
#     MEITHEAL_KUBECONFIG=$(HOME)/.kube/config \
#     MEITHEAL_ACCELERATOR=metal
MEITHEAL_NODE_NAME        ?= $(shell hostname -s)
MEITHEAL_KUBECONFIG       ?= $(HOME)/.kube/config
MEITHEAL_ACCELERATOR      ?= metal
MEITHEAL_GIT_REMOTE_URL   ?= $(shell git remote get-url origin 2>/dev/null)
MEITHEAL_COMMIT_EMAIL     ?= $(shell git config user.email 2>/dev/null)
# Required: the comma-separated model names this node serves.
MEITHEAL_INSTALLED_MODELS ?=

.PHONY: check-install-vars
check-install-vars:
	@if [ -z "$(MEITHEAL_INSTALLED_MODELS)" ]; then \
		echo "ERROR: MEITHEAL_INSTALLED_MODELS is required (comma-separated model names this node serves)."; \
		echo "       e.g. make install-agent MEITHEAL_INSTALLED_MODELS=<model>"; \
		exit 1; \
	fi
	@if [ -z "$(MEITHEAL_NODE_NAME)" ]; then \
		echo "ERROR: MEITHEAL_NODE_NAME is empty (hostname -s returned nothing); set it explicitly."; \
		exit 1; \
	fi

.PHONY: install-agent
install-agent: check-install-vars build-agent-versioned ## Install meitheal-agent into the user-owned managed layout and start its launchd service (macOS only, no sudo).
	@echo "Installing meitheal-agent $(MEITHEAL_AGENT_VERSION)..."
	mkdir -p "$(MEITHEAL_INSTALL_ROOT)/versions/$(MEITHEAL_AGENT_VERSION)"
	cp bin/meitheal-agent "$(MEITHEAL_INSTALL_ROOT)/versions/$(MEITHEAL_AGENT_VERSION)/meitheal-agent"
	chmod 0755 "$(MEITHEAL_INSTALL_ROOT)/versions/$(MEITHEAL_AGENT_VERSION)/meitheal-agent"
	@ln -sfn "$(MEITHEAL_INSTALL_ROOT)/versions/$(MEITHEAL_AGENT_VERSION)" \
		"$(MEITHEAL_INSTALL_ROOT)/current"
	@echo "Staged at $(MEITHEAL_INSTALL_ROOT)/current -> versions/$(MEITHEAL_AGENT_VERSION)"
	@echo "Installing launchd service..."
	@# Render the launchd plist: substitute the install-root path AND every
	@# REPLACE_WITH_* placeholder, so the installed service has a complete,
	@# working command line with no leftover placeholders.
	@sed \
		-e "s|/Users/YOUR_USERNAME/Library/Application Support/meitheal/meitheal-agent|$(MEITHEAL_INSTALL_ROOT)|g" \
		-e "s|REPLACE_WITH_NODE_NAME|$(MEITHEAL_NODE_NAME)|g" \
		-e "s|REPLACE_WITH_ACCELERATOR|$(MEITHEAL_ACCELERATOR)|g" \
		-e "s|REPLACE_WITH_INSTALLED_MODELS|$(MEITHEAL_INSTALLED_MODELS)|g" \
		-e "s|REPLACE_WITH_KUBECONFIG|$(MEITHEAL_KUBECONFIG)|g" \
		-e "s|REPLACE_WITH_GIT_REMOTE_URL|$(MEITHEAL_GIT_REMOTE_URL)|g" \
		-e "s|REPLACE_WITH_EMAIL|$(MEITHEAL_COMMIT_EMAIL)|g" \
		deployment/macos/dev.meitheal.agent.plist \
		> ~/Library/LaunchAgents/$(MEITHEAL_AGENT_LABEL).plist
	@echo "Starting meitheal-agent service..."
	@# Bootstrap the plist the first time (noop if already bootstrapped).
	@launchctl bootstrap gui/$$(id -u) ~/Library/LaunchAgents/$(MEITHEAL_AGENT_LABEL).plist 2>/dev/null || true
	@# kickstart -k ensures a running instance picks up the new binary.
	launchctl kickstart -k gui/$$(id -u)/$(MEITHEAL_AGENT_LABEL)
	@echo "meitheal-agent installed and restarted"
	@echo ""
	@echo "Edit ~/Library/LaunchAgents/$(MEITHEAL_AGENT_LABEL).plist to set flags, then run:"
	@echo "  launchctl kickstart -k gui/$$(id -u)/$(MEITHEAL_AGENT_LABEL)"
	@echo "To view logs: tail -f /tmp/meitheal-agent.log"

.PHONY: uninstall-agent
uninstall-agent: ## Uninstall the meitheal-agent launchd service (leaves the install root intact for rollback).
	@echo "Stopping meitheal-agent service..."
	@launchctl bootout gui/$$(id -u)/$(MEITHEAL_AGENT_LABEL) 2>/dev/null || true
	rm -f ~/Library/LaunchAgents/$(MEITHEAL_AGENT_LABEL).plist
	@echo "meitheal-agent service uninstalled (install root preserved at $(MEITHEAL_INSTALL_ROOT))"

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GO_LICENSES = $(LOCALBIN)/go-licenses

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.23.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.15.0
GO_LICENSES_VERSION ?= v2.0.1

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: go-licenses
go-licenses: $(GO_LICENSES) ## Download go-licenses locally if necessary.
$(GO_LICENSES): $(LOCALBIN)
	$(call go-install-tool,$(GO_LICENSES),github.com/google/go-licenses/v2,$(GO_LICENSES_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist.
# hack/go-mod-download.sh retries the install when the module proxy fails with a network error.
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) ;\
GOBIN=$(LOCALBIN) hack/go-mod-download.sh install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $$(realpath $(1)-$(3)) $(1)
endef
