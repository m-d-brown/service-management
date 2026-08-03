"""
test_boot_state.py - Unit Test Suite for Boot State Capture and Verification
"""

from typing import Any
from unittest.mock import MagicMock, patch

from reboot_orchestrator.boot_state import (
    BootState,
    VerificationStatus,
    capture_boot_state,
    format_uptime,
    parse_boot_probe,
    verify_reboot,
)


def test_parse_boot_probe_full_output() -> None:
    """
    Checks that both boot markers are extracted from the probe output.
    """
    state = parse_boot_probe(
        "boot_id=4f1e2d3c-0000-4000-8000-abcdefabcdef\nuptime=1234.56\n",
        captured_at=100.0,
    )

    assert state.boot_id == "4f1e2d3c-0000-4000-8000-abcdefabcdef"
    assert state.uptime_seconds == 1234.56
    assert state.captured_at == 100.0
    assert not state.is_empty


def test_parse_boot_probe_uptime_only() -> None:
    """
    Verifies hosts without a boot_id (busybox, appliances) still yield uptime.
    """
    state = parse_boot_probe("boot_id=\nuptime=98.7\n", captured_at=0.0)

    assert state.boot_id is None
    assert state.uptime_seconds == 98.7
    assert not state.is_empty


def test_parse_boot_probe_empty_output() -> None:
    """
    Verifies a host exposing neither marker is reported as an empty state.
    """
    state = parse_boot_probe("boot_id=\nuptime=\n", captured_at=0.0)

    assert state.is_empty


def test_parse_boot_probe_ignores_unparseable_uptime() -> None:
    """
    Ensures garbage uptime values are discarded rather than raising.
    """
    state = parse_boot_probe("uptime=not-a-number\n", captured_at=0.0)

    assert state.uptime_seconds is None


def test_verify_reboot_confirmed_by_boot_id() -> None:
    """
    A changed boot_id is definitive proof the host restarted.
    """
    before = BootState(boot_id="aaaa", uptime_seconds=5000.0, captured_at=0.0)
    after = BootState(boot_id="bbbb", uptime_seconds=30.0, captured_at=120.0)

    result = verify_reboot("host-a", before, after)

    assert result.status is VerificationStatus.CONFIRMED
    assert "boot_id changed" in result.detail


def test_verify_reboot_detects_unchanged_boot_id() -> None:
    """
    The core regression this module exists for: a host that answers ping but
    never rebooted must be reported as NOT_REBOOTED.
    """
    before = BootState(boot_id="aaaa", uptime_seconds=5000.0, captured_at=0.0)
    after = BootState(boot_id="aaaa", uptime_seconds=5120.0, captured_at=120.0)

    result = verify_reboot("host-a", before, after)

    assert result.status is VerificationStatus.NOT_REBOOTED
    assert "unchanged" in result.detail


def test_verify_reboot_confirmed_by_uptime_reset() -> None:
    """
    Without a boot_id, an uptime that dropped below the elapsed window confirms
    the reboot.
    """
    before = BootState(boot_id=None, uptime_seconds=86400.0, captured_at=0.0)
    after = BootState(boot_id=None, uptime_seconds=45.0, captured_at=120.0)

    result = verify_reboot("switch-1", before, after)

    assert result.status is VerificationStatus.CONFIRMED
    assert "uptime reset" in result.detail


def test_verify_reboot_detects_climbing_uptime() -> None:
    """
    Without a boot_id, an uptime that kept accumulating proves the host stayed up.
    """
    before = BootState(boot_id=None, uptime_seconds=86400.0, captured_at=0.0)
    after = BootState(boot_id=None, uptime_seconds=86520.0, captured_at=120.0)

    result = verify_reboot("switch-1", before, after)

    assert result.status is VerificationStatus.NOT_REBOOTED
    assert "kept climbing" in result.detail


def test_verify_reboot_uptime_within_elapsed_window() -> None:
    """
    A host rebooted twice (or freshly booted before the run) can report a higher
    uptime than the baseline while still being below the elapsed window.
    """
    before = BootState(boot_id=None, uptime_seconds=10.0, captured_at=0.0)
    after = BootState(boot_id=None, uptime_seconds=40.0, captured_at=300.0)

    result = verify_reboot("host-a", before, after)

    assert result.status is VerificationStatus.CONFIRMED


def test_verify_reboot_unknown_states() -> None:
    """
    Missing readings must surface as UNKNOWN rather than silently passing.
    """
    state = BootState(boot_id="aaaa", uptime_seconds=1.0, captured_at=0.0)
    empty = BootState(boot_id=None, uptime_seconds=None, captured_at=10.0)

    assert verify_reboot("h", None, state).status is VerificationStatus.UNKNOWN
    assert verify_reboot("h", state, None).status is VerificationStatus.UNKNOWN
    assert verify_reboot("h", None, None).status is VerificationStatus.UNKNOWN
    assert verify_reboot("h", empty, empty).status is VerificationStatus.UNKNOWN


@patch("subprocess.run")
def test_capture_boot_state_success(mock_run: MagicMock) -> None:
    """
    Verifies the SSH probe is constructed correctly and its output parsed.
    """
    inventory: dict[str, dict[str, Any]] = {
        "host-a": {"ip_addr": "10.0.0.10", "ansible_user": "admin"}
    }
    mock_run.return_value.returncode = 0
    mock_run.return_value.stdout = "boot_id=abc\nuptime=42.0\n"
    mock_run.return_value.stderr = ""

    state = capture_boot_state("host-a", inventory, probe_timeout_seconds=7)

    assert state is not None
    assert state.boot_id == "abc"
    assert state.uptime_seconds == 42.0

    called_cmd = mock_run.call_args[0][0]
    assert "ssh" in called_cmd
    assert "admin@10.0.0.10" in called_cmd
    assert "/proc/sys/kernel/random/boot_id" in called_cmd[-1]
    assert "/proc/uptime" in called_cmd[-1]
    assert mock_run.call_args[1]["timeout"] == 7


@patch("subprocess.run")
def test_capture_boot_state_ssh_failure(mock_run: MagicMock) -> None:
    """
    An unreachable or unauthenticated host must return None, not a bogus state.
    """
    inventory: dict[str, dict[str, Any]] = {"host-a": {"ip_addr": "10.0.0.10"}}
    mock_run.return_value.returncode = 255
    mock_run.return_value.stdout = ""
    mock_run.return_value.stderr = "Permission denied (publickey)."

    assert capture_boot_state("host-a", inventory) is None


def test_format_uptime() -> None:
    """
    Checks compact duration rendering across magnitudes.
    """
    assert format_uptime(45) == "45s"
    assert format_uptime(3600) == "1h"
    assert format_uptime(90061) == "1d 1h 1m"
