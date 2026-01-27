# Testing mx-azure

This document describes how to run tests for the `mx-azure` CLI.

## Quick Start

```bash
# Run all unit tests (fast, no Azure calls)
go test ./cmd/mx-azure/...

# Run with verbose output
go test -v ./cmd/mx-azure/...

# Run integration tests (requires Azure credentials)
go test -tags=integration -v ./cmd/mx-azure/...
```

## Unit Tests

Unit tests run without any external dependencies and complete in milliseconds.

```bash
# Run all unit tests
go test ./cmd/mx-azure/...

# Run specific package tests
go test ./cmd/mx-azure/                         # CLI argument parsing
go test ./cmd/mx-azure/internal/azure/          # Builder functions
go test ./cmd/mx-azure/internal/cloudinit/      # Cloud-init templates
go test ./cmd/mx-azure/internal/config/         # Config validation

# Run with coverage
go test -cover ./cmd/mx-azure/...

# Generate coverage report
go test -coverprofile=coverage.out ./cmd/mx-azure/...
go tool cover -html=coverage.out -o coverage.html
```

### Test Summary

| Package | Tests | Description |
|---------|-------|-------------|
| `cmd/mx-azure` | 9 | CLI argument parsing, flag defaults |
| `internal/azure` | 12 | Builder functions (NSG, VM, NIC, etc.) |
| `internal/cloudinit` | 7 | Cloud-init template rendering |
| `internal/config` | 22 | Config validation (GUID, SSH keys, etc.) |

## Integration Tests

Integration tests make real Azure API calls. They require:

1. Azure credentials (via `az login` or environment variables)
2. Environment variables for test configuration

### Required Environment Variables

```bash
export AZURE_SUBSCRIPTION_ID="your-subscription-id"
export AZURE_LOCATION="canadacentral"
export MX_RG_PREFIX="mx-inttest"
export MX_SSH_PUBLIC_KEY="$(cat ~/.ssh/id_rsa.pub)"
export MX_TAILSCALE_AUTH_KEY="tskey-auth-..."
```

### Running Integration Tests

```bash
# Run all integration tests
go test -tags=integration -v ./cmd/mx-azure/internal/azure/...

# Run specific integration test
go test -tags=integration -v -run TestIntegration_DeleteResourceGroup ./cmd/mx-azure/internal/azure/...

# Run with timeout (provisioning can take 10+ minutes)
go test -tags=integration -v -timeout=20m ./cmd/mx-azure/internal/azure/...
```

### Integration Test Summary

| Test | Duration | Description |
|------|----------|-------------|
| `TestIntegration_ClientCreation` | ~1s | Verifies Azure client creation |
| `TestIntegration_ListResourceGroups` | ~2s | Lists RGs to verify credentials |
| `TestIntegration_GetStatus` | ~1s | Tests status for non-existent resources |
| `TestIntegration_DeleteNonExistentResourceGroup` | ~1s | Verifies delete is idempotent |
| `TestIntegration_DeleteResourceGroup` | ~2min | Creates and deletes an RG |
| `TestIntegration_ProvisionAndVerify` | ~10min | Full provisioning test (expensive) |

### Cleanup

Integration tests clean up after themselves by deleting resource groups in `defer` blocks. If a test is interrupted, you may need to manually clean up:

```bash
# List test resource groups
az group list --query "[?starts_with(name, 'mx-inttest')].name" -o tsv

# Delete leftover test resource groups
az group delete --name mx-inttest-12345 --yes --no-wait
```

## Running All Tests

```bash
# Unit tests only (CI-friendly, fast)
go test ./cmd/mx-azure/...

# All tests including integration (requires Azure)
go test -tags=integration -v -timeout=30m ./cmd/mx-azure/...
```

## CI/CD Integration

For CI pipelines, run unit tests on every PR:

```yaml
# GitHub Actions example
- name: Run unit tests
  run: go test -v ./cmd/mx-azure/...
```

For integration tests, run on merge to main with Azure credentials:

```yaml
- name: Run integration tests
  env:
    AZURE_SUBSCRIPTION_ID: ${{ secrets.AZURE_SUBSCRIPTION_ID }}
    AZURE_LOCATION: canadacentral
    MX_RG_PREFIX: mx-ci-test
    MX_SSH_PUBLIC_KEY: ${{ secrets.SSH_PUBLIC_KEY }}
    MX_TAILSCALE_AUTH_KEY: ${{ secrets.TAILSCALE_AUTH_KEY }}
  run: go test -tags=integration -v -timeout=30m ./cmd/mx-azure/...
```

## Troubleshooting

### Tests Skip with "missing required env vars"

Integration tests skip gracefully when environment variables are not set:

```
--- SKIP: TestIntegration_ProvisionAndVerify (0.00s)
    integration_test.go:76: Skipping: missing required env vars: [MX_SSH_PUBLIC_KEY]
```

Set the required environment variables to run the test.

### Azure Authentication Errors

If you see `MissingSubscription` errors:

```bash
# Verify you're logged in
az account show

# Re-authenticate if needed
az login

# Verify subscription ID is correct
echo $AZURE_SUBSCRIPTION_ID
```

### Test Timeout

The full provisioning test can take 10+ minutes. Increase timeout:

```bash
go test -tags=integration -v -timeout=20m ./cmd/mx-azure/...
```
