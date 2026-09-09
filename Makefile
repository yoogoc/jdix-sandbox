# jdix-sandbox — see docs/DESIGN.md
BIN     := bin
GOFLAGS := -trimpath
LDFLAGS := -s -w
PKGS    := ./...

.PHONY: all build build-linux test test-go test-sdk test-race fuzz vet fmt clean image load-image deploy-local demo openapi dev-preflight dev-up dev-down dev-sandbox tidy generate install-crds

all: fmt vet test build

## build: host binaries, for local development
build:
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

# The sandbox image is built for whatever the target cluster runs. Defaults to
# the host's architecture, which is what a local cluster will want; override for
# a remote one, e.g. `make image TARGETARCH=amd64`.
TARGETARCH ?= $(shell go env GOARCH)

## build-linux: static binaries for the sandbox image
build-linux:
	@mkdir -p $(BIN)/linux
	CGO_ENABLED=0 GOOS=linux GOARCH=$(TARGETARCH) \
		go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/linux/ ./cmd/...
	@echo "built for linux/$(TARGETARCH)"


test: test-go test-sdk

test-go:
	go test $(PKGS)
	cd sdk/go && go test ./...

## test-sdk: the Python client, in its own virtualenv
test-sdk:
	cd sdk/python && uv venv --quiet .venv 2>/dev/null || true
	cd sdk/python && uv pip install --quiet --python .venv/bin/python -e '.[dev]'
	cd sdk/python && .venv/bin/python -m pytest -q

test-race:
	go test -race $(PKGS)
	cd sdk/go && go test -race ./...

## fuzz: the two places tenant-controlled paths reach a privileged operation
fuzz:
	go test ./pkg/bwrap   -run=FuzzGenerate     -fuzz=FuzzGenerate     -fuzztime=$(or $(FUZZTIME),60s)
	go test ./pkg/safepath -run=FuzzOpenBeneath -fuzz=FuzzOpenBeneath -fuzztime=$(or $(FUZZTIME),60s)

vet:
	go vet $(PKGS)
	GOOS=linux go vet $(PKGS)
	cd sdk/go && go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

CONTROLLER_GEN := GOSUMDB=off GOFLAGS=-mod=mod go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

## openapi: check the specs against the servers that implement them
##   The specs under api/openapi are the contract the SDKs are generated from,
##   so a route that exists in only one of the two is a bug in whichever.
openapi:
	go test ./pkg/apiserver ./pkg/initd -run 'Spec|Operations|Responses' -count=1

## generate: deepcopy funcs, CRD manifests and RBAC from the kubebuilder markers
generate:
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./pkg/apis/...
	$(CONTROLLER_GEN) crd paths=./pkg/apis/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=jdix-controller paths=./pkg/controller/... output:rbac:artifacts:config=config/rbac
	gofmt -w pkg/apis

install-crds:
	kubectl apply -f config/crd

IMAGE ?= jdix/sandbox-base:dev

## image: the sandbox base image, carrying execd, jdix-init and bubblewrap
image: build-linux
	docker build --platform linux/$(TARGETARCH) -t $(IMAGE) -f build/Dockerfile.sandbox .

## load-image: import the built image into a local k3s/OrbStack cluster
##   Neither shares the Docker image store, so the image has to be handed to
##   containerd explicitly or the kubelet will try to pull it from Docker Hub.
load-image:
	IMAGE=$(IMAGE) ./hack/load-image.sh

## deploy-local: CRDs plus a freshly built image, ready to use
deploy-local: install-crds image load-image

## demo: end-to-end on a Linux host with bubblewrap installed
demo: build
	./hack/demo.sh

## dev-preflight: check whether this machine can debug locally, and say what is missing
dev-preflight:
	./hack/dev/preflight.sh

## dev-up: namespace, token, template and pool on the local cluster
dev-up:
	./hack/dev/up.sh

## dev-down: remove what dev-up created
dev-down:
	./hack/dev/down.sh

## dev-sandbox: bind a sandbox and open a shell in it, no controller needed
dev-sandbox:
	./hack/dev/sandbox.sh

clean:
	rm -rf $(BIN)
