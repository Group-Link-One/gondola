# Contributing

Thanks for taking the time. Issues and pull requests are welcome.

## How changes flow

This repository is a mirror. Development happens in an internal repository and
each release is published here as a single squashed commit.

1. Open a pull request against `main` as usual.
2. Once it is reviewed and accepted, a maintainer applies it upstream and closes
   the pull request with a link to the release that carries it.
3. The next sync brings the change back here. Your authorship is kept with a
   `Co-authored-by:` trailer on the sync commit.

Because of this, please keep pull requests small and focused, and do not rebase
on top of a sync while a review is open; a maintainer will handle conflicts.

## Developer Certificate of Origin

Every commit must be signed off, certifying the
[Developer Certificate of Origin](https://developercertificate.org/):

```
Signed-off-by: Your Name <you@example.com>
```

`git commit -s` adds it. By signing off you confirm that you wrote the change,
or otherwise have the right to submit it, under this project's license.

## Before you open a pull request

- `go fmt ./...` and `go vet ./...` are clean.
- `go test ./...` passes.
- New behavior has a test, and user-facing changes are reflected in `README.md`.
- No secrets, internal hostnames or real customer data in code, fixtures or docs.

## Reporting security issues

Do not open a public issue. Use GitHub's
[private vulnerability reporting](../../security/advisories/new) for this
repository instead.
