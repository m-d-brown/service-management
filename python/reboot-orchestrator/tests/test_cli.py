"""
test_cli.py - Unit Test Suite for Reboot Orchestrator Command Line Interface
"""

import sys
from unittest.mock import MagicMock, patch
import pytest
from reboot_orchestrator.boot_state import RebootVerification, VerificationStatus
from reboot_orchestrator.cli import main
from reboot_orchestrator.detect import RebootStatus


@patch("sys.exit")
@patch("reboot_orchestrator.cli.RebootOrchestrator")
def test_cli_execution_flow(
    mock_orchestrator_cls: MagicMock, mock_exit: MagicMock
) -> None:
    """
    Verifies that the CLI correctly parses arguments and runs the orchestrator.
    """
    mock_orchestrator = MagicMock()
    mock_orchestrator.get_inventory.return_value = {
        "host-a": {"ip_addr": "10.0.0.1"},
        "host-b": {"ip_addr": "10.0.0.2"},
    }
    mock_orchestrator.run.return_value = []
    mock_orchestrator_cls.return_value = mock_orchestrator

    # Mock command line arguments
    test_args = [
        "reboot-orchestrator",
        "--inventory",
        "mock_inventory.yml",
        "--yes",
        "host-a",
        "host-b",
    ]

    with patch.object(sys, "argv", test_args):
        main()

    # Verify Orchestrator was initialized with parsed config
    mock_orchestrator_cls.assert_called_once()
    called_config = mock_orchestrator_cls.call_args[0][0]
    assert called_config.inventory_path == "mock_inventory.yml"

    # Verify validation and run targets were executed
    mock_orchestrator.validate_targets.assert_called_once_with(
        mock_orchestrator.get_inventory.return_value, {"host-a", "host-b"}
    )
    # Without --if-needed every named host is rebooted, and no per-tier
    # re-check is wired in.
    mock_orchestrator.run.assert_called_once_with(
        target_hosts={"host-a", "host-b"}, recheck=None
    )

    # Boot state verification is on unless explicitly skipped
    assert called_config.verify_boot_state is True

    # A clean run must not exit non-zero
    mock_exit.assert_not_called()


@patch("sys.exit")
@patch("reboot_orchestrator.cli.RebootOrchestrator")
def test_cli_exits_nonzero_when_host_did_not_reboot(
    mock_orchestrator_cls: MagicMock, mock_exit: MagicMock
) -> None:
    """
    Verifies a host proven not to have rebooted fails the run, even though every
    tier completed and the host answered ping.
    """
    mock_orchestrator = MagicMock()
    mock_orchestrator.get_inventory.return_value = {"host-a": {"ip_addr": "10.0.0.1"}}
    mock_orchestrator.run.return_value = [
        RebootVerification(
            "host-a", VerificationStatus.NOT_REBOOTED, "boot_id is unchanged"
        )
    ]
    mock_orchestrator_cls.return_value = mock_orchestrator

    test_args = ["reboot-orchestrator", "--yes", "host-a"]

    with patch.object(sys, "argv", test_args):
        main()

    mock_exit.assert_called_once_with(1)


@patch("sys.exit")
@patch("reboot_orchestrator.cli.RebootOrchestrator")
def test_cli_skip_boot_verification_flag(
    mock_orchestrator_cls: MagicMock, mock_exit: MagicMock
) -> None:
    """
    Verifies the verification opt-out is threaded through to the configuration.
    """
    mock_orchestrator = MagicMock()
    mock_orchestrator.get_inventory.return_value = {"host-a": {"ip_addr": "10.0.0.1"}}
    mock_orchestrator.run.return_value = []
    mock_orchestrator_cls.return_value = mock_orchestrator

    test_args = [
        "reboot-orchestrator",
        "--yes",
        "--skip-boot-verification",
        "--probe-timeout-seconds",
        "30",
        "host-a",
    ]

    with patch.object(sys, "argv", test_args):
        main()

    called_config = mock_orchestrator_cls.call_args[0][0]
    assert called_config.verify_boot_state is False
    assert called_config.probe_timeout_seconds == 30


def test_cli_missing_positional_hosts() -> None:
    """
    Verifies that running CLI without positional hosts causes argparse to exit.
    """
    test_args = ["reboot-orchestrator", "--inventory", "mock_inventory.yml"]

    with patch.object(sys, "argv", test_args):
        # argparse prints to stderr and exits when required arguments are missing
        with pytest.raises(SystemExit):
            main()


INVENTORY = {"host-a": {"ip_addr": "10.0.0.1"}, "host-b": {"ip_addr": "10.0.0.2"}}


