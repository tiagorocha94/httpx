# Project Guidelines

## Project scope

HTTPX is intended to become a small, composable Go HTTP infrastructure library
built on `net/http`. Its scope is reusable outbound HTTP execution, transport
configuration, middleware, resilience, and observability.

Keep business logic, domain-specific API clients, serialization frameworks,
application-specific authentication, service discovery, caching, and
application lifecycle management out of this library. Do not silently add
retries or otherwise change request semantics.

## Phase workflow

- Implement one planned phase per branch from `main`. Use a descriptive branch
  name such as `phase-04-retry`.
- Keep each commit conventional and atomic. Use Conventional Commit messages
  such as `feat(retry): add bounded backoff`.
- Open a pull request to `main` for each phase. A phase must be reviewed and
  explicitly accepted before merge; do not merge merely because CI is green.
- Do not begin a dependent phase until its prerequisite is merged.
- Each phase must include focused tests, documentation of its own behavior and
  decisions, and evidence of validation in its pull request.

## Documentation

- Keep `README.md` focused on the project goal, implemented capabilities, and a
  concise quickstart. Never describe planned behavior as available.
- Put detailed component contracts, design decisions, and development guidance
  in `docs/`.
- Update the relevant `README.md` and detailed docs in the same phase as the
  behavior they describe.

## Local development commands

- Use a `Makefile` when it meaningfully simplifies repeatable local development
  and validation tasks.
- If setup or initialization is needed, expose it as `make init` and document
  what it changes. Do not add an initialization target that performs no
  necessary work.
- Provide a `make check` target for the project's complete local checks,
  including tests, vetting, formatting, and the configured Go linter when one
  is adopted.
- Keep Make targets and GitHub Actions checks aligned so contributors can run
  the same validations locally and in CI. Document required tools and targets
  in the README or `docs/DEVELOPMENT.md`.

## Go development and validation

- Prefer the Go standard library and existing project patterns. Keep public
  APIs small, explicit, and compatible with `net/http` conventions.
- Preserve request context, response-body ownership, and clear resource
  cleanup. Shared components must be safe for concurrent use.
- Add or update unit tests for observable behavior, failure cases, and
  concurrency-sensitive code.
- Before requesting review, run `make check` when available. It should include
  formatting checks, `go test ./...`, `go vet ./...`, the configured linter
  when adopted, and `go test -race ./...` where supported. Until that target
  exists, run the applicable checks directly and report unavailable or failing
  checks explicitly.

## GitHub CI and review

- GitHub Actions must validate pull requests targeting `main` with the
  repository's documented formatting, test, vet, and race checks.
- Do not merge a phase until its required checks pass and reviewers accept it.
- Configure GitHub branch protection or rulesets so required CI checks and
  review are enforced for `main`; document any required repository settings
  that cannot be represented in workflow files.
