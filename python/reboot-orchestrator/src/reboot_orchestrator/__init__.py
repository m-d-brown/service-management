"""
reboot_orchestrator - Homelab Reboot Orchestrator Library

This package provides a modular, well-typed, and fully documented interface for
orchestrating reboots on hosts defined within an Ansible environment.
It calculates dependency graphs (DAG) to execute reboots in dynamic tiers.
"""

from reboot_orchestrator.orchestrator import (
    RebootOrchestrator,
    OrchestrationConfig,
)
from reboot_orchestrator.boot_state import (
    BootState,
    RebootVerification,
    VerificationStatus,
    capture_boot_state,
    verify_reboot,
)
from reboot_orchestrator.inventory import (
    load_inventory,
)
from reboot_orchestrator.ssh import (
    reboot_hosts,
    execute_zombie_workaround,
)
from reboot_orchestrator.ping import (
    ping_host,
    wait_for_hosts,
)

__all__ = [
    "RebootOrchestrator",
    "OrchestrationConfig",
    "BootState",
    "RebootVerification",
    "VerificationStatus",
    "capture_boot_state",
    "verify_reboot",
    "load_inventory",
    "reboot_hosts",
    "execute_zombie_workaround",
    "ping_host",
    "wait_for_hosts",
]
