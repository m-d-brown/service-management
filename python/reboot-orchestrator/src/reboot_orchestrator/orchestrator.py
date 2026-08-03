"""
orchestrator.py - Reboot Orchestrator Core Logic

This module provides the main RebootOrchestrator engine and its configuration classes,
enabling robust, dependency-aware, tiered reboot management.
"""

from dataclasses import dataclass
from typing import Any, Callable, Optional
from reboot_orchestrator.boot_state import (
    BootState,
    RebootVerification,
    VerificationStatus,
    capture_boot_state,
    verify_reboot,
)
from reboot_orchestrator.inventory import load_inventory
from reboot_orchestrator.ssh import reboot_hosts, execute_zombie_workaround
from reboot_orchestrator.ping import wait_for_hosts


@dataclass(frozen=True)
class OrchestrationConfig:
    """
    Configuration parameters for customizing the RebootOrchestrator runtime behavior.
    """

    inventory_path: str = "inventory.yml"
    ping_timeout: int = 1
    wait_drop_seconds: int = 15
    zombie_halt_wait_seconds: int = 15
    verify_boot_state: bool = True
    probe_timeout_seconds: int = 15


class RebootOrchestrator:
    """
    Orchestrates the reboot of targeted infrastructure hosts in a topologically
    sorted order determined by host-level dependency settings.
    """

    def __init__(self, config: OrchestrationConfig) -> None:
        """
        Initializes the RebootOrchestrator with the specified configuration.

        Args:
            config: An instance of OrchestrationConfig specifying runtime paths and options.
        """
        self.config = config

    def get_inventory(self) -> dict[str, dict[str, Any]]:
        """
        Loads and flattens the Ansible inventory configured in the system.

        Returns:
            dict[str, dict[str, Any]]: Flattened inventory map of hosts to properties.
        """
        return load_inventory(self.config.inventory_path)

    def build_execution_tiers(
        self, active_queue: set[str], inventory: dict[str, dict[str, Any]]
    ) -> dict[int, list[str]]:
        """
        Computes execution tiers using Kahn's topological sort algorithm based on the
        depends_on parameter declared in the host properties.

        Args:
            active_queue: Set of hostnames currently targeted for reboot execution.
            inventory: The flattened inventory mapping of hostnames to properties.

        Returns:
            dict[int, list[str]]: A mapping from tier number (int) to list of hostnames.

        Raises:
            ValueError: If a cyclic dependency is detected.
        """
        # Build dependency graph containing only targets slated for execution
        graph = {
            h: {
                dep
                for dep in inventory.get(h, {}).get("depends_on") or []
                if dep in active_queue
            }
            for h in active_queue
        }

        tier_num = 1
        tier_map: dict[int, list[str]] = {}

        while graph:
            # Hosts with zero active dependencies in the current queue go in the current tier
            current_tier = [h for h, deps in graph.items() if not deps]
            if not current_tier:
                raise ValueError(
                    "Cyclic dependency detected in inventory (depends_on)."
                )

            # Sort the tier deterministically
            current_tier.sort()
            tier_map[tier_num] = current_tier

            # Remove sorted hosts from the graph and remaining dependency lists
            for h in current_tier:
                del graph[h]
            for h in graph:
                graph[h].difference_update(current_tier)

            tier_num += 1

        return tier_map

    def validate_targets(
        self, inventory: dict[str, dict[str, Any]], target_hosts: set[str]
    ) -> None:
        """
        Validates target hosts exist and have a pingable identifier in the inventory.

        Args:
            inventory: The flattened inventory mapping of hostnames to properties.
            target_hosts: The set of hostnames targeted for reboot orchestration.

        Raises:
            ValueError: If any target verification assumption is violated.
        """
        for host in target_hosts:
            if host not in inventory:
                raise ValueError(f"Target host '{host}' is missing from the inventory.")
            props = inventory[host]
            if "ip_addr" not in props and "ansible_host" not in props:
                raise ValueError(
                    f"Host '{host}' is missing 'ip_addr' or 'ansible_host' in the inventory. "
                    "Cannot monitor via ping."
                )

    def print_dependency_tree(
        self, target_hosts: set[str], inventory: dict[str, dict[str, Any]]
    ) -> None:
        """
        Prints a beautiful, hierarchical tree representation of the targeted hosts
        showing their topological reboot order and dependencies.

        Args:
            target_hosts: The set of hostnames targeted for reboot execution.
            inventory: The flattened inventory mapping of hostnames to properties.
        """
        # Build a mapping of parent -> set of child targeted dependents
        dependents: dict[str, list[str]] = {}
        for h in target_hosts:
            for dep in inventory.get(h, {}).get("depends_on") or []:
                if dep in target_hosts:
                    dependents.setdefault(dep, []).append(h)

        # Roots are targeted hosts that do not depend on any other targeted host in the queue
        roots = [
            h
            for h in target_hosts
            if not any(
                dep in target_hosts
                for dep in inventory.get(h, {}).get("depends_on") or []
            )
        ]
        roots.sort()

        visited: set[str] = set()

        def _print_node(node: str, prefix: str = "", is_last: bool = True) -> None:
            connector = "└── " if is_last else "├── "
            if node in visited:
                print(f"{prefix}{connector}{node} (already listed)")
                return
            print(f"{prefix}{connector}{node}")
            visited.add(node)

            children = sorted(dependents.get(node, []))
            if not children:
                return

            new_prefix = prefix + ("    " if is_last else "│   ")
            for i, child in enumerate(children):
                _print_node(
                    child,
                    prefix=new_prefix,
                    is_last=(i == len(children) - 1),
                )

        for i, root in enumerate(roots):
            _print_node(root, is_last=(i == len(roots) - 1))

    def capture_baselines(
        self, hosts: list[str], inventory: dict[str, dict[str, Any]]
    ) -> dict[str, BootState | None]:
        """
        Records the pre-reboot boot identity of each host in a tier.

        Doubles as an SSH pre-flight check: a host that cannot be probed here is
        unlikely to accept the reboot command either, so it is called out early.

        Args:
            hosts: Hostnames about to be rebooted.
            inventory: The flattened inventory mapping of hostnames to properties.

        Returns:
            dict[str, BootState | None]: Baseline reading per host (None on failure).
        """
        print("Recording pre-reboot boot state...")
        baselines: dict[str, BootState | None] = {}
        for host in hosts:
            state = capture_boot_state(
                host=host,
                inventory=inventory,
                probe_timeout_seconds=self.config.probe_timeout_seconds,
            )
            if state is None:
                print(
                    f"  WARNING: Cannot read the boot state of '{host}' over SSH. "
                    "The reboot command will likely fail the same way, and the "
                    "reboot cannot be verified."
                )
            baselines[host] = state
        return baselines

    def verify_tier(
        self,
        hosts: list[str],
        baselines: dict[str, BootState | None],
        inventory: dict[str, dict[str, Any]],
    ) -> list[RebootVerification]:
        """
        Re-reads the boot identity of each host in a tier and compares it against
        the recorded baseline to prove the reboot happened.

        Args:
            hosts: Hostnames that were rebooted.
            baselines: Pre-reboot readings from capture_baselines.
            inventory: The flattened inventory mapping of hostnames to properties.

        Returns:
            list[RebootVerification]: One verdict per host, in the given order.
        """
        print("Verifying boot state changed...")
        results: list[RebootVerification] = []
        for host in hosts:
            after = capture_boot_state(
                host=host,
                inventory=inventory,
                probe_timeout_seconds=self.config.probe_timeout_seconds,
            )
            result = verify_reboot(host, baselines.get(host), after)
            if result.status is VerificationStatus.CONFIRMED:
                print(f"[✓] {host} rebooted: {result.detail}")
            elif result.status is VerificationStatus.NOT_REBOOTED:
                print(f"[✗] WARNING: {host} did NOT reboot: {result.detail}")
            else:
                print(f"[?] WARNING: {host} reboot unverified: {result.detail}")
            results.append(result)
        return results

    def print_verification_summary(self, results: list[RebootVerification]) -> None:
        """
        Prints a closing report grouping hosts by verification outcome.

        Args:
            results: Every verdict produced during the run.
        """
        if not results:
            return

        confirmed = [r for r in results if r.status is VerificationStatus.CONFIRMED]
        failed = [r for r in results if r.status is VerificationStatus.NOT_REBOOTED]
        unknown = [r for r in results if r.status is VerificationStatus.UNKNOWN]

        print("\n=== Reboot Verification Summary ===")
        print(
            f"Confirmed rebooted: {len(confirmed)}  "
            f"Not rebooted: {len(failed)}  "
            f"Unverified: {len(unknown)}"
        )
        for result in failed:
            print(f"  [✗] {result.host}: {result.detail}")
        for result in unknown:
            print(f"  [?] {result.host}: {result.detail}")

    def run(
        self,
        target_hosts: set[str],
        recheck: Optional[Callable[[list[str]], list[str]]] = None,
    ) -> list[RebootVerification]:
        """
        Executes the full tiered reboot orchestration workflow.

        1. Loads the inventory.
        2. Validates topology and monitorability.
        3. Sorts hosts into dynamic execution tiers.
        4. Executes tiered reboots sequentially, performing pre-flight zombie VM
           interventions as needed and waiting for recovery.
        5. Verifies each host's boot identity actually changed, warning loudly
           when a host came back without having rebooted.

        Args:
            target_hosts: The set of hostnames targeted for reboot execution.
            recheck: Optional callable narrowing a tier to the hosts that still
                     need a reboot, applied immediately before each tier after
                     the first. Rebooting a parent power-cycles everything
                     nested under it, so by the time a child's tier is reached
                     its earlier verdict may no longer hold; without this the
                     child would be rebooted a second time for an update its
                     parent's reboot already applied. The first tier is not
                     re-checked — nothing has gone down yet, so the caller's own
                     probe still stands.

        Returns:
            list[RebootVerification]: One verdict per rebooted host — hosts the
            re-check skipped are not included, having never been rebooted. Empty
            when verification is disabled or no hosts were targeted.
        """
        if not target_hosts:
            print("No hosts targeted for reboot.")
            return []

        inventory = self.get_inventory()
        self.validate_targets(inventory, target_hosts)

        active_queue = set(target_hosts)
        tier_map = self.build_execution_tiers(active_queue, inventory)
        executed_workarounds: set[str] = set()
        verifications: list[RebootVerification] = []

        for position, tier_num in enumerate(sorted(tier_map.keys())):
            tier_hosts = tier_map[tier_num]
            print(f"\n=== Executing Tier: {tier_num} ===")

            # Re-check everything but the first tier: the tiers before this one
            # have rebooted, taking their nested dependents down and back up
            # with them, which may already have applied what these hosts were
            # queued for. This runs before the baseline capture below so a host
            # dropped here is never probed for a boot state it will not change,
            # and never turns up in the verification summary as a host that
            # failed to reboot — it was deliberately not rebooted.
            if recheck is not None and position > 0:
                still_pending = set(recheck(tier_hosts))
                for host in tier_hosts:
                    if host not in still_pending:
                        print(f"Skipping {host}: no longer needs a reboot.")
                tier_hosts = [h for h in tier_hosts if h in still_pending]

                if not tier_hosts:
                    print(f"Tier {tier_num} is already up to date; nothing to do.")
                    for h in tier_map[tier_num]:
                        active_queue.discard(h)
                    continue

            # Record the baseline before anything powers the hosts down, so that
            # zombie VM workarounds do not race the probe.
            baselines: dict[str, BootState | None] = {}
            if self.config.verify_boot_state:
                baselines = self.capture_baselines(tier_hosts, inventory)

            # Execute zombie VM workarounds if necessary.
            # Trigger pre-flight VM shutdowns if the VM or its hypervisor is rebooting in this tier.
            for h in active_queue:
                if h in executed_workarounds:
                    continue
                workaround = inventory.get(h, {}).get("proxmox_zombie_workaround")
                if not workaround:
                    continue

                delegate = workaround.get("delegate_to")
                if h in tier_hosts or delegate in tier_hosts:
                    execute_zombie_workaround(
                        host=h,
                        workaround=workaround,
                        inventory=inventory,
                        zombie_halt_wait_seconds=self.config.zombie_halt_wait_seconds,
                    )
                    executed_workarounds.add(h)

            # Trigger reboots
            reboot_hosts(hosts=tier_hosts, inventory=inventory)

            # Wait for completion: Wait for the rebooted hosts and all their direct dependents to return online.
            wait_list = set(tier_hosts)
            for h in tier_hosts:
                wait_list.update(
                    dep
                    for dep, props in inventory.items()
                    if h in props.get("depends_on", [])
                )

            wait_for_hosts(
                hosts_to_wait=list(wait_list),
                inventory=inventory,
                wait_drop_seconds=self.config.wait_drop_seconds,
                ping_timeout=self.config.ping_timeout,
            )

            # Reachability alone does not prove a reboot: confirm the boot
            # identity changed before moving on to dependent tiers. Only the
            # hosts that survived the re-check are verified — they are the only
            # ones that were actually rebooted.
            if self.config.verify_boot_state:
                verifications.extend(self.verify_tier(tier_hosts, baselines, inventory))

            # Clean up queue. Drain the tier as originally planned, not the
            # filtered list, so anything the re-check skipped leaves too.
            for h in tier_map[tier_num]:
                active_queue.discard(h)

        self.print_verification_summary(verifications)

        unconfirmed = [
            r for r in verifications if r.status is not VerificationStatus.CONFIRMED
        ]
        if unconfirmed:
            print(
                "\nAll tiers complete, but "
                f"{len(unconfirmed)} host(s) could not be confirmed as rebooted."
            )
        else:
            print("\nAll tiers complete. Reboot orchestration finished successfully.")

        return verifications
