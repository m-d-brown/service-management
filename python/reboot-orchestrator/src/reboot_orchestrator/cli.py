"""
cli.py - Reboot Orchestrator Command Line Interface

This module provides the main CLI entry point for the reboot-orchestrator tool,
handling argument parsing, error logging, and user confirmation prompts.
"""

import argparse
import sys
from reboot_orchestrator.boot_state import VerificationStatus
from reboot_orchestrator.detect import (
    RebootStatus,
    make_recheck,
    print_report,
    probe_hosts,
)
from reboot_orchestrator.orchestrator import RebootOrchestrator, OrchestrationConfig


def main() -> None:
    """
    Main CLI entrypoint. Parses command line arguments, instantiates the orchestrator,
    prompts the user for confirmation (unless bypassed), and runs the execution.
    """
    parser = argparse.ArgumentParser(
        description="Topological dependency-aware reboot orchestrator."
    )
    parser.add_argument(
        "--inventory",
        "-i",
        default="inventory.yml",
        help="Path to the Ansible inventory file (default: inventory.yml)",
    )
    parser.add_argument(
        "--yes",
        "-y",
        action="store_true",
        help="Bypass interactive execution confirmation",
    )
    parser.add_argument(
        "--if-needed",
        action="store_true",
        help=(
            "Probe the named hosts and reboot only those with a pending "
            "reboot, instead of rebooting all of them"
        ),
    )
    parser.add_argument(
        "--ping-timeout",
        type=int,
        default=1,
        help="Timeout in seconds for single ping queries (default: 1)",
    )
    parser.add_argument(
        "--wait-drop-seconds",
        type=int,
        default=15,
        help="Number of seconds to wait for hosts to drop off the network (default: 15)",
    )
    parser.add_argument(
        "--zombie-halt-wait-seconds",
        type=int,
        default=15,
        help="Number of seconds to wait for VM graceful halt before forced stop (default: 15)",
    )
    parser.add_argument(
        "--skip-boot-verification",
        action="store_true",
        help="Do not read boot_id/uptime over SSH to confirm hosts actually rebooted",
    )
    parser.add_argument(
        "--probe-timeout-seconds",
        type=int,
        default=15,
        help="Timeout in seconds for each SSH boot state probe (default: 15)",
    )
    parser.add_argument(
        "hosts",
        nargs="+",
        help="One or more hostnames to reboot in topologically sequenced order",
    )

    args = parser.parse_args()

    config = OrchestrationConfig(
        inventory_path=args.inventory,
        ping_timeout=args.ping_timeout,
        wait_drop_seconds=args.wait_drop_seconds,
        zombie_halt_wait_seconds=args.zombie_halt_wait_seconds,
        verify_boot_state=not args.skip_boot_verification,
        probe_timeout_seconds=args.probe_timeout_seconds,
    )

    orchestrator = RebootOrchestrator(config)

    try:
        inventory = orchestrator.get_inventory()
    except FileNotFoundError as e:
        print(f"FATAL: {e}", file=sys.stderr)
        sys.exit(1)

    target_hosts = set(args.hosts)

    try:
        orchestrator.validate_targets(inventory, target_hosts)
    except ValueError as e:
        print(f"FATAL: Pre-flight validation failed: {e}", file=sys.stderr)
        sys.exit(1)

    # Hosts that could not be probed, at either the initial check or a per-tier
    # re-check. They are excluded from the reboot set — an unprobed host is not
    # known to need one — but they make the run exit non-zero so a caller
    # driving this from a script notices something went unchecked.
    unprobed: list[RebootStatus] = []
    recheck = None

    if args.if_needed:
        statuses = probe_hosts(sorted(target_hosts), inventory)
        print_report(statuses)
        unprobed.extend(s for s in statuses if s.needs_reboot is None)
        target_hosts = {s.host for s in statuses if s.needs_reboot is True}

        if not target_hosts:
            print("\nNothing to reboot.")
            sys.exit(1 if unprobed else 0)

        recheck = make_recheck(inventory, on_unprobed=unprobed.append)

    print(
        "\nThe following hosts will be rebooted (parents first, nested dependents last):"
    )
    orchestrator.print_dependency_tree(target_hosts, inventory)

    if not args.yes:
        try:
            ans = input("\nProceed with tiered orchestration? [y/N]: ")
            if ans.lower() != "y":
                print("Aborting.")
                sys.exit(0)
        except KeyboardInterrupt:
            print("\nAborting.")
            sys.exit(1)

    try:
        verifications = orchestrator.run(target_hosts=target_hosts, recheck=recheck)
    except Exception as e:
        print(f"FATAL: Reboot orchestration failed: {e}", file=sys.stderr)
        sys.exit(1)

    # Two distinct ways a run can be unsound, both fatal to the exit status: a
    # host we could not check at all, and a host we rebooted that provably did
    # not come back on a new boot.
    if unprobed:
        print("\nWARNING: these hosts could not be checked:", file=sys.stderr)
        for status in unprobed:
            print(f"  {status.host} — {status.reason}", file=sys.stderr)

    # A host proven not to have rebooted is a failed run, even though every tier
    # completed and every host answered ping.
    not_rebooted = [
        r for r in verifications if r.status is VerificationStatus.NOT_REBOOTED
    ]
    if not_rebooted:
        hosts = ", ".join(r.host for r in not_rebooted)
        print(
            f"FATAL: These hosts never rebooted: {hosts}",
            file=sys.stderr,
        )

    if unprobed or not_rebooted:
        sys.exit(1)


if __name__ == "__main__":
    main()
