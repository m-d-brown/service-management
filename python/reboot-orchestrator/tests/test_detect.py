"""
test_detect.py - Unit Test Suite for Reboot Orchestrator Detection Module
"""

import os
import subprocess
import tempfile
from typing import Any
from unittest.mock import MagicMock, patch

from reboot_orchestrator.detect import (
    REBOOT_REQUIRED_FLAG,
    RebootStatus,
    build_probe_script,
    make_recheck,
    probe_host,
    probe_hosts,
)

INVENTORY: dict[str, dict[str, Any]] = {
    "host-a": {"ip_addr": "10.0.0.10", "ansible_user": "admin"},
    "host-b": {"ip_addr": "10.0.0.11"},
}


def completed(
    stdout: str = "", stderr: str = "", returncode: int = 0
) -> subprocess.CompletedProcess[str]:
    """Builds a CompletedProcess standing in for one SSH probe."""
    return subprocess.CompletedProcess(
        args=["ssh"], returncode=returncode, stdout=stdout, stderr=stderr
    )


@patch("subprocess.run")
def test_probe_host_builds_ssh_command(mock_run: MagicMock) -> None:
    """
    The probe must run `/bin/sh` on the remote with the script on stdin, not as
    an argument: the remote login shell may be csh, which cannot parse it.
    """
    mock_run.return_value = completed("OK no pending reboot flag\n")

    probe_host("host-a", INVENTORY)

    command = mock_run.call_args[0][0]
    assert command[0] == "ssh"
    assert command[-2:] == ["admin@10.0.0.10", "/bin/sh"]
    assert "BatchMode=yes" in command
    assert mock_run.call_args[1]["input"] == build_probe_script()


@patch("subprocess.run")
def test_probe_host_parses_verdicts(mock_run: MagicMock) -> None:
    """Both verdict prefixes are recognised and the reason is carried through."""
    mock_run.return_value = completed("NEEDED packages awaiting restart: systemd\n")
    assert probe_host("host-a", INVENTORY) == RebootStatus(
        "host-a", True, "packages awaiting restart: systemd"
    )

    mock_run.return_value = completed("OK kernel 14.3-RELEASE is current\n")
    assert probe_host("host-a", INVENTORY) == RebootStatus(
        "host-a", False, "kernel 14.3-RELEASE is current"
    )


@patch("subprocess.run")
def test_probe_host_failures_are_unprobed_not_healthy(mock_run: MagicMock) -> None:
    """
    A host that cannot be reached or understood must come back as None, never
    False: treating an unreachable host as up to date would silently drop it
    from the reboot set.
    """
    mock_run.return_value = completed(stderr="ssh: connect: timed out", returncode=255)
    status = probe_host("host-b", INVENTORY)
    assert status.needs_reboot is None
    assert "timed out" in status.reason

    mock_run.return_value = completed("something else entirely\n")
    assert probe_host("host-b", INVENTORY).needs_reboot is None

    mock_run.side_effect = OSError("ssh not found")
    assert probe_host("host-b", INVENTORY).needs_reboot is None


@patch("subprocess.run")
def test_probe_hosts_preserves_order(mock_run: MagicMock) -> None:
    """Results come back in the order requested, not the order they finish."""
    mock_run.return_value = completed("OK no pending reboot flag\n")

    statuses = probe_hosts(["host-b", "host-a"], INVENTORY)

    assert [s.host for s in statuses] == ["host-b", "host-a"]
    assert probe_hosts([], INVENTORY) == []


def run_probe_script(flag: str, pkgs: str) -> str:
    """
    Runs the real probe script through /bin/sh against the given paths.

    This exercises the script itself rather than a transcript of it. Only the
    non-FreeBSD branch is covered — the development machine is not FreeBSD, so
    the kernel-comparison branch is verified by parse tests above and against
    live hosts, not here.
    """
    result = subprocess.run(
        ["/bin/sh"],
        input=build_probe_script(flag=flag, pkgs=pkgs),
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout.strip()


def test_probe_script_flag_states() -> None:
    """The script reports each pending-reboot state a Debian host can be in."""
    with tempfile.TemporaryDirectory() as tmp:
        flag = os.path.join(tmp, "reboot-required")
        pkgs = f"{flag}.pkgs"

        # No flag at all.
        assert run_probe_script(flag, pkgs) == "OK no pending reboot flag"

        # Flag present, but apt recorded no package list.
        open(flag, "w").close()
        assert run_probe_script(flag, pkgs) == f"NEEDED {flag} is present"

        # Flag plus the packages that asked for the restart.
        with open(pkgs, "w") as fh:
            fh.write("systemd\nlinux-image-6.12.43-amd64\n")
        assert run_probe_script(flag, pkgs) == (
            "NEEDED packages awaiting restart: systemd linux-image-6.12.43-amd64"
        )


def test_default_probe_script_targets_the_apt_flag() -> None:
    """The shipped script checks the real path, not a test fixture's."""
    assert REBOOT_REQUIRED_FLAG in build_probe_script()


@patch("reboot_orchestrator.detect.probe_hosts")
def test_make_recheck_narrows_and_reports_unprobed(mock_probe: MagicMock) -> None:
    """
    The re-check keeps only hosts still pending, and hands back anything it
    could not probe so the caller can flag it rather than silently reboot it.
    """
    mock_probe.return_value = [
        RebootStatus("host-a", True, "packages awaiting restart: systemd"),
        RebootStatus("host-b", False, "no pending reboot flag"),
        RebootStatus("host-c", None, "probe failed: timed out"),
    ]
    seen: list[RebootStatus] = []

    recheck = make_recheck(INVENTORY, on_unprobed=seen.append)
    remaining = recheck(["host-a", "host-b", "host-c"])

    assert remaining == ["host-a"]
    assert [s.host for s in seen] == ["host-c"]


@patch("reboot_orchestrator.detect.probe_hosts")
def test_make_recheck_without_callback(mock_probe: MagicMock) -> None:
    """on_unprobed is optional."""
    mock_probe.return_value = [RebootStatus("host-a", None, "probe failed")]

    recheck = make_recheck(INVENTORY)

    assert recheck(["host-a"]) == []
