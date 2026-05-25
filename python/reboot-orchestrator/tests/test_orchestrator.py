"""
test_orchestrator.py - Unit Test Suite for RebootOrchestrator Engine
"""

from typing import Any
from unittest.mock import MagicMock, patch
import pytest

from reboot_orchestrator import RebootOrchestrator, OrchestrationConfig


def test_build_execution_tiers_simple() -> None:
    """
    Ensures Kahn's topological sort correctly groups hosts into sequential tiers
    based on their declared dependencies.
    """
    inventory = {
        "hypervisor-1": {},
        "gateway-1": {"depends_on": ["hypervisor-1"]},
        "vm-a": {"depends_on": ["gateway-1"]},
        "dns-server": {"depends_on": ["vm-a"]},
        "app-server": {"depends_on": ["vm-a", "dns-server"]},
    }
    active_queue = {
        "hypervisor-1",
        "gateway-1",
        "vm-a",
        "dns-server",
        "app-server",
    }

    orchestrator = RebootOrchestrator(OrchestrationConfig())
    tiers = orchestrator.build_execution_tiers(active_queue, inventory)

    assert tiers[1] == ["hypervisor-1"]
    assert tiers[2] == ["gateway-1"]
    assert tiers[3] == ["vm-a"]
    assert tiers[4] == ["dns-server"]
    assert tiers[5] == ["app-server"]


def test_build_execution_tiers_missing_dependency() -> None:
    """
    Verifies that if a dependency is NOT scheduled for reboot (i.e. not in the active queue),
    it does not block the dependent host from being processed in the initial tier.
    """
    inventory = {
        "vm-a": {"depends_on": ["gateway-1"]},
        "dns-server": {"depends_on": ["vm-a"]},
    }
    # gateway-1 is not in the active queue needing reboots
    active_queue = {"vm-a", "dns-server"}

    orchestrator = RebootOrchestrator(OrchestrationConfig())
    tiers = orchestrator.build_execution_tiers(active_queue, inventory)

    assert tiers[1] == ["vm-a"]
    assert tiers[2] == ["dns-server"]


def test_build_execution_tiers_cyclic() -> None:
    """
    Ensures that cyclic dependency loops are caught and trigger a ValueError.
    """
    inventory = {
        "host-a": {"depends_on": ["host-b"]},
        "host-b": {"depends_on": ["host-c"]},
        "host-c": {"depends_on": ["host-a"]},
    }
    active_queue = {"host-a", "host-b", "host-c"}

    orchestrator = RebootOrchestrator(OrchestrationConfig())
    with pytest.raises(ValueError, match="Cyclic dependency detected"):
        orchestrator.build_execution_tiers(active_queue, inventory)


def test_validate_targets_valid() -> None:
    """
    Validates that correct topology declarations pass inventory validation.
    """
    inventory: dict[str, dict[str, Any]] = {
        "host-a": {"ip_addr": "10.0.0.1"},
        "host-b": {"ansible_host": "10.0.0.2", "depends_on": ["host-a"]},
    }
    orchestrator = RebootOrchestrator(OrchestrationConfig())
    orchestrator.validate_targets(inventory, {"host-a", "host-b"})


def test_validate_targets_missing_host() -> None:
    """
    Verifies validation fails if a targeted host is not present in the inventory.
    """
    inventory = {"host-a": {"ip_addr": "10.0.0.1"}}
    orchestrator = RebootOrchestrator(OrchestrationConfig())
    with pytest.raises(ValueError, match="Target host 'host-b' is missing"):
        orchestrator.validate_targets(inventory, {"host-a", "host-b"})


def test_validate_targets_missing_ping_target() -> None:
    """
    Verifies validation fails if a targeted host is missing both ip_addr and ansible_host.
    """
    inventory: dict[str, dict[str, Any]] = {"host-a": {}}
    orchestrator = RebootOrchestrator(OrchestrationConfig())
    with pytest.raises(ValueError, match="missing 'ip_addr' or 'ansible_host'"):
        orchestrator.validate_targets(inventory, {"host-a"})


@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.execute_zombie_workaround")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_orchestrator_run_flow(
    mock_load: MagicMock,
    mock_zombie: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
) -> None:
    """
    End-to-end unit test checking the sequential execution flow across tiers.
    """
    inventory = {
        "hypervisor-1": {"ip_addr": "10.0.0.5"},
        "vm-a": {
            "ip_addr": "10.0.0.21",
            "depends_on": ["hypervisor-1"],
            "proxmox_zombie_workaround": {
                "delegate_to": "hypervisor-1",
                "vmid": 101,
            },
        },
    }
    mock_load.return_value = inventory

    config = OrchestrationConfig(inventory_path="inventory.yml")
    orchestrator = RebootOrchestrator(config)

    orchestrator.run({"hypervisor-1", "vm-a"})

    # Check 1: loaded inventory
    mock_load.assert_called_once_with("inventory.yml")

    # Check 2: Zombie vm pre-flight shutdown triggered
    mock_zombie.assert_called_once_with(
        host="vm-a",
        workaround={"delegate_to": "hypervisor-1", "vmid": 101},
        inventory=inventory,
        zombie_halt_wait_seconds=15,
    )

    # Check 3: Reboot hosts called for each tier (Tier 1: hypervisor, Tier 2: vm-a)
    assert mock_reboot.call_count == 2
    mock_reboot.assert_any_call(hosts=["hypervisor-1"], inventory=inventory)
    mock_reboot.assert_any_call(hosts=["vm-a"], inventory=inventory)

    # Check 4: Wait reachability checks called
    assert mock_wait.call_count == 2


def test_print_dependency_tree(capsys: pytest.CaptureFixture[str]) -> None:
    """
    Verifies that print_dependency_tree produces the expected structured tree output.
    """
    inventory = {
        "hypervisor-1": {},
        "vm-a": {"depends_on": ["hypervisor-1"]},
        "vm-b": {"depends_on": ["hypervisor-1"]},
        "app-1": {"depends_on": ["vm-a", "vm-b"]},
    }
    config = OrchestrationConfig()
    orchestrator = RebootOrchestrator(config)

    orchestrator.print_dependency_tree(
        {"hypervisor-1", "vm-a", "vm-b", "app-1"}, inventory
    )

    captured = capsys.readouterr()
    output = captured.out

    assert "└── hypervisor-1" in output
    assert "    ├── vm-a" in output
    assert "    │   └── app-1" in output
    assert "    └── vm-b" in output
    assert "        └── app-1 (already listed)" in output
