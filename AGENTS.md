# PermaBox repository instructions

Follow the [contribution guide](CONTRIBUTING.md). Use GitHub Flow for repository changes.

- Continue work on the branch associated with the active task or PR. For independent work, start a short-lived branch from upstream `main`. Keep unrelated changes in separate PRs.
- Target `main` through a pull request. Do not push directly to `main`, merge your own changes, or bypass review and CI. Do not rewrite shared branch history.
- Update the English and Chinese guides when user-facing behavior, deployment steps, or contributor instructions change.
- Run checks appropriate to the change. For source or deployment changes, run `make check`; for mount lifecycle changes, also run `make test-kernel`. Report which checks ran and identify any that could not run.
- Release from a reviewed commit on `main` only. Do not create or move a release tag unless publishing that release is explicitly requested.
- Report credentials or unpublished vulnerabilities through the private process in [SECURITY.md](SECURITY.md).
