"""
ping.py - Host Reachability Monitor

This module manages checking if hosts are reachable on the network via ICMP ping
and blocking execution until hosts return online.
"""

from typing import Any
import subprocess
import time


def ping_host(ip_or_host: str, timeout: int = 1) -> bool:
    """
    Pings a host address once to check if it is reachable.

    Args:
        ip_or_host: The IP address or hostname to ping.
        timeout: Timeout in seconds for the ping response.

    Returns:
        bool: True if the host responds to the ping, False otherwise.
    """
    cmd = ["ping", "-c", "1", "-W", str(timeout), ip_or_host]
    res = subprocess.run(cmd, capture_output=True, check=False)
    return res.returncode == 0


def wait_for_hosts(
    hosts_to_wait: list[str],
    inventory: dict[str, dict[str, Any]],
    wait_drop_seconds: int = 15,
    ping_timeout: int = 1,
) -> None:
    """
    Blocks execution until all specified hosts successfully respond to ICMP pings.
    Provides an initial delay to let hosts drop off the network before checking.

    Args:
        hosts_to_wait: List of hostnames to wait for.
        inventory: The flattened inventory mapping of hostnames to properties.
        wait_drop_seconds: Time to sleep before sending pings, giving hosts time to shut down.
        ping_timeout: Timeout in seconds for each individual ping query.
    """
    if not hosts_to_wait:
        return

    # Safety delay: Give the hosts time to shut down and disconnect.
    # Otherwise, we might receive successful pings from a host that has not
    # yet finished powering off, falsely assuming it has already rebooted.
    print(f"Waiting {wait_drop_seconds} seconds for hosts to drop off the network...")
    time.sleep(wait_drop_seconds)

    print(f"Waiting for {', '.join(hosts_to_wait)} to return online...")
    pending = set(hosts_to_wait)

    # Map hostname to its pingable target (ip_addr, ansible_host, or fallback to hostname)
    ip_map: dict[str, str] = {}
    for h in pending:
        props = inventory.get(h, {})
        ip_map[h] = props.get("ip_addr") or props.get("ansible_host") or h

    # Continuous ping loop until all pending hosts are resolved
    while pending:
        for h in list(pending):
            if ping_host(ip_map[h], timeout=ping_timeout):
                print(f"[✓] {h} is back online!")
                pending.remove(h)
        if pending:
            time.sleep(2)
