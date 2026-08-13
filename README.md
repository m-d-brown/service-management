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
task install
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

### `update-container-manifest`

A tool to bump pinned container image versions in an Ansible-style manifest (a
flat `images:` map of name → full image reference) to the newest stable version
within each image's current major version.

**Features:**

- Queries each image's registry directly (anonymous, via `go-containerregistry`)
  — no pulls, no local container runtime needed.
- Conservative by design: stays within the current major version. If a newer
  major exists, it is noted in a trailing `# newer major: X.Y.Z` comment rather
  than applied, so major jumps stay deliberate hand-edits.
- Understands semver (`2.11.3`), calver (`2026.5.4`) and date tags
  (`2026-05-22`); rejects `latest`/`rc`/`beta`/`-alpine` variants and registry
  build-id tags by shape-matching against the current pin.
- Re-resolves the `:latest` manifest digest for digest-pinned images (those with
  no usable version tag).

**Installation:**

```shell
task install
```

**Usage:**

```shell
update-container-manifest -file ansible/images.yml            # rewrite in place
update-container-manifest -file ansible/images.yml -dry-run   # preview only
```

**Example Output:**

```text
IMAGE      CURRENT              NEW                  NOTE
nginx      :1.25.3              :1.25.4
postgres   :15.6                :15.8                newer major: 17.2
statuspage @sha256:abcd1234ef…  @sha256:5678feed90…
redis      :7.2.4               :7.2.4

Wrote ansible/images.yml — 3 image(s) changed.
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
- **Boot State Verification**: Compares each host's kernel boot ID (or uptime,
  for hosts without one) before and after the reboot, so a host that answered
  ping without ever restarting is warned about and fails the run instead of
  passing silently.
- **Command Transparency**: Echoes every SSH and ping command as a
  copy-pasteable shell line before running it.

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
Recording pre-reboot boot state...
  Reading boot state of hypervisor-1:
    $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@10.0.0.5 'printf "boot_id=%s\n" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"; printf "uptime=%s\n" "$(cut -d" " -f1 /proc/uptime 2>/dev/null)"'
Executing pre-flight graceful halt for 'vm-a' via SSH...
  $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new admin@10.0.0.21 'sudo poweroff || poweroff'
Waiting 15s for 'vm-a' to power down...
Verifying VM state and enforcing fallback stop command on hypervisor 'hypervisor-1' via SSH...
  $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@10.0.0.5 'sudo qm stop 101 || qm stop 101'
Issuing reboot command to: hypervisor-1
  $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@10.0.0.5 'sudo reboot || reboot'
Waiting 15 seconds for hosts to drop off the network...
Waiting for hypervisor-1, vm-a to return online...
  $ ping -c 1 -W 1 10.0.0.5
  $ ping -c 1 -W 1 10.0.0.21
[✓] hypervisor-1 is reachable.
[✓] vm-a is reachable.
Verifying boot state changed...
  Reading boot state of hypervisor-1:
    $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@10.0.0.5 'printf "boot_id=%s\n" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"; printf "uptime=%s\n" "$(cut -d" " -f1 /proc/uptime 2>/dev/null)"'
[✓] hypervisor-1 rebooted: boot_id changed (9c2f1a44-3b7e-4d51-9f0a-1b2c3d4e5f60 -> 1d7b9e02-5a3c-4f18-8e6d-7a9b0c1d2e3f)

=== Executing Tier: 2 ===
... (vm-a follows the same probe, reboot, ping, verify sequence) ...
[✓] vm-a rebooted: boot_id changed (44e1c0d3-8f2b-4a67-b1c9-0d5e6f708192 -> b83a5c17-6d4e-4029-9c3b-5f1a2e8d47c0)

=== Reboot Verification Summary ===
Confirmed rebooted: 2  Not rebooted: 0  Unverified: 0

All tiers complete. Reboot orchestration finished successfully.
```

A host that answers ping without having actually restarted is called out and
fails the run rather than being reported as a success:

