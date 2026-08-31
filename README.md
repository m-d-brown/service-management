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

A dependency-aware CLI tool that reboots hosts in the right order over plain
SSH, and proves each one actually restarted.

**Features:**

- **Topological Sequence (DAG)**: Dynamically groups hosts into execution tiers
  based on topological dependency sorting (Kahn's Algorithm).
- **No inventory format, no runner**: hosts, their addresses, their SSH user and
  their ordering are given as arguments or piped in on stdin. Nothing about
  Ansible is compiled in; see
  [`ansible-inventory-reboot-hosts`](#ansible-inventory-reboot-hosts) to feed it
  an existing inventory.
- **Direct SSH Execution**: Triggers all reboots gracefully via parallel SSH
  commands, avoiding heavy external automation runners or playbooks.
- **Asynchronous Reboots**: Dispatches reboot triggers asynchronously so that
  network/connectivity drops do not hang the orchestrator.
- **Watches the Power Cycle**: Starts sampling every host in a tier _before_
  anything is powered down and keeps sampling while it reboots, so the moment a
  host leaves the network and the moment it comes back are both observed and
  logged rather than assumed. ICMP catches the drop; the return is confirmed
  over SSH, because the kernel answers ping long before sshd will accept a
  connection.
- **Pending-Reboot Detection**: With `--if-needed`, probes the named hosts over
  SSH and reboots only those actually waiting on one, re-checking each tier as
  it comes up so guests already power-cycled by their parent are left alone.
- **Boot State Verification**: Compares each host's kernel boot ID (or uptime,
  for hosts without one) before and after the reboot, so a host that answered
  ping without ever restarting is warned about and fails the run instead of
  passing silently. Where a host exposes neither marker — appliances, switches,
  busybox firmware — the observed power cycle settles it instead, which makes
  those reboots verifiable at all for the first time.
- **Command Transparency**: Echoes every SSH and ping command as a
  copy-pasteable shell line before running it.

**Usage:**

```shell
reboot-orchestrator [flags] HOST_SPEC...
```

The command line has two parts, and they deliberately look different:

- **Flags** start with `--` and apply to the whole run: `--user ops`,
  `--if-needed`.
- **Host specs** are positional arguments naming the hosts to reboot. Each is a
  host name followed by optional `key=value` fields joined by commas, carrying
  no leading dashes: `vm-a,addr=10.0.0.21,user=admin`.

A few names appear in both lists. The spec field is the more specific of the
pair and wins: `--user` supplies the login user only for hosts whose spec has no
`user=` field of its own.

**Host spec fields** (written inside a `HOST_SPEC`, no dashes):

| Field     | Meaning                                                              |
| --------- | -------------------------------------------------------------------- |
| `addr`    | Address to ping and SSH to (default: the host name)                  |
| `user`    | SSH login user (default: `--user`)                                   |
| `ssh-arg` | Extra `ssh` argument; repeatable                                     |
| `after`   | Reboot this host only once the named host is back online; repeatable |

Describing a small fleet entirely on the command line:

```shell
reboot-orchestrator --user ops \
    hypervisor-1,addr=10.0.0.5 \
    vm-a,addr=10.0.0.21,user=admin,after=hypervisor-1
```

Specs also arrive on stdin, one per line — a pipe is detected automatically, or
name a file with `--hosts-from`. Hosts read that way are **context**: they can
be depended on and are waited for, but only the hosts named as arguments are
rebooted. Naming one that stdin already defined targets that definition rather
than replacing it, so a target costs nothing more than its name:

```shell
ansible-inventory-reboot-hosts -i inventory.yml | reboot-orchestrator vm-a vm-b
```

Use `--all` to target every host read from stdin instead:

```shell
ansible-inventory-reboot-hosts -i inventory.yml | reboot-orchestrator --all --if-needed
```

**Flags** (apply to the whole run):

| Flag                       | Default | Description                                                       |
| -------------------------- | ------- | ----------------------------------------------------------------- |
| `--user`                   |         | SSH user for hosts that do not set one themselves                 |
| `--ssh-arg`                |         | Extra argument for every `ssh` invocation (repeatable)            |
| `--hosts-from`             |         | Read host specs from this file (`-` for stdin)                    |
| `--all`                    | `false` | Target every host read from stdin or `--hosts-from`               |
| `--yes`, `-y`              | `false` | Bypass the interactive confirmation prompt                        |
| `--if-needed`              | `false` | Reboot only the targeted hosts with a pending reboot              |
| `--ping-timeout`           | `1s`    | Timeout for a single ping query                                   |
| `--drop-wait`              | `15s`   | How long to wait for a host to drop before giving up on seeing it |
| `--sample-interval`        | `1s`    | How often to probe each host while it reboots                     |
| `--probe-timeout`          | `15s`   | Timeout for each SSH boot state probe                             |
| `--skip-boot-verification` | `false` | Skip the SSH check that proves hosts rebooted                     |

Exit status is `0` when every tier completed, every host was checked, and none
was proven to have skipped its reboot; `1` otherwise.

> **Note:** when specs are piped in, stdin is spent, so the confirmation prompt
> is read from `/dev/tty`. In a script or any context without a terminal, pass
> `--yes`.

**Example Output:**

```text
The following hosts will be rebooted (parents first, nested dependents last):
└── hypervisor-1
    └── vm-a

=== Executing Tier: 1 ===
Recording pre-reboot boot state...
  hypervisor-1: $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@10.0.0.5 'printf "boot_id=%s\n" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"; printf "uptime=%s\n" "$(cut -d" " -f1 /proc/uptime 2>/dev/null)"'
Watching hypervisor-1, vm-a for the reboot (sampling every 1s)...
  hypervisor-1: $ ping -c 1 -W 1 10.0.0.5
  vm-a: $ ping -c 1 -W 1 10.0.0.21
Issuing reboot command to: hypervisor-1
  hypervisor-1: $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@10.0.0.5 'sudo reboot || reboot'
hypervisor-1: [down] stopped answering at 09:14:07
vm-a: [down] stopped answering at 09:14:07
hypervisor-1: [ping] answers ping again at 09:14:49; waiting for SSH
hypervisor-1: [back] is back at 09:15:02, after 55s down
vm-a: [ping] answers ping again at 09:15:21; waiting for SSH
vm-a: [back] is back at 09:15:29, after 1m 22s down
Verifying boot state changed...
  hypervisor-1: $ ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new root@10.0.0.5 'printf "boot_id=%s\n" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"; printf "uptime=%s\n" "$(cut -d" " -f1 /proc/uptime 2>/dev/null)"'
  hypervisor-1: [✓] rebooted: boot_id changed (9c2f1a44-3b7e-4d51-9f0a-1b2c3d4e5f60 -> 1d7b9e02-5a3c-4f18-8e6d-7a9b0c1d2e3f)

=== Executing Tier: 2 ===
... (vm-a follows the same probe, reboot, ping, verify sequence) ...
  vm-a: [✓] rebooted: boot_id changed (44e1c0d3-8f2b-4a67-b1c9-0d5e6f708192 -> b83a5c17-6d4e-4029-9c3b-5f1a2e8d47c0)

=== Reboot Verification Summary ===
Confirmed rebooted: 2  Not rebooted: 0  Unverified: 0

All tiers complete. Reboot orchestration finished successfully.
```

A host that answers ping without having actually restarted is called out and
fails the run rather than being reported as a success:

```text
vm-a: [warn] answered every probe; it never left the network
Verifying boot state changed...
  vm-a: [✗] WARNING: did NOT reboot: boot_id is unchanged (44e1c0d3-8f2b-4a67-b1c9-0d5e6f708192); the host never went down

=== Reboot Verification Summary ===
Confirmed rebooted: 1  Not rebooted: 1  Unverified: 0
  vm-a: [✗] boot_id is unchanged (44e1c0d3-8f2b-4a67-b1c9-0d5e6f708192); the host never went down

All tiers complete, but 1 host(s) could not be confirmed as rebooted.
```

### `ansible-inventory-reboot-hosts`

Converts an Ansible YAML inventory into the host specs `reboot-orchestrator`
reads, one per line on stdout.

It exists so the orchestrator needs to know nothing about Ansible. Topology that
already lives in an inventory is translated here and piped across; a fleet with
no inventory at all is described directly on the orchestrator's command line.
The boundary between the two is a stream of host specs anyone can read, diff, or
write by hand.

**Inventory variables read:**

| Variable                    | Becomes                      |
| --------------------------- | ---------------------------- |
| `ip_addr` or `ansible_host` | `addr` (`ip_addr` wins)      |
| `ansible_user`              | `user`                       |
| `ansible_ssh_common_args`   | one `ssh-arg` per shell word |
| `depends_on`                | one `after` per entry        |

Groups nest arbitrarily deep through `children`, and a host appearing in several
groups accumulates the variables from all of them. Dependencies are validated
against the inventory, so a typo is caught before the orchestrator is handed the
topology. Every other inventory variable is ignored rather than rejected — an
inventory that also serves Ansible itself works untouched.

**Example inventory:**

```yaml
all:
  children:
    hypervisors:
      hosts:
        hypervisor-1:
          ip_addr: 10.0.0.5
          ansible_user: root
    guests:
      hosts:
        vm-a:
          ip_addr: 10.0.0.21
          ansible_user: admin
          ansible_ssh_common_args: "-o StrictHostKeyChecking=no"
          depends_on: [hypervisor-1]
        vm-b:
          ip_addr: 10.0.0.22
          depends_on: [hypervisor-1]
    apps:
      hosts:
        web1:
          ip_addr: 10.0.0.30
          depends_on:
            - vm-a
            - vm-b
```

Only the variables in the table above are read; the group names are the
operator's own and carry no meaning here beyond nesting. The output below is
what this inventory produces.

**Usage:**

```shell
ansible-inventory-reboot-hosts [--inventory FILE] | reboot-orchestrator [flags] HOST...
```

**Example Output:**

```text
$ ansible-inventory-reboot-hosts -i inventory.yml
hypervisor-1,addr=10.0.0.5,user=root
vm-a,addr=10.0.0.21,user=admin,ssh-arg=-o,ssh-arg=StrictHostKeyChecking=no,after=hypervisor-1
vm-b,addr=10.0.0.22,after=hypervisor-1
web1,addr=10.0.0.30,after=vm-a,after=vm-b
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
   `golangci-lint` to format and lint your code._

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
versions read from `go/go.mod`, `.python-version` and `mise.toml`, installs
[Claude Code](https://claude.com/claude-code), runs `task setup` for you, and
points the editor at the same linters and formatters `task check` uses. Nothing
is pinned in `.devcontainer/`, so a version bump needs no change there — rebuild
the container, or re-run `./.devcontainer/post-create.sh` in place.

## Development

We use [Task](https://taskfile.dev/) as our build tool. Use `task` at the root
to manage operations across all languages:

- `task check`: Lint, format-check, type-check, and scan all files.
- `task format`: Auto-format and fix all source files.
- `task build`: Build all binaries.
- `task test`: Run all tests.
- `task tidy`: Tidy all module dependencies.

## Repository Structure

- `go/`: Go-based tools and libraries.
  - `go/cmd/container-version-snapshot`: Tool to snapshot container versions via
    SSH.
  - `go/cmd/update-container-manifest`: Tool to bump pinned image versions in an
    `images.yml` manifest to the newest stable within each major.
  - `go/cmd/proxmox-retrust-host-keys`: Tool to re-trust guest SSH host keys,
    verified out-of-band via the hypervisor.
  - `go/cmd/reboot-orchestrator`: Dependency-aware reboot orchestration over
    SSH.
  - `go/cmd/ansible-inventory-reboot-hosts`: Converts an Ansible inventory into
    `reboot-orchestrator` host specs.
- `design/`: Architectural design documents.
- `skills/`: Repository-specific skills and guides.
