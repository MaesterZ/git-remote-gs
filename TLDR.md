# TL;DR

`git-remote-gs` lets git push to and clone from a Google Cloud Storage bucket. You need no server; the bucket alone
is the remote. See [README.md](README.md) for the details.

## 1. Install


### From the latest release into `/usr/local/bin`

```console
sudo mkdir -pv /usr/local/bin
curl -fsSL "https://github.com/MaesterZ/git-remote-gs/releases/latest/download/git-remote-gs-$(uname -s)-$(uname -m).tar.gz" \
  | sudo tar -xzv --no-same-owner -C /usr/local/bin git-remote-gs
```

### Or build it into `~/go/bin` using Go, which must be on your PATH

```console
go install -trimpath -tags disable_grpc_modules github.com/MaesterZ/git-remote-gs@latest
git-remote-gs --version
```
## 2. Log in to Google Cloud

```console
gcloud auth application-default login
```

You need `storage.objects.{list,get,create,delete}` permissions on the bucket.
IAM role `storage.objectUser` grants all four.

## 3. Use


### Existing repo

```console
# An existing repo
git remote add gcs gs://my-bucket/my-repo
git push -u gcs main
```

### New repo

```console
git clone gs://my-bucket/my-repo
```

The prefix is mandatory in `gs://BUCKET/PREFIX[/...]`, one bucket can hold many repos.

## Good to know

- **Each ref has a lock in the bucket.** A lock left by a crashed push is taken over after 60s; set
  `GIT_REMOTE_GS_LOCK_TTL` to change that.
- **Protect a branch** against force pushes and deletion:
  `gcloud storage cp /dev/null 'gs://my-bucket/my-repo/refs/heads/main/PROTECTED#'`.
- Set `GIT_REMOTE_GS_VERBOSE` to any value to log every step to stderr.

## Limits

- **Full history every time:** each push uploads the ref's whole history as a single bundle, and each fetch of a
  changed ref downloads it. This suits small and medium repos, not large ones.
- **No [shallow](https://git-scm.com/docs/git-clone#Documentation/git-clone.txt---depthdepth) or
  [partial](https://git-scm.com/docs/partial-clone) clones:** `--depth`, `--shallow-since`, `--shallow-exclude` and
  `--filter` are ignored, and you get a full clone.
- **No [atomic pushes](https://git-scm.com/docs/git-push#Documentation/git-push.txt---atomic):** `--atomic` is refused.
- **[`--force-with-lease`](https://git-scm.com/docs/git-push#Documentation/git-push.txt---force-with-lease) works**, but
  git checks the lease when the push starts, not under the ref's lock.
- **No [LFS](https://git-lfs.com/):** pushing a repo with LFS files fails.
- **SHA-1 repositories only:** a push from a SHA-256 repository is refused.

See [Limits](README.md#limits) and [Differences from `git-remote-s3`](README.md#differences-from-git-remote-s3) in the
README for details.
