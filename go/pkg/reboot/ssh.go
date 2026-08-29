package reboot

import (
	"io"
	"strings"
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

// joinNames renders host names as a comma-separated list.
func joinNames(hosts []Host) string {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	return strings.Join(names, ", ")
}
