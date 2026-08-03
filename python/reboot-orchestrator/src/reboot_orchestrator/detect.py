"""
detect.py - Pending Reboot Detection

Probes hosts over SSH to find out whether they are waiting on a reboot, so the
orchestrator can reboot only what needs it without an external configuration
management runner deciding for it.

The probe is a single POSIX shell script, piped to `/bin/sh` on the remote's
standard input rather than passed as an argument. That keeps it independent of
the login shell (FreeBSD accounts commonly default to csh, where the script's
syntax would not parse) and removes shell quoting from the picture entirely.
Nothing it runs requires privilege.
"""

from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any, Callable, Optional
import subprocess

from reboot_orchestrator.ssh import get_ssh_target_and_args

# Debian and its derivatives: apt drops this flag when an installed package
# needs a restart to take effect, and records the packages responsible next to
# it. The flag is authoritative; the package list is best-effort detail that
# some upgrades leave absent.
REBOOT_REQUIRED_FLAG = "/var/run/reboot-required"
REBOOT_REQUIRED_PKGS = f"{REBOOT_REQUIRED_FLAG}.pkgs"

# FreeBSD keeps no such flag, so compare the running kernel against the
# installed one: `uname -r` reports what booted, `freebsd-version -k` what is on
# disk. They diverge exactly when an update is waiting on a reboot.
PROBE_SCRIPT_TEMPLATE = """\
if [ "$(uname -s)" = FreeBSD ]; then
    running=$(uname -r)
    installed=$(freebsd-version -k)
    if [ "$running" != "$installed" ]; then
        echo "NEEDED kernel $running running, $installed installed"
    else
        echo "OK kernel $running is current"
    fi
elif [ -e {flag} ]; then
    pkgs=$(tr '\\n' ' ' < {pkgs} 2>/dev/null)
    if [ -n "$pkgs" ]; then
        echo "NEEDED packages awaiting restart: $pkgs"
    else
        echo "NEEDED {flag} is present"
    fi
else
    echo "OK no pending reboot flag"
fi
"""


@dataclass(frozen=True)
class RebootStatus:
    """
    The outcome of probing one host.

    Attributes:
        host: Hostname as it appears in the inventory.
        needs_reboot: True if a reboot is pending, False if the host is current,
                      None if the probe could not reach or parse the host — a
                      state deliberately distinct from False, because an
                      unprobed host must not be quietly treated as up to date.
        reason: Human-readable explanation of the verdict.
    """

    host: str
    needs_reboot: Optional[bool]
    reason: str


def build_probe_script(
    flag: str = REBOOT_REQUIRED_FLAG, pkgs: str = REBOOT_REQUIRED_PKGS
) -> str:
    """
    Renders the probe script.

    Args:
        flag: Path to the pending-reboot flag file to test for.
        pkgs: Path to the file listing the packages that requested the restart.

    Returns:
        str: A POSIX shell script printing one line, prefixed NEEDED or OK.
    """
    return PROBE_SCRIPT_TEMPLATE.format(flag=flag, pkgs=pkgs)


def probe_host(
    host: str,
    inventory: dict[str, dict[str, Any]],
    script: Optional[str] = None,
) -> RebootStatus:
    """
    Probes a single host over SSH for a pending reboot.

    Args:
        host: Hostname of the target.
        inventory: Flattened inventory map of hosts to properties.
        script: Probe script to run; defaults to build_probe_script().

    Returns:
        RebootStatus: The verdict for this host. Any failure to connect, run, or
                      parse yields needs_reboot=None rather than raising, so one
                      unreachable host cannot abort a fleet-wide check.
    """
    target, ssh_args = get_ssh_target_and_args(host, inventory)
    command = ["ssh"] + ssh_args + [target, "/bin/sh"]

    try:
        result = subprocess.run(
            command,
            input=script if script is not None else build_probe_script(),
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError as e:
        return RebootStatus(host, None, f"probe failed: {e}")

    if result.returncode != 0:
        stderr = result.stderr.strip().splitlines()
        detail = stderr[-1] if stderr else f"ssh exited {result.returncode}"
        return RebootStatus(host, None, f"probe failed: {detail}")

    verdict, _, reason = result.stdout.strip().partition(" ")
    if verdict == "NEEDED":
        return RebootStatus(host, True, reason.strip())
    if verdict == "OK":
        return RebootStatus(host, False, reason.strip())
    return RebootStatus(
        host, None, f"unexpected probe output: {result.stdout.strip()!r}"
    )


def probe_hosts(
    hosts: list[str],
    inventory: dict[str, dict[str, Any]],
    max_workers: int = 8,
    script: Optional[str] = None,
) -> list[RebootStatus]:
    """
    Probes several hosts concurrently.

    Args:
        hosts: Hostnames to probe.
        inventory: Flattened inventory map of hosts to properties.
        max_workers: Maximum concurrent SSH probes.
        script: Probe script to run; defaults to build_probe_script().

    Returns:
        list[RebootStatus]: One entry per host, in the order given.
    """
    if not hosts:
        return []

    def probe(host: str) -> RebootStatus:
        return probe_host(host, inventory, script=script)

    with ThreadPoolExecutor(max_workers=min(max_workers, len(hosts))) as pool:
        return list(pool.map(probe, hosts))


def print_report(statuses: list[RebootStatus]) -> None:
    """
    Prints one line per probed host, explaining each verdict.

    Args:
        statuses: Probe results to report.
    """
    pending = [s for s in statuses if s.needs_reboot is True]
    print(f"\n{len(statuses)} hosts checked, {len(pending)} need a reboot:")
    for status in statuses:
        print(f"  {status.host} — {status.reason}")


def make_recheck(
    inventory: dict[str, dict[str, Any]],
    on_unprobed: Optional[Callable[[RebootStatus], None]] = None,
) -> Callable[[list[str]], list[str]]:
    """
    Builds the per-tier re-check the orchestrator calls before rebooting a tier.

    Rebooting a parent power-cycles everything nested under it, so a result
    gathered before the previous tier ran is stale by the time a child's tier
    comes up. Re-probing there keeps the orchestrator from rebooting a guest a
    second time for an update its parent's reboot already applied.

    Args:
        inventory: Flattened inventory map of hosts to properties.
        on_unprobed: Called for each host that could not be probed. Such hosts
                     are left out of the returned list — an unprobed host is not
                     known to need a reboot.

    Returns:
        Callable[[list[str]], list[str]]: Narrows a tier to the hosts still
                                          reporting a pending reboot.
    """

    def recheck(hosts: list[str]) -> list[str]:
        statuses = probe_hosts(hosts, inventory)
        for status in statuses:
            if status.needs_reboot is None and on_unprobed is not None:
                on_unprobed(status)
        return [s.host for s in statuses if s.needs_reboot is True]

    return recheck
