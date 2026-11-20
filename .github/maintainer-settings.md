# GitHub repository settings

Apply this checklist with a repository administrator account. These controls are GitHub settings; committing this document does not enable them.

For the initial rollout, create `dev` from the current upstream `main` if it does not exist. Merge the policy changes into `dev` through a work PR, then promote `dev` into `main` through a reviewed PR. Enable the `main-pr-source` requirement once the workflow is on `main` and GitHub has recorded a check result.

In **Settings > General > Pull Requests**:

- Enable merge commits and squash merging. Disable rebase merging.
- Delete merged work branches. Protect `dev` and `main` from deletion before enabling automatic deletion of merged head branches.

In **Settings > Rules > Rulesets**, create separate active branch rulesets for `dev` and `main`. Use branch protection with the equivalent controls if the repository does not use rulesets.

- For both branches, require a pull request with one independent approval. Dismiss stale approvals on new commits and require conversations to be resolved before merging. Block force pushes and deletion, and leave the bypass list empty.
- For both branches, require the `verify` job from **CI**, with GitHub Actions as the expected source. Use the check name shown by GitHub after the job has run.
- For `main`, also require `main-pr-source` from **Main PR policy**, with GitHub Actions as the expected source. Allow only merge commits for PRs into `main` if the ruleset offers a choice of merge methods. Leave linear history disabled so the merge preserves `dev` ancestry.
- For `dev`, use squash merges for ordinary work PRs. Also allow merge commits for PRs that synchronize history from `main`.

When reviewing or merging into `main`, confirm that the PR header names `dev` from this repository. Review changes to workflows that produce required checks: PRs can propose changes to `pull_request` workflows, and GitHub associates the check results with commits rather than PR source branches. Keep the reviewer source check even when automation is enabled.

After enabling the rules, verify them without merging the test PRs:

- A same-repository `dev` PR to `main` passes `main-pr-source` and still requires review and CI.
- A work branch and a fork's `dev` both fail `main-pr-source` when targeting `main`. Exercise both with a head commit shared with `dev` to detect status reuse, and retarget a work PR from `dev` to `main` to verify that the policy reruns. If GitHub accepts a reused status, enforce the source through review rather than relying on the status check alone.
- A work PR to `dev` requires review and CI without the `main-pr-source` check.

Stable `vMAJOR.MINOR.PATCH` release tags should come from reviewed `main` commits and remain immutable. The [release procedure](../CONTRIBUTING.md#release-an-installer-image) describes the publishing checks. Keep any tag rules compatible with maintainers creating new release tags.

GitHub documentation: [GitHub Flow](https://docs.github.com/en/get-started/using-github/github-flow) and [available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets).
