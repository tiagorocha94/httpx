# Continuous integration

The `Go` GitHub Actions workflow runs `make check` for pushes to `main` and
pull requests targeting `main`. It installs the Go version from `go.mod` and
the pinned `golangci-lint` version before running the same checks contributors
run locally.

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
