
# Image URL to use all building/pushing image targets
IMG ?= quay.io/opendatahub/odh-workbenches-operator:odh-stable
# Container engine to use for building and pushing images (podman or docker)
CONTAINER_ENGINE ?= podman
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.32.0

# Get the currently used golang install path (in GOBIN, currentl directory or GOPATH/bin)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Use existing local.mk for dev overrides
-include local.mk

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# ODH_PLATFORM_TYPE selects manifest sources in opt/manifest-sources.sh
# (loaded by get_all_manifests.sh):
#   OpenDataHub (default) — opendatahub-io upstream
#   rhoai — red-hat-data-services RHOAI/downstream
ODH_PLATFORM_TYPE ?= OpenDataHub

.PHONY: manifests-fetch
manifests-fetch: ## Fetch component manifests into opt/manifests/ (ODH_PLATFORM_TYPE=OpenDataHub|rhoai).
	env ODH_PLATFORM_TYPE="$(ODH_PLATFORM_TYPE)" bash get_all_manifests.sh

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	"$(GOLANGCI_LINT)" run --fix

##@ Testing

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test $$(go list ./... | grep -v /e2e | grep -v /tests/) -coverprofile cover.out

.PHONY: unit-test
unit-test: manifests generate envtest ## Run unit tests (no fmt/vet check).
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test $$(go list ./... | grep -v /e2e | grep -v /tests/) -coverprofile cover.out

.PHONY: test-e2e
test-e2e: ## Run end-to-end tests against the cluster specified in ~/.kube/config.
	go test ./tests/e2e/ -v -count=1 -timeout 30m $(if $(GINKGO_LABEL_FILTER),--ginkgo.label-filter="$(GINKGO_LABEL_FILTER)",)

.PHONY: test-coverage
test-coverage: test ## Generate HTML coverage report.
	go tool cover -html=cover.out -o coverage.html
	@echo "Coverage report written to coverage.html"

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

.PHONY: image-build
image-build: ## Build the operator container image.
	test -d opt/manifests || (echo "opt/manifests missing — run 'make manifests-fetch'" && exit 1)
	"$(CONTAINER_ENGINE)" build -t "$(IMG)" .

.PHONY: image-push
image-push: ## Push the operator container image to a registry.
	"$(CONTAINER_ENGINE)" push "$(IMG)"

.PHONY: image-build-push
image-build-push: image-build image-push ## Build and push the operator container image.

##@ Helm chart

CHART_DIR ?= charts/operator
HELM ?= helm
HELM_RELEASE ?= workbenches-operator
HELM_NAMESPACE ?= workbenches-operator-system
APPLICATIONS_NAMESPACE ?= opendatahub

# Shared --set flags for standalone cluster installs.
# Platform path only sets operatorNamespace (= ApplicationsNamespace); applicationsNamespace
# is empty in values.yaml and falls back to operatorNamespace. Standalone Makefile keeps
# a dedicated operator NS by setting both explicitly.
# Override extras via HELM_DEPLOY_EXTRA_SETS (do not replace HELM_DEPLOY_SETS) so namespace
# wiring is preserved — e.g. CI webhook/image overrides.
HELM_DEPLOY_EXTRA_SETS ?=
HELM_DEPLOY_SETS = \
	--set-string operatorNamespace=$(HELM_NAMESPACE) \
	--set-string applicationsNamespace=$(APPLICATIONS_NAMESPACE) \
	--set leaderElection.enabled=false \
	--set-string params.workbenchesOperatorImage=$(IMG) \
	$(HELM_DEPLOY_EXTRA_SETS)

.PHONY: chart-sync-crd
chart-sync-crd: manifests ## Copy generated Workbenches CRD into the Helm chart (generated; not a second source of truth).
	mkdir -p "$(CHART_DIR)/crd"
	cp config/crd/bases/components.platform.opendatahub.io_workbenches.yaml "$(CHART_DIR)/crd/workbenches.crd.yaml"

.PHONY: chart-sync-rbac
chart-sync-rbac: manifests ## Sync generated ClusterRole rules into the Helm chart (generated; not a second source of truth).
	bash hack/chart-sync-rbac.sh "$(CHART_DIR)"

.PHONY: chart-sync
chart-sync: chart-sync-crd chart-sync-rbac ## Sync all generated resources into the Helm chart.

.PHONY: chart-verify-sync
chart-verify-sync: manifests ## Verify Helm chart is in sync with generated config/ artifacts.
	bash hack/chart-verify-sync.sh "$(CHART_DIR)"

.PHONY: chart-verify-inventory
chart-verify-inventory: manifests kustomize chart-sync-crd ## Verify kustomize and Helm chart produce the same resource kinds.
	bash hack/chart-verify-inventory.sh "$(CHART_DIR)"

.PHONY: chart-verify-params
chart-verify-params: ## Verify params.env matches values.yaml default manager image.
	@test "$$(grep '^workbenches-operator-image=' "$(CHART_DIR)/params.env" | cut -d= -f2-)" = \
		"$$(grep 'workbenchesOperatorImage:' "$(CHART_DIR)/values.yaml" | awk '{print $$2}')" || \
		(echo "params.env workbenches-operator-image must match values.params.workbenchesOperatorImage" && exit 1)

.PHONY: helm-lint
helm-lint: chart-sync chart-verify-params ## Lint the operator Helm chart.
	$(HELM) lint "$(CHART_DIR)"

.PHONY: helm-template
helm-template: chart-sync chart-verify-params ## Render the operator Helm chart locally.
	$(HELM) template "$(HELM_RELEASE)" "$(CHART_DIR)" \
		--namespace "$(HELM_NAMESPACE)" \
		--set-string operatorNamespace=$(HELM_NAMESPACE) \
		--set-string applicationsNamespace=$(APPLICATIONS_NAMESPACE)

.PHONY: helm-deploy
helm-deploy: chart-sync chart-verify-params ## Deploy operator via Helm (run undeploy first if switching from kustomize).
	$(HELM) upgrade --install "$(HELM_RELEASE)" "$(CHART_DIR)" \
		--namespace "$(HELM_NAMESPACE)" \
		--create-namespace \
		$(HELM_DEPLOY_SETS)

.PHONY: helm-undeploy
helm-undeploy: ## Uninstall operator Helm release and Workbenches CRD from ~/.kube/config.
	@if $(HELM) status "$(HELM_RELEASE)" --namespace "$(HELM_NAMESPACE)" >/dev/null 2>&1; then \
		$(HELM) uninstall "$(HELM_RELEASE)" --namespace "$(HELM_NAMESPACE)"; \
	else \
		echo "Release $(HELM_RELEASE) not found in namespace $(HELM_NAMESPACE) — skipping uninstall"; \
	fi
	"$(KUBECTL)" delete crd workbenches.components.platform.opendatahub.io --ignore-not-found=$(ignore-not-found)


##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/crd | "$(KUBECTL)" apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/crd | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller="$(IMG)"
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	@# Delete Workbenches CRs first so the running operator can process finalizers
	"$(KUBECTL)" delete workbenches --all --timeout=60s
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.18.0
ENVTEST_VERSION ?= release-0.23
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and target directory.
# $1 - target path, $2 - package, $3 - version
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
	set -e; \
	package="$(2)@$(3)" ;\
	echo "Downloading $${package}" ;\
	rm -f "$(1)" || true ;\
	GOBIN="$(LOCALBIN)" go install $${package} ;\
	mv "$(1)" "$(1)-$(3)" ;\
}
@ln -sf "$(1)-$(3)" "$(1)"
endef
