package reboot

import (
	"context"
	"io"
	"maps"
	"slices"
	"sync"
	"time"
)

// Config tunes an orchestration run.
type Config struct {
	// PingTimeout bounds a single ICMP echo.
	PingTimeout time.Duration
	// DropWait is the drop wait: how long a host that never stops answering is
	// waited for, from the moment a tier's commands are away, before the run
	// accepts that it is not going to drop.
	DropWait time.Duration
	// SampleInterval is how often the monitor probes each host while it is
	// rebooting. It sets the resolution of the drop: a host gone for less than
	// this may pass unseen.
	SampleInterval time.Duration
	// ProbeTimeout bounds a single SSH boot state probe.
	ProbeTimeout time.Duration
	// VerifyBootState reads boot identity before and after each tier to prove
	// hosts actually restarted.
	VerifyBootState bool
	// IfNeeded reboots only the hosts reporting a pending reboot, re-checking
	// each tier as it comes up.
	IfNeeded bool
}

// defaultSampleInterval is how often the monitor samples when a caller has not
// said. It is short enough to catch a fast guest's whole power cycle and long
// enough that a large tier is not flooded with probes.
const defaultSampleInterval = time.Second

// sampleInterval is the configured sampling period, or a sane default. A ticker
// cannot be built from a zero or negative period, so this is the difference
// between a working default and a panic for a caller who left Config alone.
func (c Config) sampleInterval() time.Duration {
	if c.SampleInterval <= 0 {
		return defaultSampleInterval
	}
	return c.SampleInterval
}

// Orchestrator runs tiered reboots.
type Orchestrator struct {
	// Config tunes the run.
	Config Config
	// Runner executes the ssh and ping commands.
	Runner Runner
	// Clock supplies waits and timestamps.
	Clock Clock
	// Out receives progress output.
	Out io.Writer

	// outOnce guards the lazy creation of the shared, serialised writer.
	outOnce sync.Once
	// out is Out behind a lock, shared with the monitor goroutine.
	out io.Writer
}

// writer returns Out wrapped so the run and the monitor can both narrate to it
// without their lines colliding.
func (o *Orchestrator) writer() io.Writer {
	o.outOnce.Do(func() { o.out = &syncWriter{w: o.Out} })
	return o.out
}

// Result is what a run produced.
type Result struct {
	// Verifications holds one verdict per host actually rebooted. Hosts a
	// re-check dropped are absent, never having been rebooted.
	Verifications []RebootVerification
	// Unprobed holds hosts that could not be checked for a pending reboot.
	// They are excluded from the reboot set — an unprobed host is not known to
	// need one — but they make a run unsound, so a caller can fail on them.
	Unprobed []RebootStatus
}

// NotRebooted returns the hosts proven not to have rebooted.
func (r Result) NotRebooted() []RebootVerification {
	var out []RebootVerification
	for _, v := range r.Verifications {
		if v.Status == StatusNotRebooted {
			out = append(out, v)
		}
	}
	return out
}

// SelectPending narrows a plan to the hosts actually waiting on a reboot.
//
// Called before the run so the operator confirms the real work, it returns the
// narrowed plan and the hosts that could not be probed. With IfNeeded unset the
// plan passes through untouched.
func (o *Orchestrator) SelectPending(ctx context.Context, plan Plan) (Plan, []RebootStatus) {
	if !o.Config.IfNeeded {
		return plan, nil
	}

	statuses := ProbeHosts(ctx, o.Runner, plan.hostsFor(sortedTargets(plan.Targets)))
	PrintProbeReport(o.writer(), statuses)

	pending, unprobed := o.partitionPending(statuses, false)
	plan.Targets = pending
	return plan, unprobed
}

