LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION ?= v0.16.5
IMG ?= ghcr.io/kamarkat/kamarkat/ephemeral-job-operator:latest

.PHONY: all
all: generate manifests fmt vet build

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: generate
generate: controller-gen
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./api/...

.PHONY: manifests
manifests: controller-gen
	$(CONTROLLER_GEN) crd rbac:roleName=ephemeral-job-operator paths=./... \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt: ; go fmt ./...

.PHONY: vet
vet: ; go vet ./...

.PHONY: build
build: ; go build -o bin/manager ./cmd

.PHONY: test
test: ; go test ./... -count=1

.PHONY: docker-build
docker-build: ; docker build -t $(IMG) .