```text
[✓] vm-a is reachable.
Verifying boot state changed...
[✗] WARNING: vm-a did NOT reboot: boot_id is unchanged (44e1c0d3-8f2b-4a67-b1c9-0d5e6f708192); the host never went down

=== Reboot Verification Summary ===
Confirmed rebooted: 1  Not rebooted: 1  Unverified: 0
  [✗] vm-a: boot_id is unchanged (44e1c0d3-8f2b-4a67-b1c9-0d5e6f708192); the host never went down

All tiers complete, but 1 host(s) could not be confirmed as rebooted.
```

### `proxmox-retrust-host-keys`

A CLI tool to safely re-trust SSH host keys of Proxmox guests, verified
out-of-band via the hypervisor.

**Features:**

- **No trust-on-first-use**: reads each guest's public host keys through the
  Proxmox hypervisor (the API's guest-agent file-read via `pvesh` for VMs,
  `pct exec` for LXCs, which the API cannot reach into) rather than trusting
  whatever key the network presents after a "REMOTE HOST IDENTIFICATION HAS
  CHANGED" failure.
- **Every name covered**: SSH records trust per name-as-typed, so entries are
  maintained under each guest's name plus any supplied FQDN/IP aliases, on a
  single combined known_hosts line.
- **Idempotent**: only stale entries are replaced; a `--dry-run` mode reports
  what would change.

**Installation:**

```shell
task install
```

**Usage:**

```shell
proxmox-retrust-host-keys --node root@pve1.example.com \
    webhost=webhost.example.com,192.0.2.20
```

- `--node SSH_DEST` (repeatable): a Proxmox node to query, through the system
  `ssh` binary so your own known_hosts and ssh_config govern node verification.
- `GUEST[=NAME,...]` (optional): limit to these guests; each may carry
  comma-separated extra names (FQDN, IP) to maintain its known_hosts entries
  under. With no guest arguments every running guest is processed.
- `--dry-run` reports stale entries without touching known_hosts, exiting 1 if
  any are stale. `--known-hosts FILE` overrides `~/.ssh/known_hosts`.

VMs must run the QEMU guest agent to be readable; guests without one are
reported as warnings and skipped.

**Example Output:**

```text
ok:        webhost already trusted
retrusted: database 3 verified keys installed under 3 names
WARN:      appliance (vmid 105 on root@pve1.example.com): can't read keys
```

## Getting Started / Contributing

This repository uses [mise](https://mise.jdx.dev/) to provision its CLI tools,
[Task](https://taskfile.dev/) to orchestrate build operations, and
[pre-commit](https://pre-commit.com/) to enforce code health (linting and
formatting) across all languages.

To set up your local development environment:

1. Clone the repository.
2. [Install mise](https://mise.jdx.dev/getting-started.html), then provision the
   project's tools (`task`, `uv`, `golangci-lint`):

   ```shell
   mise install
   ```

   _What this does: It reads `mise.toml` and installs the exact tools CI uses,
   so your environment cannot drift from the pipeline._

3. Run the bootstrap command:

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

### Dev container (optional)

If you would rather not install the toolchain on your machine, this repository
ships a [dev container](https://containers.dev/). Open the repo in VS Code and
choose _Reopen in Container_, or run `devcontainer up --workspace-folder .`.

It provisions Go, Python, `uv`, `task`, `golangci-lint` and `pre-commit` at the
versions read from `go/go.mod`, `.python-version` and `mise.toml`, runs
`task setup` for you, and points the editor at the same linters and formatters
`task check` uses. Nothing is pinned in `.devcontainer/`, so a version bump
needs no change there — rebuild the container, or re-run
`./.devcontainer/post-create.sh` in place.

## Development

We use [Task](https://taskfile.dev/) as our build tool. Use `task` at the root
to manage operations across all languages:

- `task check`: Lint, format-check, type-check, and scan all files.
- `task format`: Auto-format and fix all source files.
- `task build`: Build all binaries.
- `task test`: Run all tests (Go and Python).
- `task tidy`: Tidy all module dependencies.

## Repository Structure

- `go/`: Go-based tools and libraries.
  - `go/cmd/container-version-snapshot`: Tool to snapshot container versions via
    SSH.
  - `go/cmd/update-container-manifest`: Tool to bump pinned image versions in an
    `images.yml` manifest to the newest stable within each major.
- `python/`: (Planned) Python-based tools and libraries.
- `design/`: Architectural design documents.
- `skills/`: Repository-specific skills and guides.
