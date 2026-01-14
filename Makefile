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
	kubectl apply -f config/samples/hardware-dc-west.yaml
	kubectl apply -f config/samples/hardware-dc-east.yaml
	kubectl apply -f config/samples/templates.yaml

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
