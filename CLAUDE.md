# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build, Lint, and Test Commands
- Build: `make build`
- Lint: `make lint` (uses golangci-lint)
- Update Dependencies: `make update-deps`
- Add Dependency: `make add-dep`
- Test: `go test ./...`
- Run Single Test: `go test -v ./path/to/package -run TestName`

## Code Style Guidelines
- **Imports**: Group standard library, external, and internal imports with a blank line between groups
- **Error Handling**: Return errors with context using `fmt.Errorf("grafana-connector: %w", err)`
- **Naming**: Use camelCase for variables, PascalCase for exported functions/types
- **Documentation**: All exported functions/types must have doc comments
- **Resource Types**: Define resource types in `resource_types.go`
- **Client Implementation**: Implement API clients with proper error handling and context
- **Testing**: Use the baton-sdk testing framework with defined test cases