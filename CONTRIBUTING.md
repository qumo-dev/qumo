# Contributing to qumo

Thank you for your interest in contributing to qumo! This document provides guidelines and instructions for contributing.

## Code of Conduct

This project adheres to a Code of Conduct that all contributors are expected to follow. Please read [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) before contributing.

## Getting Started

### Prerequisites

- Go 1.27 or higher
- Git
- Basic understanding of QUIC protocol and media streaming

### Setting Up Your Development Environment

1. Fork the repository
2. Clone your fork:
   ```bash
   git clone https://github.com/YOUR_USERNAME/qumo.git
   cd qumo
   ```
3. Add the upstream repository:
   ```bash
   git remote add upstream https://github.com/qumo-dev/qumo.git
   ```
4. Install dependencies:
   ```bash
   go mod download
   ```

## Development Workflow

### Creating a Branch

Create a feature branch for your work:

```bash
git checkout -b feature/your-feature-name
```

Use descriptive branch names:
- `feature/` for new features
- `fix/` for bug fixes
- `docs/` for documentation changes
- `refactor/` for code refactoring

### Making Changes

1. Make your changes in logical commits
2. Write clear, descriptive commit messages
3. Follow the existing code style
4. Add or update tests as needed
5. Update documentation as needed

### Commit Message Format

We follow the conventional commits specification:

```
<type>(<scope>): <subject>

<body>

<footer>
```

Types:
- `feat`: A new feature
- `fix`: A bug fix
- `docs`: Documentation changes
- `style`: Code style changes (formatting, etc.)
- `refactor`: Code refactoring
- `test`: Adding or updating tests
- `chore`: Maintenance tasks

Example:
```
feat(relay): add connection pooling support

Implement connection pooling to improve performance
when handling multiple concurrent streams.

Closes #123
```

### Testing

Run tests before submitting:

```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -coverprofile=coverage.out ./...

# Run tests with race detector
go test -race ./...
```

Integration tests run real QUIC servers. They are behind the `integration` build tag:

```bash
go test -tags=integration ./internal/relay/... ./internal/integration/...
```

Where an integration test lives depends on what it checks:

- **The contract, through the public API** (`internal/integration`): what a client or an embedding application relies on. Examples are session admission, subscribe authorization, and RTMP/RTSP interop. These tests build components from exported fields and functions only, as a user would.
- **Internals** (inside the package, for example `internal/relay`): white-box benchmarks and stress tests that measure or drive unexported state, such as route election, the relay-chain harness and peer discovery.

When in doubt, prefer `internal/integration`. A test that needs an unexported symbol to check the contract usually means the contract isn't exposed yet.

### Code Style

- Follow standard Go conventions and idioms
- Use `gofmt` to format your code
- Run `golangci-lint` to check for common issues:
  ```bash
  golangci-lint run
  ```

## Submitting Changes

### Pull Request Process

1. Update your fork with the latest upstream changes:
   ```bash
   git fetch upstream
   git rebase upstream/main
   ```

2. Push your changes to your fork:
   ```bash
   git push origin feature/your-feature-name
   ```

3. Create a Pull Request from your fork to the main repository

4. Fill out the PR template completely

5. Wait for review and address any feedback

### PR Requirements

- All tests must pass
- Code coverage should not decrease
- Code must pass linting checks
- Documentation must be updated if needed
- Commits should be clear and well-organized

## Reporting Bugs

Use the bug report issue template and include:
- Clear description of the bug
- Steps to reproduce
- Expected vs actual behavior
- Version information
- Logs and stack traces if applicable

## Requesting Features

Use the feature request issue template and include:
- Clear description of the feature
- Use case and motivation
- Proposed implementation (if any)

## Discussing Specifications

For protocol or specification discussions, use the spec discussion template.

## Code Review Process

- Maintainers will review PRs as time permits
- Reviews may request changes or improvements
- Once approved, a maintainer will merge the PR

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.

## Questions?

Feel free to:
- Open a discussion in GitHub Discussions
- Ask questions in your PR
- Reach out to maintainers

Thank you for contributing to qumo!
