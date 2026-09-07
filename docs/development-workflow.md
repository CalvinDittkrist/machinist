# Development workflow

This public personal fork is maintained independently by CalvinDittkrist.
Outside contributions are welcome. The original project is
[owainlewis/machinist](https://github.com/owainlewis/machinist); no changes are sent back automatically.

## Branches and issues

`main` is the stable release branch. `staging` is the integration branch, not a
deployed test environment. Start `feat/...`, `fix/...`, or `chore/...` branches
from `staging` and submit PRs to `staging`. Open an issue first for each bug or
feature. Documentation, dependency updates, and repository maintenance are exempt.

Use the [development board](https://github.com/users/CalvinDittkrist/projects/8/views/2) and project statuses Backlog → Ready → In progress → Review → Done.
Reference issues as `Refs #123` in staging PRs: GitHub's automatic closing
behavior depends on the default branch. Close an issue when its acceptance
criteria are met; distinguish integration from production release.

Only CalvinDittkrist merges. Agents may edit, commit, push working branches,
and open PRs, but must never merge, enable auto-merge, or bypass protection.
Agent sessions using the owner's credentials are technically the owner;
the no-merge rule is an explicit working agreement, not identity isolation.
External contributors use forks rather than receiving write access.

Required CI must pass, and review conversations must be resolved. Do not
require a second person's approval in this solo-maintainer repository: authors
cannot approve their own PRs. The owner's manual merge is the final decision.

Squash ordinary PRs. Promote `staging` to `main` through an owner-merged PR
using a merge commit to preserve ancestry. Do not squash promotion PRs.
After each promotion, prepare a `main` → `staging` merge PR to bring the
promotion commit back into staging before the next release. Use a merge commit
for this reverse synchronization too.
Do not delete the protected permanent branches (`main` and `staging`). GitHub
automatically deletes merged working branches. Both squash and merge commits remain enabled for these
different uses; rebase merges and auto-merge are disabled.

## Selected upstream improvements

`origin` points to this fork; `upstream` points to the original repository (pushes disabled locally; tag
auto-fetch disabled to keep release namespaces separate).
Fetch with `git fetch upstream`. `upstream/main` tracks the original without
changing your working branches. Never use a force-sync operation on this fork.

A weekly workflow opens an overview issue only when the upstream head changes.
It records the compared commit range and links to individual changes. This is
a factual commit digest, not an AI evaluation or an automatic code import.
Choose improvements in that issue. Prepare a `sync/upstream-...` branch from
`staging`, cherry-pick selected commits with `git cherry-pick -x`, adapt them,
and test before opening a PR. Keep a record of the original commits. If taking
a complete upstream range, use a merge commit to preserve its ancestry.

## Agent skills

The engineering skills from [mattpocock/skills](https://github.com/mattpocock/skills)
live in `.agents/skills/` as editable project files, with symlinks in
`.claude/skills/` for Claude Code. Codex reads `.agents/skills/` directly.
`skills-lock.json` records the installed source and hashes. Run
`/setup-matt-pocock-skills` once per clone before using the other skills; it
configures the issue tracker, triage labels, and doc locations. Pull upstream
changes with `npx skills update`, and review the diff before committing.

## Checks, releases, and costs

The existing Go and frontend CI remains the baseline. Repository policy also
validates PR routing and issue references. Browser E2E expansion and server
deployment are separate future work. Dependabot version-update PRs target `staging`.
GitHub creates security-update PRs against the default branch (`main`) regardless
of that setting. Retarget these to `staging`, update their branch against staging,
and rerun CI before merging; the routing check deliberately prevents a direct
merge into main. Agents may prepare this retargeting but must not merge. See
[GitHub's Dependabot behavior](https://docs.github.com/en/code-security/tutorials/secure-your-dependencies/customizing-dependabot-prs).
Dependency security alerts and updates are enabled on GitHub. CodeQL default
setup scans the supported languages; its result is required alongside CI.

The Release workflow can prepare a draft from `main` through Run workflow.
Use the fork's own semantic versions, starting with `v0.1.0` if unused; inspect
existing releases/tags on this fork first. Upstream tags may exist locally;
never use `git push --tags`. A draft is not a published release. The owner
reviews and publishes it. Tag-triggered builds also produce drafts. Existing
Linux/macOS amd64/arm64 assets and reproducibility verification are retained.
Never automatically publish releases or install anything on a server yet.

Use standard GitHub-hosted runners in this public repository. Do not enable
paid runners, paid AI API calls, or paid overages. AI reviews are advisory;
GitHub requests a Copilot review on new ready PRs when the author has access
and remaining premium allowance. Re-review on every push is disabled. Codex and
Claude can review locally using existing subscriptions. No API keys are needed
for the weekly upstream digest.

Scheduled workflows run from the default branch (`main`), so automation added
through `staging` becomes active after the owner promotes it to `main`.
