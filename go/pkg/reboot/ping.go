package reboot

import (
	"context"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// pingPollInterval is how long to wait between sweeps of the hosts that have
// not yet answered.
const pingPollInterval = 2 * time.Second

// pingArgs builds the ICMP reachability command for one address.
func pingArgs(addr string, timeout time.Duration) []string {
	// -W takes whole seconds and rejects zero, so a sub-second timeout rounds
	// up rather than silently becoming "no timeout".
	seconds := int(math.Ceil(timeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return []string{"-c", "1", "-W", strconv.Itoa(seconds), addr}
}

// PingHost reports whether a host answers a single ICMP echo.
func PingHost(ctx context.Context, runner Runner, host Host, timeout time.Duration) bool {
	// The command carries its own deadline; the context guards against a ping
	// binary that ignores it.
	ctx, cancel := context.WithTimeout(ctx, timeout+pingPollInterval)
	defer cancel()
	_, err := runner.Run(ctx, "", "ping", pingArgs(host.Target(), timeout)...)
	return err == nil
}

// WaitForHosts blocks until every host answers ping.
//
// It first sleeps for dropWait. Without that delay a host that has been told to
// reboot but has not yet stopped forwarding packets answers immediately, and
// the run moves on to a dependent tier while its parent is still on its way
// down.
func WaitForHosts(
	ctx context.Context,
	out io.Writer,
	runner Runner,
	clock Clock,
	hosts []Host,
	dropWait, pingTimeout time.Duration,
) error {
	if len(hosts) == 0 {
		return nil
	}

	report(out, "Waiting %s for hosts to drop off the network...\n", dropWait)
	clock.Sleep(dropWait)

	pending := slices.SortedFunc(slices.Values(hosts),
		func(a, b Host) int { return strings.Compare(a.Name, b.Name) })

	report(out, "Waiting for %s to return online...\n", joinNames(pending))
	// Announce the polling command once rather than on every sweep.
	for _, host := range pending {
		report(out, "  $ %s\n", FormatCommand("ping", pingArgs(host.Target(), pingTimeout)...))
	}

	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var still []Host
		for _, host := range pending {
			if PingHost(ctx, runner, host, pingTimeout) {
				// Reachable is not the same as rebooted; boot state
				// verification decides whether the host actually restarted.
				report(out, "[✓] %s is reachable.\n", host.Name)
				continue
			}
			still = append(still, host)
		}
		pending = still
		if len(pending) > 0 {
			clock.Sleep(pingPollInterval)
		}
	}
	return nil
}
