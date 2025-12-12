# Version must be provided via environment variable (e.g., from workflow)
VERSION ?= dev
# Image base name
IMG_BASE ?= zot.soh.re/isolation-proxy
# Image URLs with tags
IMG_VERSION ?= $(IMG_BASE):$(VERSION)
IMG_LATEST ?= $(IMG_BASE):latest
# Helm chart repository
HELM_REPO ?= oci://zot.soh.re
# Helm chart name
HELM_CHART_NAME ?= isolation-proxy-operator
# Namespace to deploy the operator
NAMESPACE ?= isolation-proxy-system

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: fmt vet ## Run the operator from your host.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	docker build -t ${IMG_VERSION} -t ${IMG_LATEST} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG_VERSION}
	docker push ${IMG_LATEST}

##@ Deployment

.PHONY: install-crd
install-crd: ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	kubectl apply -f config/crd/bases/dedicatedservice-crd.yaml

.PHONY: uninstall-crd
uninstall-crd: ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	kubectl delete -f config/crd/bases/dedicatedservice-crd.yaml

.PHONY: deploy
deploy: install-crd ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	kubectl apply -f config/manager/manager.yaml

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	kubectl delete -f config/manager/manager.yaml

.PHONY: deploy-sample
deploy-sample: ## Deploy a sample DedicatedService.
	kubectl apply -f example-dedicatedservice.yaml

.PHONY: undeploy-sample
undeploy-sample: ## Remove the sample DedicatedService.
	kubectl delete -f example-dedicatedservice.yaml

##@ Build Dependencies

.PHONY: tidy
tidy: ## Run go mod tidy.
	go mod tidy

.PHONY: vendor
vendor: ## Run go mod vendor.
	go mod vendor

##@ All-in-one

.PHONY: all-deploy
all-deploy: docker-build deploy ## Build, push image and deploy operator.

.PHONY: clean
clean: ## Clean up build artifacts.
	rm -rf bin/ vendor/

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	helm lint ./helm/isolation-proxy-operator

.PHONY: helm-template
helm-template: ## Template the Helm chart.
	helm template isolation-proxy-operator ./helm/isolation-proxy-operator --namespace $(NAMESPACE)

.PHONY: helm-install
helm-install: ## Install the operator using Helm.
	helm install isolation-proxy-operator ./helm/isolation-proxy-operator --namespace $(NAMESPACE) --create-namespace

.PHONY: helm-upgrade
helm-upgrade: ## Upgrade the operator using Helm.
	helm upgrade isolation-proxy-operator ./helm/isolation-proxy-operator --namespace $(NAMESPACE)

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the operator using Helm.
	helm uninstall isolation-proxy-operator --namespace $(NAMESPACE)

.PHONY: helm-sync-crds
helm-sync-crds: ## Sync CRDs from config/crd/bases to Helm chart templates.
	cp config/crd/bases/soh.re_dedicatedservices.yaml helm/isolation-proxy-operator/templates/soh.re_dedicatedservices.yaml
	cp config/crd/bases/soh.re_isolationpods.yaml helm/isolation-proxy-operator/templates/soh.re_isolationpods.yaml

.PHONY: helm-package
helm-package: helm-sync-crds ## Package the Helm chart.
	mkdir -p ./dist
	helm package ./helm/isolation-proxy-operator -d ./dist --version $(VERSION) --app-version $(VERSION)

.PHONY: helm-push
helm-push: helm-package ## Package and push the Helm chart to OCI registry.
	helm push ./dist/$(HELM_CHART_NAME)-*.tgz $(HELM_REPO)

##@ Release

.PHONY: release
release: docker-build docker-push helm-push ## Build and push both Docker image and Helm chart.
