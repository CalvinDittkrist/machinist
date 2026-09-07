#!/usr/bin/env bash

set -euo pipefail

if [[ $(id -u) -ne 0 ]]; then
  echo "run this script as root" >&2
  exit 1
fi

machinist_version=${MACHINIST_VERSION:-}
if [[ ! $machinist_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "set MACHINIST_VERSION to the release being installed, such as v0.2.0" >&2
  exit 2
fi

legacy_root_install=false
if [[ -d /root/.machinist ]]; then
  legacy_root_install=true
fi
for legacy_unit in machinist-control-plane.service machinist-worker.service; do
  if [[ -f /etc/systemd/system/$legacy_unit ]] && grep -q '^User=root$' "/etc/systemd/system/$legacy_unit"; then
    legacy_root_install=true
  fi
done
if [[ $legacy_root_install == true ]]; then
  echo "legacy root-based Machinist installation detected" >&2
  echo "follow the v0.1.x migration steps in docs/vm-deployment.md before running this bootstrap" >&2
  exit 3
fi

if [[ ! -r /etc/os-release ]]; then
  echo "unsupported Linux distribution: /etc/os-release is missing" >&2
  exit 1
fi
. /etc/os-release
if [[ ${ID:-} != ubuntu && ${ID:-} != debian ]]; then
  echo "unsupported Linux distribution: ${ID:-unknown}" >&2
  exit 1
fi

# MACHINIST_ROLE=control-plane skips the worker tooling (coding agents and the
# quota-axi adapter) and leaves the worker service disabled. The default
# installs both roles on one host.
machinist_role=${MACHINIST_ROLE:-worker}
if [[ $machinist_role != worker && $machinist_role != control-plane ]]; then
  echo "MACHINIST_ROLE must be worker or control-plane" >&2
  exit 2
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ca-certificates curl git gh jq openssh-client tar

# quota-axi reads provider quota for the managed worker. It is a Node.js CLI,
# pinned to the release Machinist is tested against.
quota_axi_version=${QUOTA_AXI_VERSION:-0.1.39}
node_major=${NODE_MAJOR:-22}
if [[ $machinist_role == worker ]] && { ! command -v node >/dev/null 2>&1 || [[ $(node --version | sed 's/^v//' | cut -d. -f1) -lt $node_major ]]; }; then
  curl -fsSL "https://deb.nodesource.com/setup_${node_major}.x" | bash -
  apt-get install -y nodejs
fi

runtime_user=machinist
if ! id "$runtime_user" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "$runtime_user"
fi
runtime_home=$(getent passwd "$runtime_user" | cut -d: -f6)
if [[ -z $runtime_home || ! -d $runtime_home ]]; then
  echo "could not determine home directory for $runtime_user" >&2
  exit 1
fi

curl -fsSL "https://raw.githubusercontent.com/owainlewis/machinist/$machinist_version/install.sh" | \
  env MACHINIST_VERSION="$machinist_version" sh

if [[ $machinist_role == worker ]]; then
  # Codex and Claude Code use per-user credentials, so install their standalone
  # distributions as the account that will run Machinist.
  runuser -u "$runtime_user" -- env HOME="$runtime_home" \
    bash -c 'curl -fsSL https://chatgpt.com/codex/install.sh | sh'
  runuser -u "$runtime_user" -- env HOME="$runtime_home" \
    bash -c 'curl -fsSL https://claude.ai/install.sh | bash'

  # Install the pinned quota-axi release into the runtime user's ~/.local so it
  # runs under the same account and credential store as the executors. A
  # matching existing installation is reused.
  installed_quota_axi=""
  if [[ -x "$runtime_home/.local/bin/quota-axi" ]]; then
    installed_quota_axi=$(runuser -u "$runtime_user" -- env HOME="$runtime_home" \
      "$runtime_home/.local/bin/quota-axi" --version 2>/dev/null | tail -n 1 || true)
  fi
  if [[ $installed_quota_axi != "$quota_axi_version" ]]; then
    runuser -u "$runtime_user" -- env HOME="$runtime_home" \
      npm install --global --prefix "$runtime_home/.local" "quota-axi@$quota_axi_version"
  fi

  # Standalone agent installers use ~/.local/bin. Login shells commonly add that
  # directory to PATH, but services and other non-interactive processes do not.
  for agent_command in codex claude quota-axi; do
    agent_path="$runtime_home/.local/bin/$agent_command"
    if [[ ! -x "$agent_path" ]]; then
      echo "$agent_command installer did not create $agent_path" >&2
      exit 1
    fi
    ln -sfn "$agent_path" "/usr/local/bin/$agent_command"
  done
  installed_quota_axi=$(runuser -u "$runtime_user" -- env HOME="$runtime_home" quota-axi --version | tail -n 1)
  if [[ $installed_quota_axi != "$quota_axi_version" ]]; then
    echo "quota-axi reports version $installed_quota_axi, expected $quota_axi_version" >&2
    exit 1
  fi
fi

runuser -u "$runtime_user" -- env HOME="$runtime_home" machinist init

worker_was_enabled=false
worker_was_active=false
if systemctl is-enabled --quiet machinist-worker.service 2>/dev/null; then
  worker_was_enabled=true
fi
if systemctl is-active --quiet machinist-worker.service 2>/dev/null; then
  worker_was_active=true
fi

service_base_url="https://raw.githubusercontent.com/owainlewis/machinist/$machinist_version/deploy/systemd"
service_tmp_dir=$(mktemp -d)
trap 'rm -rf "$service_tmp_dir"' EXIT
curl -fsSL "$service_base_url/machinist-control-plane.service" \
  -o "$service_tmp_dir/machinist-control-plane.service"
curl -fsSL "$service_base_url/machinist-worker.service" \
  -o "$service_tmp_dir/machinist-worker.service"
install -m 0644 "$service_tmp_dir/machinist-control-plane.service" \
  /etc/systemd/system/machinist-control-plane.service
install -m 0644 "$service_tmp_dir/machinist-worker.service" \
  /etc/systemd/system/machinist-worker.service
systemctl daemon-reload
systemctl enable machinist-control-plane.service
systemctl restart machinist-control-plane.service
if [[ $machinist_role == control-plane ]]; then
  systemctl disable --now machinist-worker.service 2>/dev/null || true
elif runuser -u "$runtime_user" -- env HOME="$runtime_home" machinist worker validate --help >/dev/null 2>&1; then
  if runuser -u "$runtime_user" -- env HOME="$runtime_home" machinist worker validate >/dev/null 2>&1; then
    systemctl enable machinist-worker.service
    systemctl restart machinist-worker.service
  else
    systemctl disable --now machinist-worker.service
  fi
else
  if [[ $worker_was_active == true ]]; then
    if [[ $worker_was_enabled == true ]]; then
      systemctl enable machinist-worker.service
    else
      systemctl disable machinist-worker.service
    fi
    systemctl restart machinist-worker.service
    echo "installed Machinist release does not support worker validation; restored the previously active worker" >&2
  else
    if [[ $worker_was_enabled == true ]]; then
      systemctl enable machinist-worker.service
      systemctl stop machinist-worker.service
      echo "installed Machinist release does not support worker validation; preserved the enabled but inactive worker" >&2
    else
      systemctl disable --now machinist-worker.service
    fi
  fi
fi

cat <<'EOF'

VM bootstrap complete.

Next steps:
  1. Run `su - machinist`, then complete the remaining login and repository steps as that user.
  2. Run `gh auth login`.
  3. Run `codex` once and sign in.
  4. Run `claude` once and sign in.
  5. Run `machinist worker quota` to confirm quota-axi reads each provider
     under this account.
  6. Clone each repository agents may use and register its absolute path in
     ~/.machinist/worker.toml.
  7. Exit back to root and run `systemctl enable --now machinist-worker` after registering a repository.
  8. Check `systemctl status machinist-control-plane machinist-worker`.

Keep the control plane on 127.0.0.1. Reach it from your computer with:
  ssh -N -L 7331:127.0.0.1:7331 machinist
EOF
