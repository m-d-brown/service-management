package reboot

import (
	"context"
	"fmt"
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
	// Verifications holds one verdict per host actually rebooted, including
	// hosts rebooted by the tier hosting them rather than by a command of
	// their own — a credited reboot is still a reboot, and belongs in the
	// summary. Hosts a re-check dropped are absent, never having been
	// rebooted at all.
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
// it, its guests, and its dependents are back, and then verified to have
// actually restarted. A tier that fails verification does not stop the run:
// stopping half way would leave the fleet in a state no one asked for, so every
// tier is attempted and the summary is the record of what to retry.
//
// A tier also delivers reboots it was never asked for, to every host declared
// as running on one of its members. Those hosts are read before the tier goes
// down and again after it returns, and any that provably restarted is credited
// and dropped from the tier that meant to reboot it — the difference between
// declaring hosting and merely ordering, and worth an entire second outage per
// guest. Crediting requires proof, so it does not happen under
// --skip-boot-verification: a reboot is skipped because it demonstrably already
// happened, never because the topology said it should have.
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

	// Hosts already power-cycled by a tier that hosts them. Their own tier
	// skips them rather than rebooting a machine that restarted minutes ago.
	credited := map[string]bool{}

	for position, tier := range tiers {
		report(o.writer(), "\n=== Executing Tier: %d ===\n", position+1)

		tierNames := o.dropCredited(plan, tier, credited)
		if len(tierNames) == 0 {
			report(o.writer(), "Tier %d was rebooted by the tier hosting it; nothing to do.\n", position+1)
			continue
		}

		// Re-check every tier but the first. The tiers before this one have
		// rebooted, taking the hosts they carry down and back up with them,
		// which may already have applied what these hosts were queued for.
		// This runs before the baseline capture so a host dropped here is never
		// probed for a boot state it will not change, and never appears in the
		// summary as a host that failed to reboot — it was deliberately not
		// rebooted. Re-probing is safe at this moment because the preceding
		// tier already waited for its hosts and their dependents to answer.
		if o.Config.IfNeeded && position > 0 {
			var unprobed []RebootStatus
			statuses := ProbeHosts(ctx, o.Runner, plan.hostsFor(tierNames))
			tierNames, unprobed = o.partitionPending(statuses, true)
			result.Unprobed = append(result.Unprobed, unprobed...)
			if len(tierNames) == 0 {
				report(o.writer(), "Tier %d is already up to date; nothing to do.\n", position+1)
				continue
			}
		}

		tierHosts := plan.hostsFor(tierNames)

		// The hosts this tier will take down with it that a later tier still
		// means to reboot itself. Computed now because the evidence expires:
		// once the parent has gone down, the boot identity that would prove the
		// guest restarted has already been replaced by the one it came back on.
		carried := o.carriedTargets(plan, tierNames, tiers[position+1:], credited)

		// Record the baselines before anything powers a host down, so the
		// reboot cannot race the probe.
		var baselines map[string]*BootState
		if o.Config.VerifyBootState {
			baselines = map[string]*BootState{}
			o.captureBaselines(ctx, baselines, tierHosts, false)
			o.captureBaselines(ctx, baselines, carried, true)
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
		// still decides; the observed cycle only speaks where it cannot. The
		// carried hosts are settled first, so a host credited here is already
		// gone from the tier that would have rebooted it.
		if o.Config.VerifyBootState {
			result.Verifications = append(result.Verifications,
				o.creditCarried(ctx, carried, baselines, cycles, credited)...)
			result.Verifications = append(result.Verifications,
				o.verifyTier(ctx, tierHosts, baselines, cycles)...)
		}
	}

	o.printSummary(result.Verifications)
	return result, nil
}

// watchlist returns what the monitor should watch for a tier, in the three
// groups whose behaviour means three different things.
//
// The tier's own hosts were sent a reboot. Everything hosted by one of them,
// transitively, is going down with it whether or not it was targeted, so the
// run cannot move on until those have come back. Everything merely ordered
// after a member of the tier is watched too, because it might go down — its
// edge never said it would, which is exactly why it is kept apart from the
// hosts whose drop was promised.
//
// A host qualifying under more than one is placed in the strongest: a guest
// that something else also orders itself after is still a guest.
func (o *Orchestrator) watchlist(plan Plan, tierNames []string) Watchlist {
	targeted := map[string]bool{}
	for _, name := range tierNames {
		targeted[name] = true
	}

	carried := map[string]bool{}
	for _, name := range plan.Hosts.Carried(tierNames) {
		if !targeted[name] {
			carried[name] = true
		}
	}

	dependents := map[string]bool{}
	for _, name := range tierNames {
		for _, dependent := range plan.Hosts.Dependents(name) {
			if !targeted[dependent] && !carried[dependent] {
				dependents[dependent] = true
			}
		}
	}

	return Watchlist{
		Targets:    plan.hostsFor(slices.Sorted(maps.Keys(targeted))),
		Carried:    plan.hostsFor(slices.Sorted(maps.Keys(carried))),
		Dependents: plan.hostsFor(slices.Sorted(maps.Keys(dependents))),
	}
}

// dropCredited removes the hosts a previous tier's reboot already delivered.
//
// A guest whose hypervisor restarted two minutes ago has had precisely the
// reboot this tier was going to give it, and the changed boot identity to prove
// it. Doing it again applies nothing and costs the fleet a second outage. That
// saving is the whole practical difference between hosting declared as hosting
// and hosting written as a bare ordering, where the second reboot is
// unavoidable because nothing ever claimed the first one happened.
func (o *Orchestrator) dropCredited(plan Plan, names []string, credited map[string]bool) []string {
	if len(credited) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if credited[name] {
			reportHost(o.writer(), name, "skipping — already rebooted with %s", plan.Hosts[name].RunsOn)
			continue
		}
		out = append(out, name)
	}
	return out
}

