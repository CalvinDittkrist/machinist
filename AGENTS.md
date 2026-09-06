# Repository working rules

- Use English for code, documentation, issues, and pull requests.
- Never merge pull requests, enable auto-merge, publish releases, or bypass branch protection. CalvinDittkrist performs merges and release publication personally.
- Work on a short-lived branch and open a PR against `staging`. Never push directly to `main` or `staging`.
- Bugs and features require an issue before implementation; link it in the PR. Documentation, dependency updates, and repository maintenance do not require an issue.
- Use squash merges for ordinary changes. Preserve ancestry with merge commits for `staging` ↔ `main` and upstream integration. Agents prepare these PRs but never merge them.
- Never send changes to the upstream repository. Fetch upstream only, and import selected improvements after the owner chooses them.
- Never add an agent as a commit co-author.
- Prefer quality, simplicity, robustness, scalability, and long-term maintainability over development cost.
- For bug fixes, first reproduce the problem end to end as an end user would experience it.
- During end-to-end checks, inspect the UI carefully and address clear visual defects. Address encountered lint failures, test failures, and flakiness.
- Run checks appropriate to the change and report the commands and results in the PR. Do not claim unexecuted checks passed.
- Keep server deployment, new product features, and application architecture changes outside repository-setup work.

See [the development workflow](docs/development-workflow.md).