// partitionPending splits probe results into the hosts still waiting on a
// reboot and the hosts that could not be probed at all.
//
// A per-tier re-check announces each host it drops, because those hosts were
// approved for reboot and are now being skipped. An up-front narrowing does
// not: it has already printed a full report of the very same statuses, and
// saying it twice buries the part the operator has to read.
func (o *Orchestrator) partitionPending(statuses []RebootStatus, announceSkips bool) ([]string, []RebootStatus) {
	var (
		pending  []string
		unprobed []RebootStatus
	)
	for _, status := range statuses {
		switch status.Need {
		case NeedYes:
			pending = append(pending, status.Host)
		case NeedUnknown:
			unprobed = append(unprobed, status)
			if announceSkips {
				reportHost(o.writer(), status.Host, "skipping — %s", status.Reason)
			}
		case NeedNo:
			if announceSkips {
				reportHost(o.writer(), status.Host, "skipping — no longer needs a reboot")
			}
		}
	}
	return pending, unprobed
}

// Run executes the tiered reboot.
//
// Tiers run in dependency order; each is rebooted in parallel, waited on until
// it and its dependents answer ping, and then verified to have actually
// restarted. A tier that fails verification does not stop the run: stopping
// half way would leave the fleet in a state no one asked for, so every tier is
// attempted and the summary is the record of what to retry.
func (o *Orchestrator) Run(ctx context.Context, plan Plan) (Result, error) {
	var result Result
	if len(plan.Targets) == 0 {
		report(o.writer(), "No hosts targeted for reboot.\n")
		return result, nil
	}

	tiers, err := BuildTiers(plan.Hosts, plan.Targets)
	if err != nil {
		return result, err
	}

	for position, tier := range tiers {
		report(o.writer(), "\n=== Executing Tier: %d ===\n", position+1)

		tierNames := tier
		// Re-check every tier but the first. The tiers before this one have
		// rebooted, taking their nested dependents down and back up with them,
		// which may already have applied what these hosts were queued for.
		// This runs before the baseline capture so a host dropped here is never
		// probed for a boot state it will not change, and never appears in the
		// summary as a host that failed to reboot — it was deliberately not
		// rebooted. Re-probing is safe at this moment because the preceding
		// tier already waited for its hosts and their dependents to answer.
		if o.Config.IfNeeded && position > 0 {
			var unprobed []RebootStatus
			statuses := ProbeHosts(ctx, o.Runner, plan.hostsFor(tier))
			tierNames, unprobed = o.partitionPending(statuses, true)
			result.Unprobed = append(result.Unprobed, unprobed...)
			if len(tierNames) == 0 {
				report(o.writer(), "Tier %d is already up to date; nothing to do.\n", position+1)
				continue
			}
		}

		tierHosts := plan.hostsFor(tierNames)

		// Record the baseline before anything powers a host down, so the reboot
		// cannot race the probe.
		var baselines map[string]*BootState
		if o.Config.VerifyBootState {
			baselines = o.captureBaselines(ctx, tierHosts)
		}

		// Start watching before anything powers a host down: a fast host can be
		// gone and back before a first look would have happened, and a drop is
		// only evidence if something was already looking when it happened.
		monitor := StartMonitor(ctx, o.writer(), o.Runner, o.Clock, o.watchlist(plan, tierNames),
			o.Config.sampleInterval(), o.Config.PingTimeout, o.Config.ProbeTimeout, o.Config.DropWait)

		RebootHosts(o.writer(), o.Runner, tierHosts)

		// Only now can a host still answering mean anything: every command that
		// should take one down is away.
		monitor.StartDropWait()

		waitErr := monitor.WaitForReturn(ctx)
		monitor.Stop()
		cycles := monitor.Cycles()
		if waitErr != nil {
			return result, waitErr
		}

		// Coming back is not the same as having restarted, so the boot identity
		// still decides; the observed cycle only speaks where it cannot.
		if o.Config.VerifyBootState {
			result.Verifications = append(result.Verifications,
				o.verifyTier(ctx, tierHosts, baselines, cycles)...)
		}
	}

	o.printSummary(result.Verifications)
	return result, nil
}

