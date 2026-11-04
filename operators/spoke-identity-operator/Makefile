SHELL := /usr/bin/env bash -euo pipefail

.PHONY: build run test manifests generate docker-build docker-push deploy

IMG ?= ghcr.io/kamarkat/kamarkat/spoke-identity-operator:latest

# Tools
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_TOOLS_VERSION ?= v0.16.5

# Setting SHELL to bash allows bash-specific expansion (e.g., $(LOCALBIN))
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

manifests: controller-gen
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

generate: controller-gen
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

build:
	go build -o bin/manager ./cmd/

run: build
	./bin/manager

test:
	go test ./...

docker-build:
	docker build -t ${IMG} .

docker-push:
	docker push ${IMG}

deploy:
	kubectl apply -f config/rbac/role.yaml
	kubectl apply -f config/manager/manager.yaml
