.PHONY: all build run test clean install-crds uninstall-crds mockapi controller simulator

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod

# Binary names
CONTROLLER_BIN=bin/controller
MOCKAPI_BIN=bin/mockapi
SIMULATOR_BIN=bin/simulator

all: build

## Build targets

build: controller mockapi simulator

controller:
	$(GOBUILD) -o $(CONTROLLER_BIN) ./main.go

mockapi:
	$(GOBUILD) -o $(MOCKAPI_BIN) ./mockapi/main.go

simulator:
	$(GOBUILD) -o $(SIMULATOR_BIN) ./cmd/simulator/main.go

## Run targets

run-mockapi-west:
	DC_NAME=dc-west PORT=8080 $(MOCKAPI_BIN)

run-mockapi-east:
	DC_NAME=dc-east PORT=8081 $(MOCKAPI_BIN)

run-controller:
	$(CONTROLLER_BIN) --dc-api-url=http://localhost:8080

run-simulator:
	sudo $(SIMULATOR_BIN)

## Kubernetes targets

install-crds:
	kubectl apply -f config/crd/bases/

uninstall-crds:
	kubectl delete -f config/crd/bases/ --ignore-not-found

create-samples:
	kubectl apply -f config/samples/server-dc-west.yaml
	kubectl apply -f config/samples/server-dc-east.yaml
	kubectl apply -f config/samples/provisioningprofiles.yaml

delete-samples:
	kubectl delete -f config/samples/ --ignore-not-found

## Test targets

test:
	$(GOTEST) -v ./...

## Dependency management

deps:
	$(GOMOD) download
	$(GOMOD) tidy

## Clean

clean:
	rm -rf bin/

## Clean everything (kind cluster, VMs, processes, network)
clean-all:
	@echo "Stopping simulator processes..."
	-pkill -f "bin/simulator" 2>/dev/null || true
	-sudo pkill -f "bin/simulator" 2>/dev/null || true
	@echo "Killing QEMU VMs..."
	-sudo pkill -f "qemu-system-x86_64.*sim-worker" 2>/dev/null || true
	@echo "Deleting kind cluster..."
	-kind delete cluster --name stargate-demo 2>/dev/null || true
	@echo "Cleaning up VM storage..."
	-sudo rm -rf /var/lib/stargate/vms/ 2>/dev/null || true
	@echo "Cleaning up tap devices..."
	-sudo ip link delete tap-sim-worker- 2>/dev/null || true
	@echo "Cleaning up bridge..."
	-sudo ip link set stargate-br0 down 2>/dev/null || true
	-sudo ip link delete stargate-br0 2>/dev/null || true
	@echo "Cleaning up demo files..."
	-rm -rf /tmp/stargate-demo 2>/dev/null || true
	-rm -f /tmp/kind-config.yaml /tmp/stargate-kubeconfig 2>/dev/null || true
	@echo "Cleanup complete!"

## Help

help:
	@echo "Available targets:"
	@echo "  build           - Build all binaries"
	@echo "  controller      - Build the controller"
	@echo "  mockapi         - Build the mock DC API"
	@echo "  simulator       - Build the QEMU simulator controller"
	@echo "  run-mockapi-west - Run mock DC API for dc-west (port 8080)"
	@echo "  run-mockapi-east - Run mock DC API for dc-east (port 8081)"
	@echo "  run-controller  - Run the controller"
	@echo "  run-simulator   - Run the simulator (requires root)"
	@echo "  install-crds    - Install CRDs to cluster"
	@echo "  uninstall-crds  - Remove CRDs from cluster"
	@echo "  create-samples  - Create sample resources"
	@echo "  delete-samples  - Delete sample resources"
	@echo "  test            - Run tests"
	@echo "  deps            - Download and tidy dependencies"
	@echo "  clean           - Remove built binaries"
	@echo "  clean-all       - Delete kind cluster, VMs, network, and temp files"
