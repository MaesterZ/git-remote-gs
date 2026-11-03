# git-remote-gs

> In a hurry? [TLDR.md](TLDR.md) is a one-page quick start.

A [git remote helper](https://git-scm.com/docs/gitremote-helpers) that uses a Google Cloud Storage bucket as a git
remote. It needs no server, VM or Secure Source Manager: the bucket alone is the remote.

```console
git remote add origin gs://my-bucket/my-repo
git push -u origin main
git clone gs://my-bucket/my-repo
```

It ships as one static Go binary for Linux and macOS, and needs only `git` at runtime. It brings
[`awslabs/git-remote-s3`](https://github.com/awslabs/git-remote-s3) to GCS and follows its storage layout, so a repo
copied from S3 to GCS works as is. It started as a Go rewrite of
[`git-remote-gcs`](https://github.com/Vicguin65/git-remote-gcs), which stores bundles and `HEAD` the same way.

---

## How it works

When a binary named `git-remote-<scheme>` is on the `PATH`, git hands it every `<scheme>://` URL: for `gs://` URLs it
runs `git-remote-gs <remote-name> <url>` and talks to it over stdin and stdout.

Each ref is stored as one full [`git bundle`](https://git-scm.com/docs/git-bundle) of its history:

- `git push` bundles the ref locally, takes a per-ref lock in the bucket, uploads the new bundle, then deletes the old
  one. Several refs (e.g. `git push --tags`) are pushed 4 at a time.
- `git fetch` and `git clone` download the bundles of the refs they need (4 at a time), verify them, and unbundle them
  into the local repository. Branches come first, so a tag or merged branch whose commit they already brought isn't
  downloaded at all. Bundles of 64 MiB or more are downloaded as parallel ranged reads of 16 MiB.

---

## Requirements

- `git` 2.30 or later, for `git rev-parse --end-of-options`
- A GCS bucket, and Google credentials that can read and write objects in it

Building from source needs Go **1.27+**.

---

## Installation

### Using `go install`

```console
go install -trimpath -tags disable_grpc_modules github.com/MaesterZ/git-remote-gs@latest
```

The binary lands in `$(go env GOPATH)/bin` (usually `~/go/bin`). Make sure that directory is on your `PATH`.

### From a pre-built release binary

To install the latest [release](https://github.com/MaesterZ/git-remote-gs/releases) for your platform into `/usr/local/bin`:

```console
sudo mkdir -pv /usr/local/bin
curl -fsSL "https://github.com/MaesterZ/git-remote-gs/releases/latest/download/git-remote-gs-$(uname -s)-$(uname -m).tar.gz" \
  | sudo tar -xzv --no-same-owner -C /usr/local/bin git-remote-gs
git-remote-gs --version
```

### Build from source

```console
git clone https://github.com/MaesterZ/git-remote-gs
cd git-remote-gs
make release        # ./git-remote-gs for this machine
make cross          # dist/git-remote-gs-linux-amd64 and dist/git-remote-gs-darwin-arm64
```

Check git finds it with `git-remote-gs --version`.

---

## Authentication

`git-remote-gs` uses [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials)
through the official Go client. On a workstation, log in once:

```console
gcloud auth application-default login
```

That's the only setup. `gcloud` is not needed at runtime, and there are no keys or config files to manage: anything
ADC supports works, including `GOOGLE_APPLICATION_CREDENTIALS` and the metadata server on Google Cloud VMs.

### IAM permissions

| Operation                  | Permissions                                                                     |
| -------------------------- | ------------------------------------------------------------------------------- |
| `clone`, `fetch`, `pull`   | `storage.objects.list`, `storage.objects.get`                                   |
| `push`, including deletes  | `storage.objects.list`, `storage.objects.get`, `storage.objects.create`, `storage.objects.delete` |

`roles/storage.objectUser` on the bucket covers everything; `roles/storage.objectViewer` is enough for read-only use.
The helper never creates or deletes buckets.

---

## URL format

```
gs://BUCKET/PREFIX[/...]
```

- `BUCKET` follows the [GCS naming rules](https://cloud.google.com/storage/docs/buckets#naming).
- `PREFIX` is required and can have several parts, so one bucket can hold many repos. The bucket root can't hold a
  repo: `gs://my-bucket` is rejected.
- A trailing slash is ignored: `gs://my-bucket/my-repo/` is the same remote as `gs://my-bucket/my-repo`.

---

## Storage layout

```
<prefix>/HEAD                    text, e.g. "refs/heads/main", written on the first branch push
<prefix>/<ref>/<sha>.bundle      full git bundle of <ref> at <sha>; exactly one per ref
<prefix>/<ref>/LOCK#.lock        push lock: {"timestamp": <unix seconds>, "host": ..., "pid": ...}
<prefix>/<ref>/PROTECTED#        optional, empty: the ref is protected
```

`<ref>` is the full ref name, e.g. `refs/heads/main` or `refs/tags/v1`. The bucket listing is the source of truth: a
ref exists exactly when its bundle exists, and objects that aren't `<40 hex>.bundle` are ignored. The names match
`git-remote-s3`'s; its locks are empty, and the JSON in ours only names the holder in error messages.

Quote object URLs that contain `#` in shell commands: with zsh's `extended_glob`, an unquoted `#` is a glob operator.

The remote `HEAD` (the branch a clone checks out) is set by the first branch push and never moved afterwards. To
change it, overwrite the object, e.g. `echo -n refs/heads/dev | gcloud storage cp - gs://my-bucket/my-repo/HEAD`.

## Locking

A push to a ref holds that ref's `LOCK#.lock`, so two pushes to one ref can't interleave. Pushes to different refs don't
block each other.

1. The lock is created only if it doesn't exist (`ifGenerationMatch=0`).
2. If a lock exists and is older than `GIT_REMOTE_GS_LOCK_TTL` seconds (default 60), its holder is presumed dead: the
   lock is deleted if it is still the same generation, and the create is retried once. Like `git-remote-s3`, the age
   comes from the object's time on the server, not from its content.
3. With the lock held, the helper lists the ref again. If it changed since the push started, the push is refused
   (`stale remote`). Otherwise the new bundle is uploaded, and only then is the old one deleted, so the bucket always
   holds a complete copy.
4. While the upload runs, the lock is rewritten every third of the TTL, each time conditioned on the generation the
   helper holds, so a long upload never looks stale. If a rewrite finds the lock taken over, the
   upload is aborted before it is committed, and the push fails with `lost the lock` with the bucket unchanged.
5. The lock is always released, even on error or Ctrl-C, by a delete conditioned on the generation the helper last
   wrote. It never deletes a lock it doesn't hold.

The lock is held only while the bundle uploads (bundles are built before locking). If the old bundle is already gone
when the helper comes to delete it, a client that doesn't lock changed the ref meanwhile, and the push reports
`another push changed the ref at the same time` instead of `ok`.

## Protected branches

A ref with a `PROTECTED#` object next to its bundle is protected, as with `git-s3 protect`:

- It accepts only fast-forwards: a `git push --force` that would rewrite history is refused with
  `protected branch, force push not allowed`. A forced push that is a fast-forward goes through.
- It can't be deleted: `git push --delete` is refused with `refusing to delete a protected branch`.

To protect or unprotect a branch, write or remove the empty object:

```console
gcloud storage cp /dev/null 'gs://my-bucket/my-repo/refs/heads/main/PROTECTED#'
gcloud storage rm 'gs://my-bucket/my-repo/refs/heads/main/PROTECTED#'
```

The helper enforces protection, not GCS: anyone with `storage.objects.delete` can remove the marker or the bundles
directly, so restrict that with IAM.

## Submodules

Git runs remote helpers for submodules only when their protocol is allowed explicitly:

```console
git config --global protocol.gs.allow always
```

---

## Environment variables

| Variable                    | Default | Effect                                                                     |
| --------------------------- | ------- | -------------------------------------------------------------------------- |
| `GIT_REMOTE_GS_LOCK_TTL`    | `60`    | Seconds after which a push lock is considered stale and may be taken over |
| `GIT_REMOTE_GS_VERBOSE`     | unset   | Any non-empty value logs every step to stderr, like `git push -v`          |
| `STORAGE_EMULATOR_HOST`     | unset   | `host:port` of a GCS emulator; used by the tests                           |
| `GIT_REMOTE_GS_TEST_BUCKET` | unset   | `gs://BUCKET[/PREFIX]` for the optional real-bucket test                   |

---

## Limits

| Feature | What happens | Commands / flags |
| --- | --- | --- |
| **Full history every time** | Every push uploads the ref's full history, and every fetch of a changed ref downloads it again; each tag is a full bundle too. Fine for small and medium repos; for large ones, use a real git server. | `git push`<br>`git fetch`<br>`git clone` |
| SHA-1 repositories only | A push from a SHA-256 repository is refused: bundles are named `<40 hex>.bundle`. | `git init --object-format=sha256` |
| No [shallow clones](https://git-scm.com/docs/git-clone#Documentation/git-clone.txt---depthdepth) | Silently ignored: you get a full clone. | `--depth=<depth>`<br>`--shallow-since=<date>`<br>`--shallow-exclude=<ref>` |
| No [partial clones](https://git-scm.com/docs/partial-clone) | Ignored: every object is downloaded. | `--filter=<filter-spec>` |
| No [atomic pushes](https://git-scm.com/docs/git-push#Documentation/git-push.txt---atomic) | Git refuses with `fatal: helper gs does not support --atomic`. | `--atomic` |
| [Forced pushes with lease](https://git-scm.com/docs/git-push#Documentation/git-push.txt---force-with-lease) work, with a race | Git checks the lease against the refs it listed when the push started, not under the ref's lock, so a push landing in between can still be overwritten. | `--force-with-lease`<br>`--force-with-lease=<refname>`<br>`--force-with-lease=<refname>:<expect>` |
| No [LFS](https://git-lfs.com/) | In a repo with LFS files, `git push` fails in git-lfs's pre-push hook with `batch request: missing protocol`. | `git lfs` |

---

## Differences from `git-remote-s3`

The storage layout is the same as [`git-remote-s3`](https://github.com/awslabs/git-remote-s3)'s: bundles, `HEAD`,
`LOCK#.lock` and `PROTECTED#`. What differs is how the helper uses it.

### Improvements

- **One static binary**, with no Python or boto to install: only `git` is needed at runtime.
- **Locks can't be stolen by mistake.** Taking over a stale lock and releasing a lock are both conditioned on the lock's
  generation. `git-remote-s3` deletes the lock unconditionally, so two clients can both take over a stale lock, or one
  can release a lock another just took.
- **Long pushes keep their lock.** The lock is refreshed while the bundle uploads; with `git-remote-s3`, a push taking
  longer than the TTL can be taken over midway, leaving two bundles for one ref.
- **Writes never overwrite.** New bundles and `HEAD` are only created if absent, the old bundle is deleted only if it
  is still the one listed, and a push that finds the ref changed under it reports it instead of `ok`.
- **Deletes are safe.** Deleting a ref takes its lock, and refuses the branch the remote `HEAD` points at and protected
  branches; `git-remote-s3` deletes either without a lock.
- **Pushing a raw SHA works** (`git push origin <sha>:refs/heads/x`); `git-remote-s3` passes the SHA to
  `git bundle create`, which needs a ref name.
- **Failed fetches fail.** A bundle that is missing, invalid or can't be unbundled stops the fetch with a message naming
  the ref; `git-remote-s3` runs fetches in a thread pool and never checks their errors.
- **Faster pushes and fetches.** Refs are pushed 4 at a time, not one by one. Fetch takes branches first and skips the
  refs whose objects they brought, instead of downloading every ref's full bundle.
- **Clearer errors.** Rejected pushes say `non-fast-forward` or `fetch first`, as git does for other remotes, instead
  of `remote ref is not ancestor of ...`. A 403 names the missing IAM permission and a role that grants it, and other
  failures print their cause instead of `unknown error. Run with --verbose flag to get full log`.

### Other differences

- `gs://` URLs and Application Default Credentials, instead of `s3://[profile@]bucket` and AWS profiles.
- `GIT_REMOTE_GS_LOCK_TTL` and `GIT_REMOTE_GS_VERBOSE`, instead of `GIT_REMOTE_S3_LOCK_TTL_SECONDS` and
  `GIT_REMOTE_S3_VERBOSE`.
- Locks hold JSON naming their holder, shown when a push is blocked; `git-remote-s3`'s are empty.
- Not included: Git LFS (`git-lfs-s3`), the `s3+zip://` scheme for CodePipeline, and the `git-s3` CLI (`doctor`,
  `protect`, `unprotect`, `delete-branch`). Error messages print the `gcloud storage` command that fixes the problem,
  and the README shows how to protect a branch.

---

## Development

```console
make help         # list targets
make check        # go vet, golangci-lint and go test -race: what CI runs
make test-bucket BUCKET=gs://my-bucket          # optional round trip on a real bucket
```

- `go test ./...` needs no GCP access. End-to-end tests build the binary, run real `git` against throwaway repos, and
  serve the bucket from an in-process [fake-gcs-server](https://github.com/fsouza/fake-gcs-server).
- fake-gcs-server honours `ifGenerationMatch` on writes and generation-pinned reads, but not on deletes, so
  conditional deletes are tested against the in-memory store in `internal/store`.
- The real-bucket test writes under a unique prefix and deletes only what it wrote.

Layout:

| Path                 | Contents |
| -------------------- | -------- |
| `main.go`            | Argument parsing and wiring |
| `internal/protocol`  | The remote helper protocol: list, fetch, push, locking |
| `internal/store`     | `Store` interface, the GCS implementation, an in-memory fake, URL parsing |
| `internal/git`       | Wrappers around the `git` binary |
| `e2e`                | End-to-end tests |

---

## License

Copyright &copy; 2026 MaesterZ

Licensed under the Apache License, Version 2.0. See [LICENSE.md](LICENSE.md) for the full text.

Behaviour and storage layout derive from [`git-remote-s3`](https://github.com/awslabs/git-remote-s3) (Copyright
Amazon.com, Inc. or its affiliates) and [`git-remote-gcs`](https://github.com/Vicguin65/git-remote-gcs) (Copyright 2026
Tyler Du), both Apache-2.0; their attribution notices are in [NOTICE.md](NOTICE.md).

See [CHANGELOG.md](CHANGELOG.md) for the changes in each release.
