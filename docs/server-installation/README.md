# Future server installation

Planning notes only. No deployment has been configured or performed.

## Available hardware

- Ugreen NAS DXP2800GT, Linux; confirm the exact CPU architecture, OS, and container support before choosing installation artifacts.
- Raspberry Pi 4, Linux; confirm whether the installed OS is 64-bit before choosing an arm64 artifact.

## Intended outcome

Host the application for personal use. Later, install approved releases
automatically with minimal maintenance. First review the application architecture
and choose the server roles, storage layout, authentication, and network access.

Keep production and any future test installation separate: data directories,
credentials, configuration, ports, and agent permissions. A `staging` Git branch
alone does not isolate runtime data. Plan backups, restore testing, health checks,
and rollback before enabling automatic updates.

Installation instructions will be written after the architecture review and
hardware/OS verification. Do not treat these notes as an installation procedure.
