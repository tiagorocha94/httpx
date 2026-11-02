# HTTPX

HTTPX is a Go library for reliable, observable outbound HTTP communication,
built on the standard library's `net/http` package.

The goal is to provide a small, composable foundation for configuring HTTP
clients, middleware, resilience policies, and observability—while preserving
standard Go request and response behavior.

## Status

HTTPX is in its initial development stage. The repository does not yet provide
a usable client API. Features described here are project goals, not available
functionality.

## Intended scope

The library is intended to provide:

- A configurable HTTP client compatible with Go's `net/http` conventions.
- Composable middleware and request lifecycle hooks.
- Opt-in resilience policies such as retries, circuit breaking, rate limiting,
  and concurrency limits.
- Structured logging and optional metrics and tracing integrations.

Business logic, domain-specific API clients, serialization frameworks,
application-specific authentication, service discovery, and caching are out of
scope.

## Quickstart

A working quickstart will be added when the initial client API is implemented.
There is no supported usage example yet.

## Development

The project's development and CI workflow is being established. Once available,
the documented local checks will be kept aligned with the checks run for GitHub
pull requests.
