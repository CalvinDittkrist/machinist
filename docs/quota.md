# Quota-aware admission and usage history

Machinist can keep queued work waiting until the worker that would run it has
fresh evidence of enough subscription quota. It also records provider quota
before and after every run, so later estimates come from comparable history
instead of guesses.

Quota admission is an estimate, not a guarantee that a task finishes. Reported
tokens and subscription quota are separate measurements: Machinist never
converts one into the other, and provider percentages are never compared
across providers.

## How it works

1. The **worker** runs a pinned [quota-axi](https://github.com/kunchenguid/quota-axi)
   release as its own service user, so it sees the same sign-in state as its
   executors. The adapter applies a subprocess timeout, caches results briefly,
   and validates the JSON schema version. Account identity is reduced to a
   pseudonymous key before anything leaves the worker; credentials are never
   read or stored by Machinist.
2. Each poll carries the sanitized observations. The **control plane**
   evaluates every governed run before leasing it. A run whose provider lacks
   fresh, sufficient evidence records a waiting reason and next check time and
   stays queued. Other eligible runs are still admitted.
3. When a run is admitted, its estimated requirement is reserved on that
   provider account. Workers sharing the account see the reservation and cannot
   claim the same headroom.
4. After the process exits, the worker takes a fresh observation and delivers
   it with the completion. The control plane stores the before/after
   comparison per window and marks its quality.
5. Comparable, reliably measured runs feed the next estimate.

Providers: Claude, Codex, and GitHub Copilot. There is no automatic provider or
model fallback.

## Configure the worker

```toml
# ~/.machinist/worker.toml
[quota]
command = ["quota-axi"]   # or ["npx", "-y", "quota-axi@0.1.39"]
timeout = "20s"           # subprocess limit
cache = "1m"              # reuse one observation for this long
credential_refresh = true # let quota-axi delegate an expired session renewal to the vendor CLI

[executors.claude]
command = ["claude", "--print", "--model={{machinist.model}}"]
models = { opus = "claude-opus-5" }
# provider is inferred from the executable name; set it explicitly for wrappers.
# provider = "claude"
```

Remove the `[quota]` table to send no evidence. An executor without a provider
(a script, a test runner) is never governed. Set `provider` explicitly to
`claude`, `codex`, or `copilot` when a wrapper hides the executable name.

Keep `cache` shorter than the control plane's `max_observation_age`; otherwise
every observation is already stale by the time it is evaluated. With
`credential_refresh = false` a read is strictly read-only, but an expired
session then parks every governed run until someone signs in interactively;
the default lets quota-axi hand the renewal to the vendor CLI, exactly as the
executor would on its next run. Initial sign-in is never performed by
Machinist.

Verify the adapter under the worker's account:

```sh
machinist worker quota
```

The diagnostic prints the installed quota-axi version against the pinned
release, every governed provider's windows and freshness, and fails when a
provider is not usable, for example when a sign-in is missing.

## Configure the control plane

```toml
# ~/.machinist/config.toml
[server.quota]
enforcement = "enabled"      # "disabled" keeps capability-only scheduling
minimum_reserve_percent = 20 # required per window without reliable history
safety_reserve_percent = 5   # added on top of a historical estimate
minimum_samples = 3          # reliable samples needed before history is used
max_observation_age = "3m"   # older evidence counts as stale
check_interval = "1m"        # how long a waiting run stays parked before re-evaluation
```

With enforcement disabled, scheduling behaves exactly as before, and evidence
is still recorded when the worker supplies it. With enforcement enabled, a run
waits when:

- the worker sent no evidence for the provider;
- the provider needs a sign-in, is unavailable, or the adapter failed
  (missing tool, timeout, malformed or unsupported output);
- the observation is flagged stale or older than `max_observation_age`;
- a window reset after the observation was taken (fresh evidence is required;
  the reset timestamp alone never releases a run);
- any binding window has less available headroom than the run requires.

Binding windows are every session, weekly, and monthly window plus model
windows that match the run's requested or resolved model. When the model is unknown, every
model window binds. An available session window never overrides an exhausted
weekly or model window.

A waiting run is re-evaluated when its next check is due, or immediately when
a worker supplies newer evidence or evidence for a different account.

## Estimates and reserves

The requirement for a window is the 90th percentile of consumption over the
most recent comparable runs (up to 20), plus the safety reserve. Runs are
comparable when they share repository, command, command hash, provider, and
requested and resolved model. History is never widened across projects,
workflow versions, or model mappings when comparable samples are insufficient. Failed, timed out, and cancelled runs count
because they consume quota. Windows without at least `minimum_samples`
reliable samples use `minimum_reserve_percent` instead.

Availability is the window's remaining percentage minus reservations held by
other running runs on the same provider account. Reservations are conservative:
they are not reduced as a run consumes quota. After a run completes, observations
older than that completion cannot allocate more headroom on the same account,
even if the worker cache has not expired or the completion has no quota evidence.
The worker must refresh its observation. Incomplete reports also wait until all
quota windows are readable.

## Measurement quality

Each window is compared before and after the run and stored with one quality:

| Quality | Meaning | Used for estimates |
| --- | --- | --- |
| `measured` | Same reset time, no other run on the account was active | yes |
| `overlapping` | Another run on the same provider account was active in between | no |
| `reset` | The window reset during the run | no |
| `missing_before` / `missing_after` | One observation was unusable or absent | no |

A difference is never presented as proven task-attributable consumption.
Overlap detection uses the timestamps of both observations, including activity
between a cached pre-run observation and the run start. Unrelated manual use
of the same account is unobservable and may be included.

## Dashboard

Queued runs that are waiting show a **Waiting for quota** badge with the reason
and next check. The task page shows the binding windows with remaining,
reserved, available, and required percentages, the admission headroom of a
running run, and the before/after usage with its quality after completion.
The Workers page lists each worker's providers with freshness, plan, and window
percentages, or the sign-in or tooling problem that blocks them.

## Storage

Schema version 3 adds quota columns to `runs`, a `run_quota_windows` table
with one row per run and window, and a `worker_quota` table with each worker's
latest observation per provider. Existing runs and token values are preserved
by the upgrade. Snapshots hold only sanitized observations: no credentials,
emails, or account identifiers.

## Installation

`scripts/setup-vm.sh` installs Node.js and the pinned quota-axi release into
the runtime user's `~/.local/bin`, links it into `/usr/local/bin`, and checks
the version. A control-plane-only host does not need quota-axi: run the
bootstrap with `MACHINIST_ROLE=control-plane` to skip it.

Manual installation:

```sh
npm install --global quota-axi@0.1.39
quota-axi --version
machinist worker quota
```

Provider sign-in stays an explicit setup step: run `claude`, `codex`, or the
Copilot CLI once as the worker user before enabling enforcement.
