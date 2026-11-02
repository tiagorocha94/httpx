# Continuous integration

The `Go` GitHub Actions workflow runs `make check` for pushes to `main` and
pull requests targeting `main`. It installs the Go version from `go.mod` and
the pinned `golangci-lint` version before running the same checks contributors
run locally.

The workflow pins its runner to `ubuntu-26.04` so a change to GitHub's
`ubuntu-latest` alias cannot silently change the CI operating system. It also
pins GitHub Actions to full commit SHAs and records their corresponding
versions inline. `setup-go` caches against `go.mod`, which exists even when
the project has no `go.sum`.

Dependabot checks GitHub Actions and Go module dependencies weekly and opens
update pull requests. Review those updates, run the required checks, and merge
them through the normal protected-branch process. When changing the linter,
update `GOLANGCI_LINT_VERSION` in the workflow and the pinned version in
`docs/DEVELOPMENT.md` together. Review the pinned Ubuntu image periodically
and update it deliberately when a newer supported runner is appropriate.

The `check` job covers:

- Go source formatting.
- Unit tests.
- `go vet`.
- `golangci-lint`.
- Race-enabled tests on the Ubuntu CI runner.

## Protecting `main`

Workflow files define checks but cannot enable repository branch protection.
An administrator should configure a branch ruleset or branch protection rule
for `main` in GitHub:

1. Require pull requests before merging.
2. Require at least one approving review.
3. Require the `Go / check` status check.
4. Require the branch to be up to date before merging, if compatible with the
   repository's merge workflow.
5. Require conversation resolution and disallow bypassing the rules where the
   repository's policy allows.

Do not merge a phase until its pull request is reviewed and accepted and the
required checks pass. Confirm the exact status-check name in GitHub after the
workflow's first run.
