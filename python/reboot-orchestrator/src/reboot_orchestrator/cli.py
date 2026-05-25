"""
cli.py - Reboot Orchestrator Command Line Interface

This module provides the main CLI entry point for the reboot-orchestrator tool,
handling argument parsing, error logging, and user confirmation prompts.
"""

import argparse
import sys
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

    print("\nThe following hosts will be rebooted:")
    for h in sorted(target_hosts):
        print(f" - {h}")

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
        orchestrator.run(target_hosts=target_hosts)
    except Exception as e:
        print(f"FATAL: Reboot orchestration failed: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
