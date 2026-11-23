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
	# roleName IS THE TRACKED ROLE'S NAME. It said manager-role -- the kubebuilder
	# scaffold's default -- while config/rbac/role.yaml, which config/rbac/
	# kustomization.yaml binds, is spoke-identity-operator-role. So regenerating
	# renamed the ClusterRole and left the RoleBinding pointing at a role that no
	# longer existed, taking the operator's access to its own custom resources
	# with it.
	$(CONTROLLER_GEN) rbac:roleName=spoke-identity-operator-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

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
