"""
test_ping.py - Unit Test Suite for Reboot Orchestrator Ping Reachability Module
"""

from unittest.mock import MagicMock, patch
from reboot_orchestrator.ping import wait_for_hosts


@patch("time.sleep")
def test_wait_for_hosts(mock_sleep: MagicMock) -> None:
    """
    Checks that the ping reachability loop continues checking until all targeted
    hosts return online.
    """
    inventory = {
        "host-a": {"ip_addr": "10.0.0.1"},
        "host-b": {},  # Should fallback to pinging the hostname string "host-b"
    }

    # Simulate first ping checks failing, second check succeeding
    ping_results = [False, False, True, True]

    def mock_ping(ip: str, timeout: int = 1) -> bool:
        return ping_results.pop(0)

    with patch("reboot_orchestrator.ping.ping_host", side_effect=mock_ping):
        wait_for_hosts(["host-a", "host-b"], inventory, wait_drop_seconds=5)

    # Sleep should have been called twice (once for initial drop, once for the retry loop)
    assert mock_sleep.call_count == 2
