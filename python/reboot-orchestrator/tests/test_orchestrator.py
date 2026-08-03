"""
test_orchestrator.py - Unit Test Suite for RebootOrchestrator Engine
"""

from typing import Any
from unittest.mock import MagicMock, patch
import pytest

from reboot_orchestrator import (
    BootState,
    OrchestrationConfig,
    RebootOrchestrator,
    RebootVerification,
    VerificationStatus,
)


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


@patch("reboot_orchestrator.orchestrator.capture_boot_state")
@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.execute_zombie_workaround")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_orchestrator_run_flow(
    mock_load: MagicMock,
    mock_zombie: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
    mock_capture: MagicMock,
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

    # Every probe reports a fresh boot_id, so all hosts verify as rebooted.
    boot_ids = iter(["old-1", "new-1", "old-2", "new-2"])
    mock_capture.side_effect = lambda **kwargs: BootState(
        boot_id=next(boot_ids), uptime_seconds=None, captured_at=0.0
    )

    config = OrchestrationConfig(inventory_path="inventory.yml")
    orchestrator = RebootOrchestrator(config)

    results = orchestrator.run({"hypervisor-1", "vm-a"})

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

    # Check 5: Each host was probed before and after its reboot and verified
    assert mock_capture.call_count == 4
    assert len(results) == 2
    assert all(r.status is VerificationStatus.CONFIRMED for r in results)


