"""
boot_state.py - Boot Identity Capture and Reboot Verification

ICMP reachability alone cannot prove that a host rebooted: a host that ignored
the reboot command (failed SSH authentication, denied sudo, hung shutdown unit)
keeps answering pings and looks indistinguishable from one that came back.

This module records a boot identity over SSH before the reboot is issued, reads
it again once the host is reachable, and compares the two to decide whether the
expected state change actually occurred.
"""

from dataclasses import dataclass
from enum import Enum
from typing import Any
import subprocess
import time

from reboot_orchestrator.ssh import format_command, get_ssh_target_and_args

# Reads the two independent boot markers exposed by the kernel:
#   - /proc/sys/kernel/random/boot_id: regenerated on every boot (most reliable).
#   - /proc/uptime: seconds since boot, for hosts without a boot_id (busybox,
#     appliance firmware, network gear).
# Both are emitted as key=value lines and are empty when unreadable, so a host
# missing one marker still reports the other. The command deliberately contains
# no single quotes so that echoing it back to the operator stays readable.
BOOT_PROBE_COMMAND = (
    'printf "boot_id=%s\\n" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"; '
    'printf "uptime=%s\\n" "$(cut -d" " -f1 /proc/uptime 2>/dev/null)"'
)

# Slack allowed when comparing a post-reboot uptime against elapsed wall time,
# absorbing clock skew and the gap between issuing the probe and the host
# answering it.
UPTIME_TOLERANCE_SECONDS = 5.0


class VerificationStatus(Enum):
    """
    Outcome of comparing a host's boot identity before and after a reboot.
    """

    CONFIRMED = "confirmed"
    NOT_REBOOTED = "not-rebooted"
    UNKNOWN = "unknown"


@dataclass(frozen=True)
class BootState:
    """
    A point-in-time reading of a host's boot identity.

    Attributes:
        boot_id: Kernel boot UUID, or None when the host does not expose one.
        uptime_seconds: Seconds since boot, or None when unreadable.
        captured_at: Monotonic timestamp of the reading, used to measure the
                     wall-clock window a reboot had to occur in.
    """

    boot_id: str | None
    uptime_seconds: float | None
    captured_at: float

    @property
    def is_empty(self) -> bool:
        """
        Returns True when the host answered SSH but exposed no usable boot marker.
        """
        return self.boot_id is None and self.uptime_seconds is None


@dataclass(frozen=True)
class RebootVerification:
    """
    The verdict for a single host, suitable for printing and for deciding the
    process exit status.

    Attributes:
        host: Hostname the verdict applies to.
        status: Whether the reboot was confirmed, disproved, or indeterminate.
        detail: Human-readable evidence behind the verdict.
    """

    host: str
    status: VerificationStatus
    detail: str


def format_uptime(seconds: float) -> str:
    """
    Renders a duration in seconds as a compact human-readable uptime string.

    Args:
        seconds: Duration in seconds.

    Returns:
        str: Formatted duration such as "3d 4h 12m" or "45s".
    """
    total = int(seconds)
    days, rem = divmod(total, 86400)
    hours, rem = divmod(rem, 3600)
    minutes, secs = divmod(rem, 60)

    parts = []
    if days:
        parts.append(f"{days}d")
    if hours:
        parts.append(f"{hours}h")
    if minutes:
        parts.append(f"{minutes}m")
    if not parts:
        parts.append(f"{secs}s")

    return " ".join(parts)


def parse_boot_probe(output: str, captured_at: float) -> BootState:
    """
    Parses the key=value output of BOOT_PROBE_COMMAND into a BootState.

    Args:
        output: Raw stdout captured from the probe command.
        captured_at: Monotonic timestamp to record against the reading.

    Returns:
        BootState: Parsed markers; fields are None when absent or unparseable.
    """
    boot_id: str | None = None
    uptime: float | None = None

    for line in output.splitlines():
        key, _, value = line.partition("=")
        value = value.strip()
        if not value:
            continue
        if key.strip() == "boot_id":
            boot_id = value
        elif key.strip() == "uptime":
            try:
                uptime = float(value)
            except ValueError:
                continue

    return BootState(boot_id=boot_id, uptime_seconds=uptime, captured_at=captured_at)


