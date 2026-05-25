# service-management

A collection of tools and libraries for managing infrastructure services. This
repo is particularly relevant for 'homelab' environments that use Podman,
Docker, Ansible and Terraform.

## Tools

### `container-version-snapshot`

A tool to snapshot container versions across multiple hosts via SSH.

**Features:**

- Scans multiple hosts via SSH.
- Detects running containers using `podman`.
- Resolves versions from OCI labels or registry lookup.
- Outputs a structured JSON snapshot.

**Installation:**

Use the project's task runner to install the tool to your `$GOBIN`:

```shell
task go:install
```

**Usage:**

```shell
container-version-snapshot --host user@host1 --sudo-host user@host2
```

**Example Output:**

```json
{
  "timestamp": "2026-05-09T00:00:00Z",
  "targets": [
    {
      "host": "host1",
      "user": "user",
      "sudo": false
    }
  ],
  "containers": [
    {
      "host": "host1",
      "container": "nginx-proxy",
      "image_label": "docker.io/library/nginx:latest",
      "on_host_versioning": {
        "oci": "1.25.3",
        "raw": "sha256:abcd...",
        "other_repo_tags": ["nginx:1.25.3"]
      },
      "remote_registry_info": {
        "version": "1.25.4"
      }
    }
  ]
}
```

### `reboot-orchestrator`

A lightweight, modular, and dependency-aware Python library and CLI tool
designed to orchestrate system reboots across network infrastructure.

**Features:**

- **Topological Sequence (DAG)**: Dynamically groups hosts into execution tiers
  based on topological dependency sorting (Kahn's Algorithm).
- **Direct SSH Execution**: Triggers all reboots gracefully via parallel SSH
  commands, avoiding heavy external automation runners or playbooks.
- **ACPI "Zombie" VM Workaround**: Safely handles virtual machines suffering
  from poweroff bugs by executing pre-flight graceful halts and issuing fallback
  cut-power commands on the hypervisor host via SSH if they do not halt in time.
- **Asynchronous Reboots & Ping Tracking**: Dispatches reboot triggers
  asynchronously so that network/connectivity drops do not hang the
  orchestrator, and asynchronously tracks host status using continuous ICMP ping
  loops to guarantee a tier is fully online before moving to the next.

**Usage:**

Run the tool using `uv` from the repository root:

```shell
uv run --project python/reboot-orchestrator reboot-orchestrator [options] host1 host2 [host3 ...]
```

For detailed usage, configuration, and API specifications, consult the
[python/reboot-orchestrator README](python/reboot-orchestrator/README.md).

**Example Output:**

```text
The following hosts will be rebooted (parents first, nested dependents last):
└── hypervisor-1
    └── vm-a

=== Executing Tier: 1 ===
Issuing reboot command to: hypervisor-1
Waiting 15 seconds for hosts to drop off the network...
Waiting for hypervisor-1, vm-a to return online...
[✓] hypervisor-1 is back online!
[✓] vm-a is back online!

=== Executing Tier: 2 ===
Executing pre-flight graceful halt for 'vm-a' via SSH...
Waiting 15s for 'vm-a' to power down...
Issuing reboot command to: vm-a
Waiting 15 seconds for hosts to drop off the network...
Waiting for vm-a to return online...
[✓] vm-a is back online!

All tiers complete. Reboot orchestration finished successfully.
```

## Getting Started / Contributing

This repository uses [Task](https://taskfile.dev/) to orchestrate build
operations and [pre-commit](https://pre-commit.com/) to enforce code health
(linting and formatting) across all languages.

To set up your local development environment:

1. Clone the repository.
2. Run the bootstrap command:

   ```shell
   task setup
   ```

   _What this does: It installs `pre-commit` and configures the git hooks. Every
   time you try to `git commit`, it will automatically run tools like
   `golangci-lint` (for Go) and `ruff` (for Python) to format and lint your
   code._

You can also run these checks manually at any time without committing:

```shell
task check
```

Our GitHub Actions CI pipeline will automatically run these same checks on all
pull requests to ensure nothing slips through.

## Development

We use [Task](https://taskfile.dev/) as our build tool. Use `task` at the root
to manage operations across all languages:

- `task check`: Format, lint, type-check, and scan all files.
- `task build`: Build all binaries.
- `task test`: Run all tests.
- `task tidy`: Tidy all module dependencies.

## Repository Structure

- `go/`: Go-based tools and libraries.
  - `go/cmd/container-version-snapshot`: Tool to snapshot container versions via
    SSH.
- `python/`: (Planned) Python-based tools and libraries.
- `design/`: Architectural design documents.
- `skills/`: Repository-specific skills and guides.
