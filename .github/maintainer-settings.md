# GitHub repository settings

Apply this checklist with a repository administrator account. These controls are GitHub settings; committing this document does not enable them.

In **Settings > General > Pull Requests**:

- Enable squash merging and disable merge commits and rebase merging for this repository.
- Enable automatic deletion of merged head branches.

In **Settings > Rules > Rulesets**, create an active branch ruleset targeting `main`. Use branch protection with the equivalent controls if the repository does not use rulesets.

- Require a pull request with one approving review. Dismiss stale approvals on new commits and require conversations to be resolved before merging.
- Require the existing `verify` job from the **CI** workflow, with GitHub Actions as the expected source. Select the result shown by GitHub for a PR after CI has run. Require the PR branch to be up to date before merging. If a job is renamed, update the required check when the replacement result is available.
- Block force pushes and deletion. Keep the bypass list empty for routine development. Require linear history to match the squash merge policy.

After enabling the rules, verify them on a draft or test PR without merging it: check that GitHub marks review and CI as required, then confirm that an approved PR with successful checks offers the squash merge option. Do not use a direct push to `main` to test the rules.

Stable `vMAJOR.MINOR.PATCH` release tags should come from reviewed `main` commits and remain immutable. The [release procedure](../CONTRIBUTING.md#release-an-installer-image) describes the publishing checks. Keep any tag rules compatible with maintainers creating new release tags.

GitHub documentation: [GitHub Flow](https://docs.github.com/en/get-started/using-github/github-flow) and [available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets).
