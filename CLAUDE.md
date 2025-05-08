# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build, Lint, and Test Commands
- Build: `make build`
- Lint: `make lint` (uses golangci-lint)
- Update Dependencies: `make update-deps`
- Add Dependency: `make add-dep`
- Test: `go test ./...`
- Test with Verbose Output: `go test -v ./...`
- Run Single Test: `go test -v ./path/to/package -run TestName`

## Project Structure
- `cmd/baton-grafana/` - Main application entry point and configuration
- `pkg/connector/` - Core connector implementation integrating with Baton SDK
- `pkg/grafana/` - Grafana API client implementation

## Key Components
- **Grafana Client** (`pkg/grafana/client.go`): Handles all API interactions with the Grafana service
- **Connector** (`pkg/connector/connector.go`): Implements the Baton connector interface
- **Resource Types** (`pkg/connector/resource_types.go`): Defines the Grafana resource types (Organizations and Users)
- **Resource Builders**: `pkg/connector/organizations.go` and `pkg/connector/users.go` implement sync logic for each resource type

## Code Style Guidelines
- **Imports**: Group standard library, external, and internal imports with a blank line between groups
- **Error Handling**: Return errors with context using `fmt.Errorf("grafana-connector: %w", err)`
- **Naming**: Use camelCase for variables, PascalCase for exported functions/types
- **Documentation**: All exported functions/types must have doc comments
- **Client Implementation**: Implement API clients with proper error handling and context

## Testing
- Use the baton-sdk testing framework with defined test cases
- Create separate test files alongside implementation files

## Configuration
The connector requires:
- `hostname`: Grafana server URL (default: http://localhost:3000)
- `username`: Grafana username (required)
- `password`: Grafana password (required)

These can be provided via command-line flags or environment variables (BATON_HOSTNAME, BATON_USERNAME, BATON_PASSWORD)