@patch("reboot_orchestrator.orchestrator.capture_boot_state")
@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_orchestrator_run_warns_when_host_did_not_reboot(
    mock_load: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
    mock_capture: MagicMock,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """
    A host that answers ping without having rebooted must produce a warning and
    a NOT_REBOOTED verdict rather than a silent success.
    """
    inventory: dict[str, dict[str, Any]] = {"host-a": {"ip_addr": "10.0.0.10"}}
    mock_load.return_value = inventory

    # Identical boot_id before and after: the host never went down.
    mock_capture.side_effect = lambda **kwargs: BootState(
        boot_id="same-boot-id", uptime_seconds=None, captured_at=0.0
    )

    orchestrator = RebootOrchestrator(OrchestrationConfig())
    results = orchestrator.run({"host-a"})

    assert [r.status for r in results] == [VerificationStatus.NOT_REBOOTED]

    output = capsys.readouterr().out
    assert "did NOT reboot" in output
    assert "Not rebooted: 1" in output
    assert "finished successfully" not in output


@patch("reboot_orchestrator.orchestrator.capture_boot_state")
@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_orchestrator_run_warns_when_baseline_unavailable(
    mock_load: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
    mock_capture: MagicMock,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """
    A host that cannot be probed over SSH must be flagged as unverified, since
    the reboot command was almost certainly not delivered either.
    """
    inventory: dict[str, dict[str, Any]] = {"host-a": {"ip_addr": "10.0.0.10"}}
    mock_load.return_value = inventory
    mock_capture.return_value = None

    orchestrator = RebootOrchestrator(OrchestrationConfig())
    results = orchestrator.run({"host-a"})

    assert [r.status for r in results] == [VerificationStatus.UNKNOWN]

    output = capsys.readouterr().out
    assert "Cannot read the boot state of 'host-a'" in output
    assert "Unverified: 1" in output


@patch("reboot_orchestrator.orchestrator.capture_boot_state")
@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_orchestrator_run_skips_verification_when_disabled(
    mock_load: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
    mock_capture: MagicMock,
) -> None:
    """
    Verifies the opt-out leaves the original ping-only behavior intact.
    """
    inventory: dict[str, dict[str, Any]] = {"host-a": {"ip_addr": "10.0.0.10"}}
    mock_load.return_value = inventory

    orchestrator = RebootOrchestrator(OrchestrationConfig(verify_boot_state=False))
    results = orchestrator.run({"host-a"})

    mock_capture.assert_not_called()
    assert results == []


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


# The recheck scenarios below all use this shape: a hypervisor in tier 1 and two
# guests nested under it in tier 2. Rebooting the hypervisor power-cycles both
# guests, which is exactly when a stale pre-flight verdict would cause a
# needless second reboot.
NESTED_INVENTORY: dict[str, dict[str, Any]] = {
    "hypervisor-1": {"ip_addr": "10.0.0.5"},
    "vm-a": {"ip_addr": "10.0.0.21", "depends_on": ["hypervisor-1"]},
    "vm-b": {"ip_addr": "10.0.0.22", "depends_on": ["hypervisor-1"]},
}


@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_run_recheck_skips_hosts_the_parent_already_rebooted(
    mock_load: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
) -> None:
    """
    A guest that comes back clean after its hypervisor's reboot must be dropped
    from its tier, not rebooted a second time. Tier 1 is never re-checked — the
    caller's probe is still current there.
    """
    mock_load.return_value = NESTED_INVENTORY
    seen: list[list[str]] = []

    def recheck(hosts: list[str]) -> list[str]:
        seen.append(list(hosts))
        return [h for h in hosts if h == "vm-b"]

    orchestrator = RebootOrchestrator(OrchestrationConfig(verify_boot_state=False))
    orchestrator.run({"hypervisor-1", "vm-a", "vm-b"}, recheck=recheck)

    # Only tier 2 was re-checked, and only as a single call for that tier.
    assert seen == [["vm-a", "vm-b"]]

    # vm-a dropped out; the hypervisor and vm-b still rebooted.
    assert mock_reboot.call_count == 2
    mock_reboot.assert_any_call(hosts=["hypervisor-1"], inventory=NESTED_INVENTORY)
    mock_reboot.assert_any_call(hosts=["vm-b"], inventory=NESTED_INVENTORY)
    assert mock_wait.call_count == 2


@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_run_recheck_emptied_tier_issues_no_reboot_or_wait(
    mock_load: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
) -> None:
    """
    When every host in a tier comes back clean, the tier is skipped outright —
    no reboot, and no ping wait, because nothing went down.
    """
    mock_load.return_value = NESTED_INVENTORY

    orchestrator = RebootOrchestrator(OrchestrationConfig(verify_boot_state=False))
    orchestrator.run({"hypervisor-1", "vm-a", "vm-b"}, recheck=lambda hosts: [])

    mock_reboot.assert_called_once_with(
        hosts=["hypervisor-1"], inventory=NESTED_INVENTORY
    )
    mock_wait.assert_called_once()


@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_run_without_recheck_is_unchanged(
    mock_load: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
) -> None:
    """Omitting recheck leaves the original every-tier-reboots behaviour intact."""
    mock_load.return_value = NESTED_INVENTORY

    orchestrator = RebootOrchestrator(OrchestrationConfig(verify_boot_state=False))
    orchestrator.run({"hypervisor-1", "vm-a", "vm-b"})

    assert mock_reboot.call_count == 2
    mock_reboot.assert_any_call(hosts=["hypervisor-1"], inventory=NESTED_INVENTORY)
    mock_reboot.assert_any_call(hosts=["vm-a", "vm-b"], inventory=NESTED_INVENTORY)
    assert mock_wait.call_count == 2


@patch("reboot_orchestrator.orchestrator.verify_reboot")
@patch("reboot_orchestrator.orchestrator.capture_boot_state")
@patch("reboot_orchestrator.orchestrator.wait_for_hosts")
@patch("reboot_orchestrator.orchestrator.reboot_hosts")
@patch("reboot_orchestrator.orchestrator.load_inventory")
def test_recheck_skipped_hosts_are_never_verified(
    mock_load: MagicMock,
    mock_reboot: MagicMock,
    mock_wait: MagicMock,
    mock_capture: MagicMock,
    mock_verify: MagicMock,
) -> None:
    """
    A host the re-check drops was deliberately not rebooted, so it must be left
    out of boot state verification entirely. Verifying it anyway would probe a
    boot ID that was never going to change and report the host as having failed
    to reboot — a false alarm about a decision the orchestrator itself made.
    """
    mock_load.return_value = NESTED_INVENTORY
    mock_capture.return_value = BootState(
        boot_id="stub", uptime_seconds=1.0, captured_at=0.0
    )
    mock_verify.return_value = RebootVerification(
        host="vm-b", status=VerificationStatus.CONFIRMED, detail="boot ID changed"
    )

    orchestrator = RebootOrchestrator(OrchestrationConfig())
    verifications = orchestrator.run(
        {"hypervisor-1", "vm-a", "vm-b"},
        recheck=lambda hosts: [h for h in hosts if h == "vm-b"],
    )

    # vm-a was dropped, so it is neither baselined nor re-read afterwards.
    probed = {call.kwargs["host"] for call in mock_capture.call_args_list}
    assert "vm-a" not in probed
    assert {"hypervisor-1", "vm-b"} <= probed

    # One verdict per host actually rebooted: the hypervisor and vm-b.
    assert len(verifications) == 2
    assert "vm-a" not in {call.args[0] for call in mock_verify.call_args_list}
