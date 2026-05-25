# Reboot Orchestrator

A lightweight, modular, and dependency-aware Python library and CLI tool designed to orchestrate system reboots across network infrastructure.

By utilizing a Directed Acyclic Graph (DAG) built from your inventory declarations, `reboot-orchestrator` ensures that high-priority/network-critical systems (e.g., hypervisors, core switches, DNS hosts) reboot and return online before the systems that depend on them are touched.

---

## Key Features

- **Topological Sequence (DAG)**: Dynamically groups hosts into execution tiers based on topological dependency sorting (Kahn's Algorithm).
- **Direct SSH Execution**: Triggers all reboots gracefully via native parallel SSH commands, avoiding heavy external automation runners or playbooks.
- **ACPI "Zombie" VM Workaround**: Safely handles virtual machines suffering from poweroff bugs by executing pre-flight graceful halts and issuing fallback cut-power commands on the hypervisor host via SSH if they do not halt in time.
- **Asynchronous Reboots**: Dispatches reboot triggers asynchronously so that network/connectivity drops do not hang the orchestrator.
- **Ping Tracking**: Asynchronously tracks host status using continuous ICMP ping loops to guarantee a tier is fully online before moving to the next.

---

## Inventory YAML Data Contract (API)

The orchestrator reads your Ansible `inventory.yml` file as its source of truth. To integrate with the orchestrator, your inventory hosts must define the following variables:

### 1. `ip_addr` or `ansible_host` (Required for targeted hosts)

Used by the reachability monitor to track the host state. If neither is present, pre-flight validation will fail.

```yaml
hosts:
  host-a:
    ip_addr: 10.0.0.10
```

### 2. `depends_on` (Optional List)

A list of hostnames that the current host relies on. The orchestrator uses these to construct the topological execution order.

```yaml
hosts:
  vm-a:
    ip_addr: 10.0.0.21
    depends_on:
      - hypervisor-1
      - dns-server
```

### 3. `proxmox_zombie_workaround` (Optional Object)

Configures a fallback forced shutdown sequence for VMs that fail to completely power off via ACPI signals.

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

The tool is packaged with an executable entrypoint `reboot-orchestrator`. You must explicitly supply the hostnames to reboot as positional arguments.

```bash
reboot-orchestrator [options] host1 host2 [host3 ...]
```

### Options

| Flag                         | Default         | Description                                             |
| ---------------------------- | --------------- | ------------------------------------------------------- |
| `--inventory`, `-i`          | `inventory.yml` | Path to the Ansible inventory file                      |
| `--yes`, `-y`                | `False`         | Bypass interactive confirmation prompt                  |
| `--ping-timeout`             | `1`             | Timeout in seconds for single ping queries              |
| `--wait-drop-seconds`        | `15`            | Seconds to wait for hosts to drop off network           |
| `--zombie-halt-wait-seconds` | `15`            | Seconds to wait for VM graceful halt before forced stop |

---

## Programmatic Python API

You can import `reboot_orchestrator` directly into any Python program for custom integration.

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
orchestrator.run(target_hosts=targets)
```
