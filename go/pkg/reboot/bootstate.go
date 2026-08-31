package reboot

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// bootProbeCommand reads the two independent boot markers the kernel exposes:
//
//   - /proc/sys/kernel/random/boot_id, regenerated on every boot, and so
//     authoritative.
//   - /proc/uptime, the fallback for hosts exposing no boot ID at all —
//     busybox-based firmware, appliances, network gear.
//
// Both are emitted as key=value lines and left empty when unreadable, so a host
// missing one marker still reports the other. The command deliberately contains
// no single quotes, keeping it readable when echoed back to the operator.
const bootProbeCommand = `printf "boot_id=%s\n" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"; ` +
	`printf "uptime=%s\n" "$(cut -d" " -f1 /proc/uptime 2>/dev/null)"`

// uptimeTolerance absorbs clock skew and the gap between issuing a probe and
// the host answering it when comparing uptime against elapsed wall time.
const uptimeTolerance = 5 * time.Second

// VerificationStatus is the outcome of comparing a host's boot identity before
// and after a reboot.
type VerificationStatus int

const (
	// StatusConfirmed means the host provably restarted.
	StatusConfirmed VerificationStatus = iota
	// StatusNotRebooted means the host provably did not restart.
	StatusNotRebooted
	// StatusUnknown means the evidence does not settle it either way.
	StatusUnknown
)

// String renders the status for display.
func (s VerificationStatus) String() string {
	switch s {
	case StatusConfirmed:
		return "confirmed"
	case StatusNotRebooted:
		return "not-rebooted"
	default:
		return "unknown"
	}
}

// BootState is a point-in-time reading of a host's boot identity.
type BootState struct {
	// BootID is the kernel boot UUID, empty when the host exposes none.
	BootID string
	// Uptime is the time since boot; zero when unreadable, which HasUptime
	// distinguishes from a genuine reading.
	Uptime time.Duration
	// HasUptime reports whether Uptime was actually read.
	HasUptime bool
	// CapturedAt is when the reading was taken, used to measure the wall-clock
	// window a reboot had to happen in.
	CapturedAt time.Time
}

// isEmpty reports whether the host answered but exposed no usable marker.
func (b BootState) isEmpty() bool {
	return b.BootID == "" && !b.HasUptime
}

// RebootVerification is the verdict for a single host.
type RebootVerification struct {
	// Host is the host the verdict applies to.
	Host string
	// Status is whether the reboot was confirmed, disproved, or indeterminate.
	Status VerificationStatus
	// Detail is the human-readable evidence behind the verdict.
	Detail string
}

// parseBootProbe parses the key=value output of bootProbeCommand.
func parseBootProbe(output string, capturedAt time.Time) BootState {
	state := BootState{CapturedAt: capturedAt}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "boot_id":
			state.BootID = value
		case "uptime":
			seconds, err := strconv.ParseFloat(value, 64)
			if err != nil {
				continue
			}
			state.Uptime = time.Duration(seconds * float64(time.Second))
			state.HasUptime = true
		}
	}
	return state
}

// CaptureBootState reads a host's boot identity over SSH.
//
// This doubles as an SSH pre-flight check: a host that cannot be probed almost
// certainly cannot be rebooted over SSH either. A nil return means the host
// could not be read at all, which is deliberately distinct from a reading that
// contained no markers.
func CaptureBootState(
	ctx context.Context,
	out io.Writer,
	runner Runner,
	clock Clock,
	host Host,
	timeout time.Duration,
) *BootState {
	// The echoed command is the whole announcement: it names the host, and the
	// printf it carries says plainly what is being read.
	args := sshCommand(host, bootProbeCommand)
	reportHost(out, host.Name, "$ %s", FormatCommand("ssh", args...))

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, err := runner.Run(ctx, "", "ssh", args...)
	if err != nil {
		reportHost(out, host.Name, "WARNING: boot state probe failed: %v", err)
		return nil
	}

	state := parseBootProbe(stdout, clock.Now())
	if state.isEmpty() {
		reportHost(out, host.Name, "WARNING: exposed neither /proc/sys/kernel/random/boot_id "+
			"nor /proc/uptime; its reboot cannot be verified.")
	}
	return &state
}

// VerifyReboot decides whether a host restarted, from its boot markers first
// and the monitor's observation second.
//
// Boot markers stay authoritative: they are read from the host itself and say
// what happened rather than what was seen from outside. The observed power
// cycle only breaks ties the markers leave open — which is most valuable on
// hosts that expose no marker at all, whose reboots were previously
// unverifiable in either direction.
func VerifyReboot(host string, before, after *BootState, cycle Cycle) RebootVerification {
	verdict := verifyByMarkers(host, before, after)
	if verdict.Status != StatusUnknown {
		return verdict
	}
	switch {
	case cycle.Complete():
		return RebootVerification{host, StatusConfirmed, fmt.Sprintf(
			"%s, but it was seen to go down at %s and come back %s later",
			verdict.Detail, stamp(cycle.DownAt), formatUptime(cycle.DownFor()))}
	case cycle.StayedUp():
		return RebootVerification{host, StatusNotRebooted, fmt.Sprintf(
			"%s, and it answered every probe throughout; it never went down", verdict.Detail)}
	}
	return verdict
}

// verifyByMarkers compares boot identities recorded before and after a reboot.
//
// A changed boot ID is definitive. Failing that, uptime decides: a host that
// restarted must report an uptime lower than its previous one, or one no larger
// than the window between the two readings.
func verifyByMarkers(host string, before, after *BootState) RebootVerification {
	switch {
	case before == nil && after == nil:
		return RebootVerification{host, StatusUnknown,
			"boot state could not be read before or after the reboot"}
	case before == nil:
		return RebootVerification{host, StatusUnknown,
			"no baseline boot state was recorded before the reboot"}
	case after == nil:
		return RebootVerification{host, StatusUnknown,
			"host answered ping but its boot state could not be read afterwards"}
	}

	if before.BootID != "" && after.BootID != "" {
		if before.BootID != after.BootID {
			return RebootVerification{host, StatusConfirmed,
				fmt.Sprintf("boot_id changed (%s -> %s)", before.BootID, after.BootID)}
		}
		return RebootVerification{host, StatusNotRebooted,
			fmt.Sprintf("boot_id is unchanged (%s); the host never went down", before.BootID)}
	}

	if before.HasUptime && after.HasUptime {
		elapsed := after.CapturedAt.Sub(before.CapturedAt)
		beforeStr, afterStr := formatUptime(before.Uptime), formatUptime(after.Uptime)
		if after.Uptime < before.Uptime || after.Uptime <= elapsed+uptimeTolerance {
			return RebootVerification{host, StatusConfirmed,
				fmt.Sprintf("uptime reset (%s -> %s)", beforeStr, afterStr)}
		}
		return RebootVerification{host, StatusNotRebooted,
			fmt.Sprintf("uptime kept climbing (%s -> %s) over a %s window; the host never went down",
				beforeStr, afterStr, formatUptime(elapsed))}
	}

	return RebootVerification{host, StatusUnknown,
		"host exposes no boot_id or uptime marker to compare"}
}

// formatUptime renders a duration as a compact human-readable string.
func formatUptime(d time.Duration) string {
	total := int(d.Seconds())
	if total < 0 {
		total = 0
	}
	days, rem := total/86400, total%86400
	hours, rem := rem/3600, rem%3600
	minutes, seconds := rem/60, rem%60

	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return strings.Join(parts, " ")
}
