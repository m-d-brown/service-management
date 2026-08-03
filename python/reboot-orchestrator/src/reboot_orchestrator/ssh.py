"""
ssh.py - Direct SSH Execution Interface

This module encapsulates all external system calls via SSH,
providing Python interfaces for rebooting hosts and running hypervisor
management tasks without relying on Ansible execution.
"""

import subprocess
import shlex
import time
from typing import Any


def format_command(cmd: list[str]) -> str:
    """
    Renders an argument vector as a single copy-pasteable shell command string.

    Args:
        cmd: The argument vector as passed to subprocess.

    Returns:
        str: The equivalent quoted shell command line.
    """
    return shlex.join(cmd)


def get_ssh_target_and_args(
    host: str, inventory: dict[str, dict[str, Any]]
) -> tuple[str, list[str]]:
    """
    Resolves the SSH target (user@ip) and common SSH arguments for a host from the inventory.

    Args:
        host: Hostname of the target.
        inventory: Flattened inventory map of hosts to properties.

    Returns:
        tuple[str, list[str]]: SSH target string and list of SSH CLI arguments.
    """
    props = inventory.get(host, {})
    ip = props.get("ip_addr") or props.get("ansible_host") or host
    user = props.get("ansible_user")
    target = f"{user}@{ip}" if user else ip

    ssh_args = [
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=5",
        "-o",
        "StrictHostKeyChecking=accept-new",
    ]

    common_args = props.get("ansible_ssh_common_args")
    if common_args:
        ssh_args.extend(shlex.split(common_args))

    return target, ssh_args


def reboot_hosts(
    hosts: list[str],
    inventory: dict[str, dict[str, Any]],
) -> None:
    """
    Triggers an asynchronous reboot command on a list of hosts via direct SSH.
    Asynchronous execution ensures that connection drops (expected when rebooting
    network components or routers) do not block the orchestrator process.

    Args:
        hosts: List of hostnames to reboot.
        inventory: Flattened inventory map of hosts to properties.
    """
    if not hosts:
        return

    print(f"Issuing reboot command to: {', '.join(hosts)}")
    for host in hosts:
        target, ssh_args = get_ssh_target_and_args(host, inventory)
        # We run the command with Popen so it is asynchronous and does not block.
        # "sudo reboot || reboot" ensures it works whether running as root or a sudoer.
        cmd = ["ssh"] + ssh_args + [target, "sudo reboot || reboot"]
        print(f"  $ {format_command(cmd)}")
        try:
            # We use Popen and do not wait for it, letting it run in the background.
            # stdout/stderr are redirected to DEVNULL so they don't pollute the console on connection drop.
            # A reboot that silently fails here is caught later by boot state verification.
            subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        except Exception as e:
            print(f"Failed to issue SSH reboot to {host}: {e}")


def execute_zombie_workaround(
    host: str,
    workaround: dict[str, Any],
    inventory: dict[str, dict[str, Any]],
    zombie_halt_wait_seconds: int = 15,
) -> None:
    """
    Handles virtual machines suffering from ACPI shutdown bugs by triggering
    a graceful halt command via SSH, waiting for it to power down, and running a fallback
    forced-stop command on the hypervisor host via SSH if necessary.

    Args:
        host: Hostname of the target VM.
        workaround: Dictionary representing the zombie VM configuration:
                    - 'delegate_to': Hypervisor hostname (e.g. Proxmox host)
                    - 'vmid': Proxmox VM ID (int or str)
        inventory: Flattened inventory map of hosts to properties.
        zombie_halt_wait_seconds: Number of seconds to wait before forcing a shutdown.
    """
    delegate = workaround.get("delegate_to")
    vmid = workaround.get("vmid")

    if not delegate or not vmid:
        print(
            f"WARNING: Host '{host}' is missing delegate_to or vmid in zombie VM workaround config."
        )
        return

    print(f"Executing pre-flight graceful halt for '{host}' via SSH...")
    vm_target, vm_ssh_args = get_ssh_target_and_args(host, inventory)
    vm_cmd = ["ssh"] + vm_ssh_args + [vm_target, "sudo poweroff || poweroff"]
    print(f"  $ {format_command(vm_cmd)}")
    try:
        subprocess.Popen(vm_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except Exception as e:
        print(f"Failed to issue SSH poweroff to {host}: {e}")

    print(f"Waiting {zombie_halt_wait_seconds}s for '{host}' to power down...")
    time.sleep(zombie_halt_wait_seconds)

    print(
        f"Verifying VM state and enforcing fallback stop command on hypervisor '{delegate}' via SSH..."
    )
    hypervisor_target, hypervisor_ssh_args = get_ssh_target_and_args(
        delegate, inventory
    )
    hypervisor_cmd = (
        ["ssh"]
        + hypervisor_ssh_args
        + [hypervisor_target, f"sudo qm stop {vmid} || qm stop {vmid}"]
    )
    print(f"  $ {format_command(hypervisor_cmd)}")
    try:
        res = subprocess.run(
            hypervisor_cmd, capture_output=True, text=True, check=False
        )
        if res.returncode != 0:
            print(
                f"Hypervisor stop command returned code {res.returncode}. Error: {res.stderr.strip()}"
            )
    except Exception as e:
        print(
            f"Failed to execute hypervisor stop command for {host} on {delegate}: {e}"
        )
