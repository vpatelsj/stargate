.PHONY: all build run test clean install-crds uninstall-crds mockapi azure-controller qemu-controller simulator \
        clean-all clean-kind clean-azure clean-tailscale clean-local prep-dc-inventory azure

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod

# Binary names
AZURE_CONTROLLER_BIN=bin/azure-controller
QEMU_CONTROLLER_BIN=bin/qemu-controller
MOCKAPI_BIN=bin/mockapi
SIMULATOR_BIN=bin/simulator
PREP_DC_INVENTORY_BIN=bin/prep-dc-inventory
AZURE_BIN=bin/azure
MX_AZURE_BIN=bin/mx-azure

all: build

## Build targets

build: azure-controller qemu-controller mockapi simulator prep-dc-inventory azure mx-azure

azure-controller:
	$(GOBUILD) -o $(AZURE_CONTROLLER_BIN) ./cmd/azure-controller/main.go

qemu-controller:
	$(GOBUILD) -o $(QEMU_CONTROLLER_BIN) ./cmd/qemu-controller/main.go

mockapi:
	$(GOBUILD) -o $(MOCKAPI_BIN) ./mockapi/main.go

simulator:
	$(GOBUILD) -o $(SIMULATOR_BIN) ./cmd/simulator/main.go

prep-dc-inventory:
	$(GOBUILD) -o $(PREP_DC_INVENTORY_BIN) ./cmd/infra-prep/main.go

azure:
	$(GOBUILD) -o $(AZURE_BIN) ./cmd/azure/main.go

mx-azure:
	$(GOBUILD) -o $(MX_AZURE_BIN) ./cmd/mx-azure/main.go

## Run targets

run-mockapi-west:
	DC_NAME=dc-west PORT=8080 $(MOCKAPI_BIN)

run-mockapi-east:
	DC_NAME=dc-east PORT=8081 $(MOCKAPI_BIN)

run-controller:
	$(AZURE_CONTROLLER_BIN) --dc-api-url=http://localhost:8080

run-qemu-controller:
	$(QEMU_CONTROLLER_BIN)

run-simulator:
	sudo $(SIMULATOR_BIN)

# Build and start both controllers (azure + qemu)
start-controllers: azure-controller qemu-controller
	@echo "Starting azure-controller..."
	@nohup $(AZURE_CONTROLLER_BIN) --metrics-bind-address=:8081 > /tmp/stargate-azure-controller.log 2>&1 & echo $$! > /tmp/stargate-azure-controller.pid
	@echo "azure-controller PID: $$(cat /tmp/stargate-azure-controller.pid)"
	@echo "Starting qemu-controller with sudo..."
	@sudo -n true 2>/dev/null || (echo "sudo requires a password; run 'sudo -v' first" && exit 1)
	@nohup sudo -n -E $(QEMU_CONTROLLER_BIN) --metrics-bind-address=:8082 > /tmp/stargate-qemu-controller.log 2>&1 & echo $$! > /tmp/stargate-qemu-controller.pid
	@sudo -n chmod 644 /tmp/stargate-qemu-controller.log 2>/dev/null || true
	@echo "qemu-controller PID: $$(cat /tmp/stargate-qemu-controller.pid)"
	@echo "Logs: /tmp/stargate-azure-controller.log, /tmp/stargate-qemu-controller.log"

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

## Clean everything (kind cluster, Azure VMs, Tailscale devices, local processes, binaries)
clean-all: clean-kind clean-tailscale clean-azure clean-local clean
	@echo "=== Full cleanup complete! ==="

## Delete local Kind cluster
clean-kind:
	@echo "=== Deleting Kind cluster ==="
	-kind delete cluster --name stargate-demo 2>/dev/null || true
	-rm -f /tmp/kind-config.yaml /tmp/stargate-kubeconfig 2>/dev/null || true