def capture_boot_state(
    host: str,
    inventory: dict[str, dict[str, Any]],
    probe_timeout_seconds: int = 15,
    announce: bool = True,
) -> BootState | None:
    """
    Reads a host's boot identity over SSH.

    This doubles as an SSH pre-flight check: a host that cannot be probed almost
    certainly cannot be rebooted over SSH either.

    Args:
        host: Hostname of the target.
        inventory: Flattened inventory map of hosts to properties.
        probe_timeout_seconds: Seconds to wait for the SSH probe to complete.
        announce: Whether to print the command being run.

    Returns:
        BootState | None: The reading, or None when the host could not be probed.
    """
    target, ssh_args = get_ssh_target_and_args(host, inventory)
    cmd = ["ssh"] + ssh_args + [target, BOOT_PROBE_COMMAND]

    if announce:
        print(f"  Reading boot state of {host}:")
        print(f"    $ {format_command(cmd)}")

    try:
        res = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            check=False,
            timeout=probe_timeout_seconds,
        )
    except subprocess.TimeoutExpired:
        print(f"  WARNING: Boot state probe for '{host}' timed out.")
        return None
    except Exception as e:
        print(f"  WARNING: Boot state probe for '{host}' failed: {e}")
        return None

    captured_at = time.monotonic()

    if res.returncode != 0:
        print(
            f"  WARNING: Boot state probe for '{host}' returned code {res.returncode}. "
            f"Error: {res.stderr.strip()}"
        )
        return None

    state = parse_boot_probe(res.stdout, captured_at)
    if state.is_empty:
        print(
            f"  WARNING: '{host}' exposed neither /proc/sys/kernel/random/boot_id "
            "nor /proc/uptime; its reboot cannot be verified."
        )
    return state


def verify_reboot(
    host: str,
    before: BootState | None,
    after: BootState | None,
) -> RebootVerification:
    """
    Compares boot identities recorded before and after a reboot.

    A changed boot_id is definitive. Failing that, uptime is used: a host that
    rebooted must report an uptime lower than its previous one and no larger
    than the wall-clock window between the two readings.

    Args:
        host: Hostname the readings belong to.
        before: Reading taken before the reboot was issued, or None.
        after: Reading taken once the host was reachable again, or None.

    Returns:
        RebootVerification: The verdict and the evidence behind it.
    """
    if before is None and after is None:
        return RebootVerification(
            host,
            VerificationStatus.UNKNOWN,
            "boot state could not be read before or after the reboot",
        )
    if before is None:
        return RebootVerification(
            host,
            VerificationStatus.UNKNOWN,
            "no baseline boot state was recorded before the reboot",
        )
    if after is None:
        return RebootVerification(
            host,
            VerificationStatus.UNKNOWN,
            "host answered ping but its boot state could not be read afterwards",
        )

    if before.boot_id and after.boot_id:
        if before.boot_id != after.boot_id:
            return RebootVerification(
                host,
                VerificationStatus.CONFIRMED,
                f"boot_id changed ({before.boot_id} -> {after.boot_id})",
            )
        return RebootVerification(
            host,
            VerificationStatus.NOT_REBOOTED,
            f"boot_id is unchanged ({before.boot_id}); the host never went down",
        )

    if before.uptime_seconds is not None and after.uptime_seconds is not None:
        elapsed = after.captured_at - before.captured_at
        before_str = format_uptime(before.uptime_seconds)
        after_str = format_uptime(after.uptime_seconds)
        if (
            after.uptime_seconds < before.uptime_seconds
            or after.uptime_seconds <= elapsed + UPTIME_TOLERANCE_SECONDS
        ):
            return RebootVerification(
                host,
                VerificationStatus.CONFIRMED,
                f"uptime reset ({before_str} -> {after_str})",
            )
        return RebootVerification(
            host,
            VerificationStatus.NOT_REBOOTED,
            f"uptime kept climbing ({before_str} -> {after_str}) over a "
            f"{format_uptime(elapsed)} window; the host never went down",
        )

    return RebootVerification(
        host,
        VerificationStatus.UNKNOWN,
        "host exposes no boot_id or uptime marker to compare",
    )