def cli_with_if_needed(mock_orchestrator_cls: MagicMock) -> MagicMock:
    """
    Runs the CLI with --if-needed against a mocked orchestrator.

    Configures the class's existing return_value rather than replacing it, so a
    caller can set up `mock_orchestrator_cls.return_value.run` beforehand and
    have it survive.
    """
    mock_orchestrator = mock_orchestrator_cls.return_value
    mock_orchestrator.get_inventory.return_value = INVENTORY

    test_args = [
        "reboot-orchestrator",
        "--inventory",
        "mock_inventory.yml",
        "--yes",
        "--if-needed",
        "host-a",
        "host-b",
    ]
    with patch.object(sys, "argv", test_args):
        main()
    return mock_orchestrator


@patch("reboot_orchestrator.cli.probe_hosts")
@patch("reboot_orchestrator.cli.RebootOrchestrator")
def test_cli_if_needed_narrows_targets(
    mock_orchestrator_cls: MagicMock, mock_probe: MagicMock
) -> None:
    """
    --if-needed reboots only the hosts the probe flagged, and installs the
    per-tier re-check so nested guests are re-examined as tiers advance.
    """
    mock_probe.return_value = [
        RebootStatus("host-a", True, "packages awaiting restart: systemd"),
        RebootStatus("host-b", False, "no pending reboot flag"),
    ]

    mock_orchestrator = cli_with_if_needed(mock_orchestrator_cls)

    run_kwargs = mock_orchestrator.run.call_args[1]
    assert run_kwargs["target_hosts"] == {"host-a"}
    assert callable(run_kwargs["recheck"])


@patch("reboot_orchestrator.cli.probe_hosts")
@patch("reboot_orchestrator.cli.RebootOrchestrator")
def test_cli_if_needed_nothing_to_do(
    mock_orchestrator_cls: MagicMock, mock_probe: MagicMock
) -> None:
    """
    With nothing pending and every host probed cleanly, the CLI exits 0 without
    reaching the orchestrator at all.
    """
    mock_probe.return_value = [
        RebootStatus("host-a", False, "no pending reboot flag"),
        RebootStatus("host-b", False, "no pending reboot flag"),
    ]

    with pytest.raises(SystemExit) as excinfo:
        cli_with_if_needed(mock_orchestrator_cls)

    assert excinfo.value.code == 0
    mock_orchestrator_cls.return_value.run.assert_not_called()


@patch("reboot_orchestrator.cli.probe_hosts")
@patch("reboot_orchestrator.cli.RebootOrchestrator")
def test_cli_if_needed_unprobed_host_is_not_silent(
    mock_orchestrator_cls: MagicMock, mock_probe: MagicMock
) -> None:
    """
    A host that could not be probed must not pass for healthy: it stays out of
    the reboot set but forces a non-zero exit so a calling script notices.
    """
    mock_probe.return_value = [
        RebootStatus("host-a", False, "no pending reboot flag"),
        RebootStatus("host-b", None, "probe failed: timed out"),
    ]

    with pytest.raises(SystemExit) as excinfo:
        cli_with_if_needed(mock_orchestrator_cls)

    assert excinfo.value.code == 1


@patch("reboot_orchestrator.cli.probe_hosts")
@patch("reboot_orchestrator.cli.RebootOrchestrator")
def test_cli_if_needed_unprobed_host_after_reboots_still_exits_nonzero(
    mock_orchestrator_cls: MagicMock, mock_probe: MagicMock
) -> None:
    """
    A host lost during a per-tier re-check, after other hosts have already
    rebooted, must still surface: the run completes, then exits non-zero.
    """
    mock_probe.return_value = [
        RebootStatus("host-a", True, "packages awaiting restart: systemd"),
        RebootStatus("host-b", True, "packages awaiting restart: systemd"),
    ]
    # Stand in for the orchestrator reaching host-b's tier and re-checking it.
    mock_orchestrator_cls.return_value.run.side_effect = lambda target_hosts, recheck: (
        recheck(["host-b"])
    )

    # The re-check resolves probe_hosts in detect's namespace, so this patch
    # affects only the per-tier probe and leaves the initial one above intact.
    with patch("reboot_orchestrator.detect.probe_hosts") as mock_recheck_probe:
        mock_recheck_probe.return_value = [
            RebootStatus("host-b", None, "probe failed: timed out")
        ]
        with pytest.raises(SystemExit) as excinfo:
            cli_with_if_needed(mock_orchestrator_cls)

    assert excinfo.value.code == 1
    mock_orchestrator_cls.return_value.run.assert_called_once()
