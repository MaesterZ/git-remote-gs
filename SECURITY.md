# Security policy

Report vulnerabilities privately through [GitHub's private vulnerability reporting](https://github.com/MaesterZ/git-remote-gs/security/advisories/new), not in a public issue.

Include the steps to reproduce, the output of `git-remote-gs --version`, and what an attacker could do.

Only the latest release gets security fixes.

## Threat model

What the helper trusts, and what it doesn't:

- **Google credentials.** Application Default Credentials decide who can read and write the bucket. Anyone holding them can change the repository.
- **The bucket is the repository.** Whoever can write to the bucket can change the history every clone fetches. The helper checks bundles with `git bundle verify`, but doesn't run `git fsck` on what it downloads.
- **Text from the bucket is untrusted.** Lock files are written by other clients. Their contents are printed in error messages, so control characters are shown as `?`.
- **Your machine.** The helper runs `git` with your environment and writes temporary bundle files, which it deletes afterwards.

Out of scope: access rules inside the bucket (use IAM), and scanning pushed content for secrets.
