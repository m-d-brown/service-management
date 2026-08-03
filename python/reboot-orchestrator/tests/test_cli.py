"""
test_cli.py - Unit Test Suite for Reboot Orchestrator Command Line Interface
"""

import sys
from unittest.mock import MagicMock, patch
import pytest
from reboot_orchestrator.boot_state import RebootVerification, VerificationStatus
from reboot_orchestrator.cli import main


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
    mock_orchestrator.run.assert_called_once_with(target_hosts={"host-a", "host-b"})

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
