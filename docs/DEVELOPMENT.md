# Development

## Prerequisites

- The Go version declared in `go.mod`.
- GNU Make 4 or later.
- `golangci-lint` v2.15.0 for lint checks. Keep this version aligned with
  `GOLANGCI_LINT_VERSION` in the GitHub Actions workflow.
- A C toolchain and `CGO_ENABLED=1` to run race tests. When cgo is disabled,
  `make check` reports that race tests were skipped.

Install the pinned linter version with:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
```

Ensure the Go binary directory is on `PATH`.

## Commands

Run all repository checks:

```sh
make check
```

This checks Go formatting with the Go toolchain's `gofmt`, runs unit tests and
`go vet`, runs `golangci-lint`, and runs race-enabled tests when cgo is enabled.

To run an individual check:

```sh
make fmt-check
make test
make vet
make lint
make race
```

Use `make fmt` to format Go source files. `make race` requires cgo and a
working C toolchain; `make check` skips that target when `CGO_ENABLED=0`.

There is currently no `make init` target because the project has no dependency
or generated-file setup that requires initialization. Add one only if the
project later gains necessary setup work.

## Pull request workflow

Implement one phase per branch from `main`. Keep commits conventional and
atomic, open a pull request to `main`, and wait for review and required CI
checks before merge. Each phase should update its own relevant documentation
and README overview. See [CI](CI.md) for the GitHub check and branch protection
requirements.