// watchlist returns what the monitor should watch for a tier: the tier itself,
// plus every host that sits behind one of its members. A dependent may go down
// with its parent whether or not it was targeted, so the run cannot move on
// until it has come back — but it is kept apart from the tier, because only a
// host that was sent a reboot can be judged by whether it went down.
func (o *Orchestrator) watchlist(plan Plan, tierNames []string) Watchlist {
	targeted := map[string]bool{}
	for _, name := range tierNames {
		targeted[name] = true
	}
	dependents := map[string]bool{}
	for _, name := range tierNames {
		for _, dependent := range plan.Hosts.Dependents(name) {
			if !targeted[dependent] {
				dependents[dependent] = true
			}
		}
	}
	return Watchlist{
		Targets:    plan.hostsFor(slices.Sorted(maps.Keys(targeted))),
		Dependents: plan.hostsFor(slices.Sorted(maps.Keys(dependents))),
	}
}

// captureBaselines records the pre-reboot boot identity of each host in a tier.
func (o *Orchestrator) captureBaselines(ctx context.Context, hosts []Host) map[string]*BootState {
	report(o.writer(), "Recording pre-reboot boot state...\n")
	baselines := make(map[string]*BootState, len(hosts))
	for _, host := range hosts {
		state := CaptureBootState(ctx, o.writer(), o.Runner, o.Clock, host, o.Config.ProbeTimeout)
		if state == nil {
			reportHost(o.writer(), host.Name, "WARNING: cannot read the boot state over SSH. The reboot "+
				"command will likely fail the same way, and the reboot cannot be verified.")
		}
		baselines[host.Name] = state
	}
	return baselines
}

// verifyTier re-reads each host's boot identity and compares it to the baseline.
func (o *Orchestrator) verifyTier(ctx context.Context, hosts []Host,
	baselines map[string]*BootState, cycles map[string]Cycle) []RebootVerification {
	report(o.writer(), "Verifying boot state changed...\n")
	results := make([]RebootVerification, 0, len(hosts))
	for _, host := range hosts {
		after := CaptureBootState(ctx, o.writer(), o.Runner, o.Clock, host, o.Config.ProbeTimeout)
		result := VerifyReboot(host.Name, baselines[host.Name], after, cycles[host.Name])
		switch result.Status {
		case StatusConfirmed:
			reportHost(o.writer(), host.Name, "[✓] rebooted: %s", result.Detail)
		case StatusNotRebooted:
			reportHost(o.writer(), host.Name, "[✗] WARNING: did NOT reboot: %s", result.Detail)
		case StatusUnknown:
			reportHost(o.writer(), host.Name, "[?] WARNING: reboot unverified: %s", result.Detail)
		}
		results = append(results, result)
	}
	return results
}

// printSummary writes a closing report grouping hosts by outcome.
func (o *Orchestrator) printSummary(results []RebootVerification) {
	if len(results) == 0 {
		report(o.writer(), "\nAll tiers complete. Reboot orchestration finished successfully.\n")
		return
	}

	var confirmed, failed, unknown []RebootVerification
	for _, result := range results {
		switch result.Status {
		case StatusConfirmed:
			confirmed = append(confirmed, result)
		case StatusNotRebooted:
			failed = append(failed, result)
		case StatusUnknown:
			unknown = append(unknown, result)
		}
	}

	report(o.writer(), "\n=== Reboot Verification Summary ===\n")
	report(o.writer(), "Confirmed rebooted: %d  Not rebooted: %d  Unverified: %d\n",
		len(confirmed), len(failed), len(unknown))
	for _, result := range failed {
		reportHost(o.writer(), result.Host, "[✗] %s", result.Detail)
	}
	for _, result := range unknown {
		reportHost(o.writer(), result.Host, "[?] %s", result.Detail)
	}

	if unconfirmed := len(failed) + len(unknown); unconfirmed > 0 {
		report(o.writer(), "\nAll tiers complete, but %d host(s) could not be confirmed as rebooted.\n", unconfirmed)
		return
	}
	report(o.writer(), "\nAll tiers complete. Reboot orchestration finished successfully.\n")
}

// hostsFor resolves names to their definitions, preserving order.
func (p Plan) hostsFor(names []string) []Host {
	hosts := make([]Host, 0, len(names))
	for _, name := range names {
		hosts = append(hosts, p.Hosts[name])
	}
	return hosts
}
