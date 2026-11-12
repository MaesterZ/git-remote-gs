# Contributing to git-remote-gs

Thanks for taking the time to contribute. This guide covers how to report problems, set up a development environment, and get a change merged.

## Reporting bugs and requesting features

Search the [open and recently closed issues](https://github.com/MaesterZ/git-remote-gs/issues?q=is%3Aissue) first. A useful bug report includes:

- The exact command you ran and the output, with `git push -v` or `GIT_REMOTE_GS_VERBOSE=1` if you can.
- `git-remote-gs --version`, your `git --version`, and your operating system.
- Whether you changed anything locally, such as a patched binary or a `git config` override.

Remove bucket names, object paths and credentials from logs before posting them.

For a feature, describe the problem it solves before the solution. Open an issue before you start a large change, so we can agree on the approach before you spend time on it.

Issues labelled `help wanted` are good starting points.

## Development setup

You need Go 1.27 or later (see `go.mod`) and `git`. Google Cloud credentials are not needed for the default checks.

```bash
git clone https://github.com/MaesterZ/git-remote-gs.git
cd git-remote-gs
make check   # go vet, golangci-lint and the race-enabled tests: what CI runs
```

Run `make help` for the other targets. Two details matter:

- The Makefile builds with `-tags disable_grpc_modules`, which keeps the storage client's unused gRPC transport out of the binary. Use `make` or pass the tag yourself, or your build will differ from the release.
- The end-to-end tests in `e2e/` build the binary, run real `git` commands against an in-process fake GCS server, and need no GCP access.

To run the store contract and a round trip against a real bucket, set up Application Default Credentials (`gcloud auth application-default login`) and run `make test-bucket BUCKET=gs://your-bucket`. The test writes under a unique prefix and deletes only what it wrote.

## Making a change

1. Branch from the latest `main`.
2. Keep the change focused. One logical change per pull request is easier to review than several.
3. Add or update tests for behaviour you change. The unit tests live beside the code, and the end-to-end tests live in `e2e/`.
4. Run `make check` and `make format` before you push.
5. Update the docs when behaviour changes: `README.md` for users and `TLDR.md` for the short version.
6. Add a line under `## [Unreleased]` in `CHANGELOG.md` for anything users would notice.
7. Open a pull request against `main`.

### GitHub Actions lockfile

Workflows pin actions by tag, and `.github/workflows/actions.lock` records the commit behind each tag. After you add or change a `uses:` line, run `gh actions-lock` (install it with `gh extension install github/gh-actions-lock --pin v0.4.1`) and commit the updated lockfile. The `verify-lockfile` workflow fails if the lockfile is out of date. Its `verify` check is required to merge. Run `gh actions-lock --verify-local` for the same check offline.

### Commit messages

Use a short, imperative subject line that says what the change does, such as `Add progress meters for push`. Add a body only when the reason for the change is not obvious from the diff.

Pull requests are squash-merged, so the pull request title becomes the commit message on `main`. Write the title as you would want it in the history.

### Pull requests

A pull request can merge into `main` only when all of these hold:

- The required checks pass: `build`, `lint`, `test (ubuntu-latest)`, `test (macos-latest)` and `verify`.
- The branch is up to date with `main`.
- A code owner (see `.github/CODEOWNERS`) has approved the latest commit.
- Every review conversation is resolved.

GitHub does not let an author approve their own pull request. While the sole code owner is also the usual author, they merge their own pull requests with an admin override, which the branch protection allows.

The history on `main` is linear. Force pushes and branch deletion on `main` are blocked.

Reviewers look for correctness first, then tests, then clarity. A reviewer may ask you to split a large change; that is normal.

## Releases

Releases are cut by pushing a tag that matches `v*.*.*`. The `goreleaser` workflow builds the binaries, signs their build provenance, and publishes the archives. Maintainers only.

## Licensing

git-remote-gs is licensed under the Apache License 2.0 (see [LICENSE.md](LICENSE.md)). By contributing, you agree that your contribution is licensed under the same terms. If your change adds a dependency with its own notice requirements, update [NOTICE.md](NOTICE.md).

## Security issues

Do not open a public issue for a security problem. Report it privately through GitHub's [private vulnerability reporting](https://github.com/MaesterZ/git-remote-gs/security/advisories/new), which is available on the repository's Security tab.
