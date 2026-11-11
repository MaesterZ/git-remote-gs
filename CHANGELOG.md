# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `--force-with-lease`: the helper answers git's `option cas` and checks the lease under the ref's lock, so a lease
  push can't overwrite a push that landed after git listed the refs.
- Signed build provenance for release archives and binaries, verifiable with `gh attestation verify`.
- Progress on push (`Creating bundle: …, done.`, then `Uploading: 54% (…) | …/s`) and fetch/clone (`Downloading: …`), shown when git asks for progress
  (a terminal, or `--progress`).

### Fixed

- A `--force-with-lease` push that rewrites history was always refused as non-fast-forward: git sends it unforced,
  expecting the helper to honour the lease.
- Non-fast-forward rejections found by the helper are reported as `non-fast forward`, the spelling git maps to its
  own `[rejected]` status and `git pull` hint, instead of a `[remote rejected]` one.

## [1.0.0] - 2026-11-03

First version: [`git-remote-s3`](https://github.com/awslabs/git-remote-s3) brought to Google Cloud Storage as a single
Go binary, following its storage layout so a repo copied from S3 to GCS works as is. Bundles and `HEAD` are also stored
as [`git-remote-gcs`](https://github.com/Vicguin65/git-remote-gcs) stores them.

### Added

- `git-remote-gs`, a remote helper for `gs://BUCKET/PREFIX[/...]` URLs supporting `capabilities`, `option verbosity`,
  `list`, `list for-push`, `fetch` and `push`, including force pushes, ref deletion and tags.
- Authentication through Application Default Credentials only, with one-line errors for missing credentials and for
  403s naming the bucket and the missing IAM permission.
- Per-ref push lock (`<ref>/LOCK#.lock`) created with `ifGenerationMatch=0` and taken over once older than
  `GIT_REMOTE_GS_LOCK_TTL` seconds (default 60) by its object's server time. While a push holds it, the lock is
  rewritten every third of the TTL, so a long upload isn't taken over; a push whose lock is taken over anyway aborts its
  upload before committing it. The lock is released by a delete conditioned on its generation.
- Protected branches: a `PROTECTED#` object under a ref, as `git-s3 protect` writes it, makes the ref accept only
  fast-forwards, even when forced, and refuse deletion.
- Up to 4 refs pushed and 4 bundles downloaded in parallel. Fetch takes branches first and skips refs whose objects
  they brought; bundles of 64 MiB or more download as parallel 16 MiB ranged reads, pinned to the listed generation.
- `GIT_REMOTE_GS_VERBOSE`, set to any non-empty value, logs every step to stderr.
- Unit tests against an in-memory store with generations and preconditions; end-to-end tests that drive real `git`
  against an in-process fake-gcs-server; an optional store contract and round trip on a real bucket
  (`make test-bucket BUCKET=gs://my-bucket`).
- `Makefile`, golangci-lint configuration, CI (tests on Linux and macOS with `-race`, lint, static cross-builds) and a
  GoReleaser release workflow for linux and darwin, amd64 and arm64.
- Builds use the storage client's `disable_grpc_modules` tag to leave out its unused gRPC transport: release binaries
  are about 20 MB instead of 32 MB.
- `README.md`, `TLDR.md` (a one-page quick start), an Apache-2.0 `LICENSE.md`, and `NOTICE.md` carrying the attribution
  notices of `git-remote-s3` and `git-remote-gcs`; release archives include them.

### Changed from `git-remote-s3`

- The scheme is `gs://`, matching `gcloud storage`, and the binary is `git-remote-gs`.
- Locks hold JSON naming their holder, shown when a push is blocked; `git-remote-s3`'s are empty.
- Taking over a stale lock and releasing a lock are conditioned on the lock's generation, so a client never deletes a
  lock another one just took.
- New bundles and `HEAD` are written with a `DoesNotExist` precondition; the old bundle is deleted only after the
  upload, and a push that finds it already gone reports a concurrent change instead of `ok`.
- Deletes take the ref's lock too, and refuse the branch the remote `HEAD` points at and protected branches.
- Pushing a raw SHA (`git push origin <sha>:refs/heads/x`) works: the bundle is built from a temporary ref.
- A fetch whose bundle fails `git bundle verify` or `git bundle unbundle` fails instead of being ignored.
- Rejected pushes report `non-fast-forward` or `fetch first`, as git does for other remotes.
- Bundles are downloaded into private temporary files (mode 0600).
- Pushes from SHA-256 repositories are refused instead of uploading a bundle `list` never shows.
- A clone with `--filter` works, as a full clone: the helper's git commands never lazy-fetch through it again.
- Commands in error messages quote `gs://` URLs, which contain `#` for locks: zsh's `extended_glob` treats it as a glob.

### Not included

- Git LFS
- The `+zip` scheme and CodePipeline archives
- AWS profiles
- The `git-s3` management CLI (`doctor`, `protect`, `unprotect`, `delete-branch`): error messages print the
  `gcloud storage` command that fixes the problem

[Unreleased]: https://github.com/MaesterZ/git-remote-gs/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/MaesterZ/git-remote-gs/releases/tag/v1.0.0
