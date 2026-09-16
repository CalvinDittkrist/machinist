# Configuration

Commands use an executor, optional prompt template, and timeout:

```toml
[commands.audit]
executor = "codex"
prompt_file = "prompts/audit.md"
timeout = "30m"

[commands.custom-workflow]
executor = "custom-workflow-script"
timeout = "2h"
```

Without `prompt_file`, the input prompt is sent unchanged. With a template, include
`{{machinist.prompt}}`. Executors and repositories remain worker-owned:

```toml
[executors.custom-workflow-script]
command = ["./scripts/custom-workflow.sh"]

[repositories.my-project]
path = "/absolute/path/to/my-project"
```

Managed triggers select one command with `command = "audit"`. Model selection remains
available when the executor command includes `{{machinist.model}}`.

## Quota-aware admission

The worker can report provider quota with each poll, and the control plane can keep
governed runs queued until fresh evidence shows enough headroom:

```toml
# worker.toml
[quota]
command = ["quota-axi"]
timeout = "20s"
cache = "1m"
credential_refresh = true

[executors.claude]
command = ["claude", "--print", "--model={{machinist.model}}"]
provider = "claude" # inferred from the executable name when omitted
```

```toml
# config.toml
[server.quota]
enforcement = "enabled" # "disabled" preserves capability-only scheduling
minimum_reserve_percent = 20
safety_reserve_percent = 5
minimum_samples = 3
max_observation_age = "3m"
check_interval = "1m"
```

Executors whose command does not name `claude`, `codex`, or `copilot` are ungoverned
unless `provider` is set. Run `machinist worker quota` as the worker user to verify the
adapter. See [quota-aware admission](quota.md) for the estimate, reserve, and
measurement semantics.

## Migration

The `agents` table was renamed to `commands`. Move `[agents.NAME]` to `[commands.NAME]`
and replace `--agent` with `--command`.

The pipeline feature was removed. Replace a sequential pipeline with one executable script,
configure that script as an approved worker executor, and expose it through one command.
Legacy `[pipelines]` configuration fails with migration guidance. Pre-command databases are
recreated once because this release intentionally consolidates the schema before active use.
