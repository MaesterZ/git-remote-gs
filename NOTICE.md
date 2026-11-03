# Notice

```text
git-remote-gs
Copyright 2026 MaesterZ
```

`git-remote-gs` is licensed under the Apache License, Version 2.0. See [LICENSE.md](LICENSE.md) for the full text.

This file holds the attribution notices that Section 4(d) of the license requires to travel with the project. It is
informational only and does not modify the license. If you redistribute `git-remote-gs` or a work derived from it,
keep these notices; you may add your own alongside them.

---

## Derived works

`git-remote-gs` is a Go rewrite. No source files were copied from the projects below, but its protocol handling,
storage layout and locking scheme derive from them. Both are licensed under the Apache License, Version 2.0.

### git-remote-s3

<https://github.com/awslabs/git-remote-s3>: the original Python remote helper for Amazon S3, and the source of the
bundle-per-ref design and the push lock. Its NOTICE file reads:

```text
git-remote-s3
Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
```

### git-remote-gcs

<https://github.com/Vicguin65/git-remote-gcs>: the Python port to Google Cloud Storage that `git-remote-gs` started as a rewrite
of.

```text
git-remote-gcs
Copyright 2026 Tyler Du
```

---

## Third-party Go modules

Release binaries are statically linked with the Go modules listed in [go.mod](go.mod), each under its own license. The
direct dependencies are:

| Module                        | License      |
| ----------------------------- | ------------ |
| `cloud.google.com/go/storage` | Apache-2.0   |
| `cloud.google.com/go/auth`    | Apache-2.0   |
| `google.golang.org/api`       | BSD-3-Clause |

`github.com/fsouza/fake-gcs-server` (BSD-2-Clause) is used by the tests only and is not part of the binary.
