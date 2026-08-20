package reboot

import (
	"context"
	"io"
	"strings"
	"time"
)

// baseSSHArgs are applied to every connection. BatchMode keeps a host that
// wants a password from hanging the run on a prompt no one is there to answer,
// and accept-new trusts an unseen host without also accepting a changed key.
var baseSSHArgs = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=5",
	"-o", "StrictHostKeyChecking=accept-new",
}

// sshCommand builds the argument vector for running remoteCmd on a host.
func sshCommand(h Host, remoteCmd string) []string {
	args := append([]string{}, baseSSHArgs...)
	args = append(args, h.SSHArgs...)
	return append(args, sshDestination(h), remoteCmd)
}

// sshDestination renders the user@address destination for a host.
func sshDestination(h Host) string {
	if h.User != "" {
		return h.User + "@" + h.Target()
	}
	return h.Target()
}

// RebootHosts issues a reboot to each host without waiting for it.
//
// The commands are dispatched rather than awaited because rebooting severs the
// SSH connection as a matter of course: waiting would mean waiting for a
// failure that means nothing. Whether the reboot actually landed is settled
// afterwards by comparing boot state, not by any status returned here.
func RebootHosts(out io.Writer, runner Runner, hosts []Host) {
	if len(hosts) == 0 {
		return
	}
	report(out, "Issuing reboot command to: %s\n", joinNames(hosts))
	for _, host := range hosts {
		// Falling back to an unprivileged reboot covers hosts where the login
		// user is already root and sudo is not installed at all.
		args := sshCommand(host, "sudo reboot || reboot")
		report(out, "  $ %s\n", FormatCommand("ssh", args...))
		if err := runner.Start("ssh", args...); err != nil {
			report(out, "  WARNING: failed to issue reboot to %s: %v\n", host.Name, err)
		}
	}
}

// ForceHostOff powers down a host that cannot be trusted to power itself off.
//
// The host is asked to halt gracefully first and given time to do it, so
// filesystems are flushed and services stopped in the ordinary way; only then is
// its power cut from the delegate. Cutting first would make every such host an
// unclean shutdown, which is a worse problem than the one being solved.
func ForceHostOff(
	ctx context.Context,
	out io.Writer,
	runner Runner,
	clock Clock,
	host, delegate Host,
	haltWait time.Duration,
) {
	report(out, "Halting %q gracefully before forcing it off...\n", host.Name)
	haltArgs := sshCommand(host, "sudo poweroff || poweroff")
	report(out, "  $ %s\n", FormatCommand("ssh", haltArgs...))
	if err := runner.Start("ssh", haltArgs...); err != nil {
		report(out, "  WARNING: failed to issue poweroff to %s: %v\n", host.Name, err)
	}

	report(out, "Waiting %s for %q to power down...\n", haltWait, host.Name)
	clock.Sleep(haltWait)

	report(out, "Forcing %q off via %q...\n", host.Name, delegate.Name)
	// The command is the operator's own and runs verbatim: this package has no
	// way to know whether the delegate's CLI wants sudo, a wrapper, or neither,
	// and second-guessing it would break commands that are already correct.
	stopArgs := sshCommand(delegate, host.ForceOff.Command)
	report(out, "  $ %s\n", FormatCommand("ssh", stopArgs...))
	// A failure here is reported rather than fatal: the host may well have
	// powered off gracefully already, which is exactly the case where the
	// force-off command finds nothing to stop and says so.
	if _, err := runner.Run(ctx, "", "ssh", stopArgs...); err != nil {
		report(out, "  WARNING: force-off of %s via %s failed: %v\n", host.Name, delegate.Name, err)
	}
}

// joinNames renders host names as a comma-separated list.
func joinNames(hosts []Host) string {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	return strings.Join(names, ", ")
}