## Remove stargate VMs from Tailscale (requires TAILSCALE_CLIENT_ID and TAILSCALE_CLIENT_SECRET env vars)
clean-tailscale:
	@echo "=== Removing stargate devices from Tailscale ==="
	@if [ -z "$$TAILSCALE_CLIENT_ID" ] || [ -z "$$TAILSCALE_CLIENT_SECRET" ]; then \
		echo "TAILSCALE_CLIENT_ID or TAILSCALE_CLIENT_SECRET not set - skipping Tailscale cleanup"; \
		echo "To clean Tailscale devices, set both env vars and re-run"; \
	else \
		echo "Getting OAuth access token..."; \
		TOKEN=$$(curl -s -u "$$TAILSCALE_CLIENT_ID:$$TAILSCALE_CLIENT_SECRET" \
			-d "grant_type=client_credentials" \
			"https://api.tailscale.com/api/v2/oauth/token" | jq -r '.access_token'); \
		if [ "$$TOKEN" = "null" ] || [ -z "$$TOKEN" ]; then \
			echo "Failed to get access token"; \
		else \
			echo "Fetching stargate devices from Tailscale..."; \
			DEVICES=$$(curl -s -H "Authorization: Bearer $$TOKEN" \
				"https://api.tailscale.com/api/v2/tailnet/-/devices" | \
				jq -r '.devices[]? | select(.hostname | startswith("stargate")) | .id' 2>/dev/null); \
			if [ -z "$$DEVICES" ]; then \
				echo "No stargate devices found"; \
			else \
				for DEVICE_ID in $$DEVICES; do \
					echo "Deleting Tailscale device: $$DEVICE_ID"; \
					curl -s -X DELETE -H "Authorization: Bearer $$TOKEN" \
						"https://api.tailscale.com/api/v2/device/$$DEVICE_ID" > /dev/null || true; \
				done; \
			fi; \
			echo "Tailscale cleanup complete"; \
		fi; \
	fi

## Delete all stargate Azure resource groups
clean-azure:
	@echo "=== Deleting Azure resource groups ==="
	@RGS=$$(az group list --query "[?starts_with(name, 'stargate-vapa')].name" -o tsv 2>/dev/null); \
	if [ -z "$$RGS" ]; then \
		echo "No stargate-vapa-* resource groups found"; \
	else \
		for RG in $$RGS; do \
			echo "Deleting resource group: $$RG"; \
			az group delete --name "$$RG" --yes --no-wait || true; \
		done; \
		echo "Azure resource group deletion initiated (running in background)"; \
	fi

## Clean up local processes and QEMU resources
clean-local:
	@echo "=== Cleaning local resources ==="
	@echo "Stopping controller..."
	-pkill -f "bin/azure-controller" 2>/dev/null || true
	-pkill -f "bin/qemu-controller" 2>/dev/null || true
	-sudo pkill -f "bin/qemu-controller" 2>/dev/null || true
	@echo "Stopping simulator processes..."
	-pkill -f "bin/simulator" 2>/dev/null || true
	-sudo pkill -f "bin/simulator" 2>/dev/null || true
	@echo "Killing QEMU VMs..."
	-sudo pkill -f "qemu-system-x86_64.*sim-worker" 2>/dev/null || true
	-sudo pkill -f "qemu-system-x86_64.*stargate-qemu" 2>/dev/null || true
	@echo "Cleaning up VM storage..."
	-sudo rm -rf /var/lib/stargate/vms/ 2>/dev/null || true
	@echo "Cleaning up tap devices..."
	-sudo ip link delete tap-sim-worker- 2>/dev/null || true
	@echo "Cleaning up bridge..."
	-sudo ip link set stargate-br0 down 2>/dev/null || true
	-sudo ip link delete stargate-br0 2>/dev/null || true
	@echo "Cleaning up demo files..."
	-rm -rf /tmp/stargate-demo 2>/dev/null || true
	-rm -f /tmp/stargate-controller.log 2>/dev/null || true

## Help

help:
	@echo "Available targets:"
	@echo "  build           - Build all binaries"
	@echo "  azure-controller - Build the Azure controller"
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
	@echo "  clean-all       - Full cleanup: Kind cluster, Azure RGs, Tailscale, local"
	@echo "  clean-kind      - Delete local Kind cluster"
	@echo "  clean-azure     - Delete all stargate-vapa-* Azure resource groups"
	@echo "  clean-tailscale - Remove stargate-azure-* devices from Tailscale (needs TAILSCALE_CLIENT_ID/SECRET)"
	@echo "  clean-local     - Stop controllers, kill QEMU VMs, clean up network/resources"
