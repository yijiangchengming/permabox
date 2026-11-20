# PermaBox repository instructions

Follow the [contribution guide](CONTRIBUTING.md). Use the `work branch -> dev -> main` workflow.

- Continue work on the branch associated with the active task or PR. For independent work, start a short-lived branch from upstream `dev`. Keep unrelated changes in separate PRs and target `dev`.
- Only a PR from this repository's `dev` may target `main`. Use a merge commit for these PRs and keep `dev` after merging.
- Do not push directly to `dev` or `main`, merge your own changes, bypass review and CI, or rewrite shared branch history.
- Update the English and Chinese guides when user-facing behavior, deployment steps, or contributor instructions change.
- Run checks appropriate to the change. For source or deployment changes, run `make check`; for mount lifecycle changes, also run `make test-kernel`. Report which checks ran and identify any that could not run.
- Release from a reviewed commit on `main` only. Do not create or move a release tag unless publishing that release is explicitly requested.
- Report credentials or unpublished vulnerabilities through the private process in [SECURITY.md](SECURITY.md).
