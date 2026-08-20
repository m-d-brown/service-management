package reboot

import (
	"context"
	"math"
	"strconv"
	"time"
)

// pingGrace is the slack added to a ping's own deadline before the context
// gives up on it, so a ping binary that ignores -W cannot stall a sweep.
const pingGrace = 2 * time.Second

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
	ctx, cancel := context.WithTimeout(ctx, timeout+pingGrace)
	defer cancel()
	_, err := runner.Run(ctx, "", "ping", pingArgs(host.Target(), timeout)...)
	return err == nil
}
