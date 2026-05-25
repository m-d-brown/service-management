"""
test_ssh.py - Unit Test Suite for Reboot Orchestrator SSH Execution Module
"""

from typing import Any
from unittest.mock import MagicMock, patch
from reboot_orchestrator.ssh import (
    get_ssh_target_and_args,
    reboot_hosts,
    execute_zombie_workaround,
)


def test_get_ssh_target_and_args() -> None:
    """
    Checks that SSH target and command args are parsed correctly from properties.
    """
    inventory = {
        "host-a": {
            "ip_addr": "192.168.1.100",
            "ansible_user": "root",
            "ansible_ssh_common_args": "-o StrictHostKeyChecking=no -p 2222",
        },
        "host-b": {
            "ansible_host": "192.168.1.101",
        },
    }

    # Case 1: Root user, explicit common args
    target, args = get_ssh_target_and_args("host-a", inventory)
    assert target == "root@192.168.1.100"
    assert "StrictHostKeyChecking=no" in args
    assert "2222" in args

    # Case 2: No user, ansible_host fallback
    target, args = get_ssh_target_and_args("host-b", inventory)
    assert target == "192.168.1.101"
    assert "-o" in args


@patch("subprocess.Popen")
def test_reboot_hosts_ssh(mock_popen: MagicMock) -> None:
    """
    Verifies reboot_hosts correctly starts Popen with the constructed SSH reboot commands.
    """
    inventory = {
        "host-a": {"ip_addr": "10.0.0.10", "ansible_user": "admin"},
        "host-b": {"ip_addr": "10.0.0.11"},
    }

    reboot_hosts(["host-a", "host-b"], inventory)

    assert mock_popen.call_count == 2
    called_args_1 = mock_popen.call_args_list[0][0][0]
    called_args_2 = mock_popen.call_args_list[1][0][0]

    assert "ssh" in called_args_1
    assert "admin@10.0.0.10" in called_args_1
    assert "sudo reboot || reboot" in called_args_1

    assert "ssh" in called_args_2
    assert "10.0.0.11" in called_args_2
    assert "sudo reboot || reboot" in called_args_2


@patch("subprocess.Popen")
@patch("subprocess.run")
@patch("time.sleep")
def test_execute_zombie_workaround_ssh(
    mock_sleep: MagicMock, mock_run: MagicMock, mock_popen: MagicMock
) -> None:
    """
    Checks that the ACPI VM zombie workaround runs poweroff and fallback hypervisor stop commands.
    """
    inventory: dict[str, dict[str, Any]] = {
        "vm-1": {
            "ip_addr": "10.0.0.20",
            "proxmox_zombie_workaround": {
                "delegate_to": "hypervisor-1",
                "vmid": 105,
            },
        },
        "hypervisor-1": {"ip_addr": "10.0.0.5", "ansible_user": "root"},
    }

    workaround = inventory["vm-1"]["proxmox_zombie_workaround"]
    mock_run.return_value.returncode = 0

    execute_zombie_workaround("vm-1", workaround, inventory, 10)

    # 1. SSH Poweroff triggered on VM
    mock_popen.assert_called_once()
    called_vm_cmd = mock_popen.call_args[0][0]
    assert "10.0.0.20" in called_vm_cmd
    assert "sudo poweroff || poweroff" in called_vm_cmd

    # 2. Slept for wait period
    mock_sleep.assert_called_once_with(10)

    # 3. Forced stop triggered on hypervisor
    mock_run.assert_called_once()
    called_hv_cmd = mock_run.call_args[0][0]
    assert "root@10.0.0.5" in called_hv_cmd
    assert "sudo qm stop 105 || qm stop 105" in called_hv_cmd
