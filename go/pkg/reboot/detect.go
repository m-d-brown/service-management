package reboot

import (
	"context"
	"fmt"
	"io"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Paths Debian and its derivatives use to record a pending reboot. apt drops
// the flag when an installed package needs a restart to take effect and lists
// the packages responsible alongside it. The flag is authoritative; the package
// list is best-effort detail that some upgrades leave absent.
const (
	rebootRequiredFlag = "/var/run/reboot-required"
	rebootRequiredPkgs = rebootRequiredFlag + ".pkgs"
)

// probeScript decides whether a host is waiting on a reboot.
//
// It is piped to /bin/sh on the remote's standard input rather than passed as
// an argument, which keeps it independent of the login shell — commonly csh on
// FreeBSD, where this syntax would not parse — and removes shell quoting from
// the picture entirely. Nothing it runs requires privilege.
//
// FreeBSD keeps no pending-reboot flag, so the running kernel is compared
// against the installed one: uname -r reports what booted, freebsd-version -k
// what is on disk, and they diverge exactly when an update awaits a reboot.
var probeScript = fmt.Sprintf(`if [ "$(uname -s)" = FreeBSD ]; then
    running=$(uname -r)
    installed=$(freebsd-version -k)
    if [ "$running" != "$installed" ]; then
        echo "NEEDED kernel $running running, $installed installed"
    else
        echo "OK kernel $running is current"
    fi
elif [ -e %[1]s ]; then
    pkgs=$(tr '\n' ' ' < %[2]s 2>/dev/null)
    if [ -n "$pkgs" ]; then
        echo "NEEDED packages awaiting restart: $pkgs"
    else
        echo "NEEDED %[1]s is present"
    fi
else
    echo "OK no pending reboot flag"
fi
`, rebootRequiredFlag, rebootRequiredPkgs)

// maxProbeConcurrency bounds how many hosts are probed at once.
const maxProbeConcurrency = 8

// RebootNeed is a host's answer to whether it is waiting on a reboot.
type RebootNeed int

const (
	// NeedUnknown means the host could not be probed. It is deliberately
	// distinct from NeedNo: an unprobed host must never be quietly treated as
	// up to date.
	NeedUnknown RebootNeed = iota
	// NeedYes means a reboot is pending.
	NeedYes
	// NeedNo means the host is current.
	NeedNo
)

// RebootStatus is the outcome of probing one host.
type RebootStatus struct {
	// Host is the host name as given.
	Host string
	// Need is the verdict.
	Need RebootNeed
	// Reason explains the verdict.
	Reason string
}

// ProbeHost asks one host whether it is waiting on a reboot. Any failure to
// connect, run, or parse yields NeedUnknown rather than an error, so one
// unreachable host cannot abort a fleet-wide check.
func ProbeHost(ctx context.Context, runner Runner, host Host) RebootStatus {
	stdout, err := runner.Run(ctx, probeScript, "ssh", sshCommand(host, "/bin/sh")...)
	if err != nil {
		return RebootStatus{host.Name, NeedUnknown, fmt.Sprintf("probe failed: %v", err)}
	}

	verdict, reason, _ := strings.Cut(strings.TrimSpace(stdout), " ")
	switch verdict {
	case "NEEDED":
		return RebootStatus{host.Name, NeedYes, strings.TrimSpace(reason)}
	case "OK":
		return RebootStatus{host.Name, NeedNo, strings.TrimSpace(reason)}
	default:
		return RebootStatus{host.Name, NeedUnknown,
			fmt.Sprintf("unexpected probe output: %q", strings.TrimSpace(stdout))}
	}
}

// ProbeHosts probes several hosts concurrently, returning one status per host
// in the order given.
func ProbeHosts(ctx context.Context, runner Runner, hosts []Host) []RebootStatus {
	statuses := make([]RebootStatus, len(hosts))
	group := new(errgroup.Group)
	group.SetLimit(maxProbeConcurrency)
	for i, host := range hosts {
		group.Go(func() error {
			// Every failure mode is already folded into a status, so there is
			// no error to propagate: one unreachable host must not abort a
			// fleet-wide check.
			statuses[i] = ProbeHost(ctx, runner, host)
			return nil
		})
	}
	_ = group.Wait()
	return statuses
}

// PrintProbeReport writes one line per probed host explaining its verdict.
func PrintProbeReport(out io.Writer, statuses []RebootStatus) {
	pending := 0
	for _, status := range statuses {
		if status.Need == NeedYes {
			pending++
		}
	}
	report(out, "\n%d hosts checked, %d need a reboot:\n", len(statuses), pending)
	for _, status := range statuses {
		reportHost(out, status.Host, "%s", status.Reason)
	}
}
