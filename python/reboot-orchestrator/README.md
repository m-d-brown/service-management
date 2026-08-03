# Reboot Orchestrator

A lightweight, modular, and dependency-aware Python library and CLI tool
designed to orchestrate system reboots across network infrastructure.

By utilizing a Directed Acyclic Graph (DAG) built from your inventory
declarations, `reboot-orchestrator` ensures that high-priority/network-critical
systems (e.g., hypervisors, core switches, DNS hosts) reboot and return online
before the systems that depend on them are touched.

---

## Key Features

- **Topological Sequence (DAG)**: Dynamically groups hosts into execution tiers
  based on topological dependency sorting (Kahn's Algorithm).
- **Pending-Reboot Detection**: With `--if-needed`, probes the named hosts over
  SSH and reboots only those actually waiting on one, re-checking each tier as
  it comes up so guests already power-cycled by their parent are left alone.
- **Direct SSH Execution**: Triggers all reboots gracefully via native parallel
  SSH commands, avoiding heavy external automation runners or playbooks.
- **ACPI "Zombie" VM Workaround**: Safely handles virtual machines suffering
  from poweroff bugs by executing pre-flight graceful halts and issuing fallback
  cut-power commands on the hypervisor host via SSH if they do not halt in time.
- **Asynchronous Reboots**: Dispatches reboot triggers asynchronously so that
  network/connectivity drops do not hang the orchestrator.
- **Ping Tracking**: Asynchronously tracks host status using continuous ICMP
  ping loops to guarantee a tier is fully online before moving to the next.
- **Boot State Verification**: Records each host's kernel boot ID (or uptime)
  over SSH before the reboot and re-reads it afterwards, so a host that answered
  ping without ever restarting is reported instead of silently passing.
- **Command Transparency**: Every SSH and ping command is echoed as a
  copy-pasteable shell line before it runs.

---

## Verifying That Hosts Actually Rebooted

ICMP reachability alone cannot prove a reboot happened. If the reboot command
never lands — failed SSH authentication, denied `sudo`, a hung shutdown unit —
the host keeps answering pings and looks identical to one that came back up.

Before rebooting a tier, the orchestrator reads two markers over SSH:

- `/proc/sys/kernel/random/boot_id`, regenerated on every boot (authoritative).
- `/proc/uptime`, used for hosts that expose no boot ID (busybox, appliance
  firmware, network gear).

Once the tier is reachable again, both are re-read and compared:

| Outcome              | Meaning                                             | Reported as                   |
| -------------------- | --------------------------------------------------- | ----------------------------- |
| Boot ID changed      | The host restarted                                  | `[✓] host rebooted`           |
| Uptime reset         | The host restarted (no boot ID available)           | `[✓] host rebooted`           |
| Boot ID unchanged    | The host never went down                            | `[✗] WARNING: did NOT reboot` |
| Uptime kept climbing | The host never went down                            | `[✗] WARNING: did NOT reboot` |
| Probe failed/absent  | SSH unreachable, or the host exposes neither marker | `[?] WARNING: unverified`     |

A run ends with a summary of all three categories. The CLI exits non-zero when
any host is proven not to have rebooted; hosts that merely could not be verified
warn without failing the run. Orchestration always continues through remaining
tiers so a partial run is never left half-applied — the summary is the record of
what to retry.

The pre-reboot probe doubles as an SSH pre-flight check: a host that cannot be
probed almost certainly cannot be rebooted over SSH either, and is called out
before the reboot is issued.

Use `--skip-boot-verification` for fleets where SSH-based inspection is not
possible; the tool then falls back to the original ping-only behavior.

---

## Inventory YAML Data Contract (API)

The orchestrator reads your Ansible `inventory.yml` file as its source of truth.
To integrate with the orchestrator, your inventory hosts must define the
following variables:

### 1. `ip_addr` or `ansible_host` (Required for targeted hosts)

Used by the reachability monitor to track the host state. If neither is present,
pre-flight validation will fail.

```yaml
hosts:
  host-a:
    ip_addr: 10.0.0.10
```

### 2. `depends_on` (Optional List)

A list of hostnames that the current host relies on. The orchestrator uses these
to construct the topological execution order.

```yaml
hosts:
  vm-a:
    ip_addr: 10.0.0.21
    depends_on:
      - hypervisor-1
      - dns-server
```

### 3. `proxmox_zombie_workaround` (Optional Object)

Configures a fallback forced shutdown sequence for VMs that fail to completely
power off via ACPI signals.

```yaml
hosts:
  vm-a:
    ip_addr: 10.0.0.21
    proxmox_zombie_workaround:
      delegate_to: hypervisor-1 # The hypervisor hostname running the VM
      vmid: 101 # The Proxmox VM ID
```

---

## CLI Usage

The tool is packaged with an executable entrypoint `reboot-orchestrator`. You
must explicitly supply the hostnames to reboot as positional arguments.

```bash
reboot-orchestrator [options] host1 host2 [host3 ...]
```

### Options

| Flag                         | Default         | Description                                              |
| ---------------------------- | --------------- | -------------------------------------------------------- |
| `--inventory`, `-i`          | `inventory.yml` | Path to the Ansible inventory file                       |
| `--yes`, `-y`                | `False`         | Bypass interactive confirmation prompt                   |
| `--if-needed`                | `False`         | Reboot only the named hosts that have a pending reboot   |
| `--ping-timeout`             | `1`             | Timeout in seconds for single ping queries               |
| `--wait-drop-seconds`        | `15`            | Seconds to wait for hosts to drop off network            |
| `--zombie-halt-wait-seconds` | `15`            | Seconds to wait for VM graceful halt before forced stop  |
| `--skip-boot-verification`   | `False`         | Skip the SSH boot state check that proves hosts rebooted |
| `--probe-timeout-seconds`    | `15`            | Timeout in seconds for each SSH boot state probe         |

### Exit Codes

| Code | Meaning                                                                                                              |
| ---- | -------------------------------------------------------------------------------------------------------------------- |
| `0`  | All tiers completed; every host was checked, and none was proven to have skipped its reboot                          |
| `1`  | Pre-flight validation failed, orchestration errored, a host could not be checked, or a host was proven not to reboot |

---

## Pending-Reboot Detection

By default every host named on the command line is rebooted. With `--if-needed`
the orchestrator first probes them and reboots only those actually waiting on
one, reporting a reason for every host either way.

The probe is a single unprivileged shell script, piped to `/bin/sh` on the
remote's standard input rather than passed as an argument — that keeps it
working regardless of the login shell, which on FreeBSD is commonly csh. It
branches on `uname -s`:

| Platform      | Check                                                                                                      |
| ------------- | ---------------------------------------------------------------------------------------------------------- |
| FreeBSD       | `uname -r` (running kernel) differs from `freebsd-version -k` (installed kernel)                           |
| Debian et al. | `/var/run/reboot-required` exists; `/var/run/reboot-required.pkgs` names the packages, when apt records it |

### Re-checking as tiers advance

Rebooting a parent power-cycles everything nested under it — reboot a hypervisor
and its guests go down and come back with it. A guest's pre-flight verdict is
therefore stale by the time its own tier is reached, and acting on it would
reboot that guest a second time for an update its parent's reboot already
applied.

So each tier after the first is re-probed immediately before it runs. Hosts that
now come back clean are announced and dropped, and a tier emptied that way is
skipped outright — no reboot issued and no ping wait, because nothing went down.
One consequence worth knowing: the dependency tree shown at the confirmation
prompt is the plan as understood before anything has rebooted, so a host listed
there may still be skipped later. The skip is announced when it happens.

A host that cannot be probed is reported and left out of the reboot set — an
unprobed host is never assumed up to date — and makes the process exit non-zero
so a calling script notices something went unchecked.

---

## Programmatic Python API

You can import `reboot_orchestrator` directly into any Python program for custom
integration.

```python
from reboot_orchestrator import RebootOrchestrator, OrchestrationConfig

# 1. Initialize configuration
config = OrchestrationConfig(
    inventory_path="path/to/inventory.yml",
    wait_drop_seconds=10
)

# 2. Instantiate orchestrator
orchestrator = RebootOrchestrator(config)

# 3. Load and parse inventory
inventory = orchestrator.get_inventory()

# 4. Target hosts to reboot
targets = {"hypervisor-1", "vm-a"}

# 5. Validate inventory topology & ping requirements
orchestrator.validate_targets(inventory, targets)

# 6. Execute tiered reboot orchestration
verifications = orchestrator.run(target_hosts=targets)

# 7. Inspect which hosts were proven to have rebooted
for result in verifications:
    print(result.host, result.status.value, result.detail)
```
