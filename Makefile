.PHONY: all build run test clean \
        clean-all clean-kind clean-azure clean-tailscale clean-local prep-dc-inventory azure azure-controller \
        proto stargate-server sgctl stargate

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod

# Binary names
PREP_DC_INVENTORY_BIN=bin/prep-dc-inventory
AZURE_BIN=bin/azure
AZURE_CONTROLLER_BIN=bin/azure-controller
STARGATE_SERVER_BIN=bin/stargate-server
SGCTL_BIN=bin/sgctl

all: build

## Build targets

build: prep-dc-inventory azure azure-controller stargate

prep-dc-inventory:
	$(GOBUILD) -o $(PREP_DC_INVENTORY_BIN) ./cmd/infra-prep/main.go

azure:
	$(GOBUILD) -o $(AZURE_BIN) ./cmd/azure/main.go

azure-controller:
	$(GOBUILD) -o $(AZURE_CONTROLLER_BIN) ./cmd/azure-controller/main.go

## Run targets

run-server:
	$(STARGATE_SERVER_BIN)

## Kubernetes targets (for route-sync controller if still needed)

## Test targets

test:
	$(GOTEST) -v ./...

## Dependency management

deps:
	$(GOMOD) download
	$(GOMOD) tidy

## Proto generation (requires buf: https://buf.build/docs/installation)

proto:
	buf generate

## Stargate gRPC server and CLI

stargate: stargate-server sgctl

stargate-server:
	$(GOBUILD) -o $(STARGATE_SERVER_BIN) ./cmd/stargate-server/main.go

sgctl:
	$(GOBUILD) -o $(SGCTL_BIN) ./cmd/sgctl/main.go

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
	@echo "Stopping stargate-server..."
	-pkill -f "bin/stargate-server" 2>/dev/null || true
	@echo "Cleaning up demo files..."
	-rm -rf /tmp/stargate-demo 2>/dev/null || true
	-rm -f /tmp/stargate-server.log 2>/dev/null || true

## Help

help:
	@echo "Available targets:"
	@echo "  build           - Build all binaries"
	@echo "  stargate-server - Build the Stargate gRPC server"
	@echo "  sgctl           - Build the Stargate CLI"
	@echo "  run-server      - Run the Stargate server"
	@echo "  test            - Run tests"
	@echo "  deps            - Download and tidy dependencies"
	@echo "  proto           - Generate Go code from proto files (requires buf)"
	@echo "  clean           - Remove built binaries"
	@echo "  clean-all       - Full cleanup: Kind cluster, Azure RGs, Tailscale, local"
	@echo "  clean-kind      - Delete local Kind cluster"
	@echo "  clean-azure     - Delete all stargate-vapa-* Azure resource groups"
	@echo "  clean-tailscale - Remove stargate-azure-* devices from Tailscale (needs TAILSCALE_CLIENT_ID/SECRET)"
	@echo "  clean-local     - Stop server, clean up network/resources"
