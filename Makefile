# jdix-sandbox — see docs/DESIGN.md
BIN     := bin
GOFLAGS := -trimpath
LDFLAGS := -s -w
PKGS    := ./...

.PHONY: all build build-linux test test-go test-sdk test-race fuzz vet fmt clean image demo tidy generate install-crds

all: fmt vet test build

## build: host binaries, for local development
build:
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

## build-linux: static binaries for the sandbox image
build-linux:
	@mkdir -p $(BIN)/linux
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/linux/ ./cmd/...

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

## generate: deepcopy funcs, CRD manifests and RBAC from the kubebuilder markers
generate:
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./pkg/apis/...
	$(CONTROLLER_GEN) crd paths=./pkg/apis/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=jdix-controller paths=./pkg/controller/... output:rbac:artifacts:config=config/rbac
	gofmt -w pkg/apis

install-crds:
	kubectl apply -f config/crd

image: build-linux
	docker build -t jdix/sandbox-base:dev -f build/Dockerfile.sandbox .

## demo: end-to-end on a Linux host with bubblewrap installed
demo: build
	./hack/demo.sh

clean:
	rm -rf $(BIN)