// carriedTargets returns the hosts this tier will take down that a later tier
// still means to reboot in its own right.
//
// Those are the only ones whose free reboot is worth proving. A baseline costs
// a connection per host, and a hypervisor's guests are where a fleet keeps its
// largest counts, so the run spends that connection only where the answer can
// save a whole reboot. Every other carried host is watched all the same — the
// tier is not finished until it is back — but nothing is asked of it.
func (o *Orchestrator) carriedTargets(
	plan Plan, tierNames []string, remaining [][]string, credited map[string]bool,
) []Host {
	later := map[string]bool{}
	for _, tier := range remaining {
		for _, name := range tier {
			if !credited[name] {
				later[name] = true
			}
		}
	}

	var names []string
	for _, name := range plan.Hosts.Carried(tierNames) {
		if later[name] {
			names = append(names, name)
		}
	}
	return plan.hostsFor(names)
}

// creditCarried decides which of the hosts this tier took down actually
// restarted, and credits those with a reboot they no longer need.
//
// The evidence is exactly what a host of the tier's own is judged by: the boot
// identity read before the parent went down against the one read now, with the
// observed power cycle breaking ties where the host exposes no marker. Nothing
// is taken on the strength of the declaration itself — runs-on decided which
// hosts were worth asking, and each host then answers for itself. A hypervisor
// that quietly migrated a guest away cannot cause that guest's reboot to be
// skipped.
//
// A host that cannot be shown to have restarted is left alone rather than
// counted as a failure. Nothing was asked of it, so there is nothing it failed
// to do: it keeps its place in a later tier and is rebooted there, which is
// what would have happened had hosting never been declared at all.
func (o *Orchestrator) creditCarried(ctx context.Context, hosts []Host,
	baselines map[string]*BootState, cycles map[string]Cycle, credited map[string]bool,
) []RebootVerification {
	if len(hosts) == 0 {
		return nil
	}
	report(o.writer(), "Checking the hosts this tier carried down...\n")

	var results []RebootVerification
	for _, host := range hosts {
		after := CaptureBootState(ctx, o.writer(), o.Runner, o.Clock, host, o.Config.ProbeTimeout)
		result := VerifyReboot(host.Name, baselines[host.Name], after, cycles[host.Name])
		if result.Status != StatusConfirmed {
			reportHost(o.writer(), host.Name,
				"not credited, so it keeps its own tier: %s", result.Detail)
			continue
		}
		credited[host.Name] = true
		result.Detail = fmt.Sprintf("rebooted with %s: %s", host.RunsOn, result.Detail)
		reportHost(o.writer(), host.Name, "[✓] %s", result.Detail)
		results = append(results, result)
	}
	return results
}

// captureBaselines records the pre-reboot boot identity of each host into the
// map the tier will later verify against.
//
// carried says which set these are: the tier's own hosts, about to be sent a
// reboot, or the hosts underneath them, about to receive one whether anyone
// asks or not. Both readings have to be taken before anything powers down, and
// they differ only in what failing to take one costs.
func (o *Orchestrator) captureBaselines(
	ctx context.Context, into map[string]*BootState, hosts []Host, carried bool,
) {
	if len(hosts) == 0 {
		return
	}
	if carried {
		report(o.writer(), "Recording boot state of the hosts this tier will carry down...\n")
	} else {
		report(o.writer(), "Recording pre-reboot boot state...\n")
	}

	for _, host := range hosts {
		state := CaptureBootState(ctx, o.writer(), o.Runner, o.Clock, host, o.Config.ProbeTimeout)
		if state == nil {
			if carried {
				reportHost(o.writer(), host.Name, "WARNING: cannot read the boot state over SSH, so a "+
					"reboot carried from its host cannot be credited; it will be rebooted in its own tier.")
			} else {
				reportHost(o.writer(), host.Name, "WARNING: cannot read the boot state over SSH. The reboot "+
					"command will likely fail the same way, and the reboot cannot be verified.")
			}
		}
		into[host.Name] = state
	}
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
